package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
)

// Handler failures reach clients as the same documented problem detail as
// routing fallbacks: one stable code, the actual status, validation locations
// and messages without rejected values, and the operation's request identifier.
func TestHandlerFailuresUseTheSharedProblemModel(t *testing.T) {
	server := newFailureTestServer(t)

	t.Run("validation failure", func(t *testing.T) {
		status, contentType, body := postJSON(t, server, `{"name":"secret-rejected-value"}`)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d: %s", status, http.StatusUnprocessableEntity, body)
		}
		if contentType != "application/problem+json" {
			t.Fatalf("content type = %q, want application/problem+json", contentType)
		}

		var decoded struct {
			Type      string `json:"type"`
			Status    int    `json:"status"`
			Code      string `json:"code"`
			RequestID string `json:"requestId"`
			Errors    []struct {
				Location string `json:"location"`
				Message  string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("decode problem: %v: %s", err, body)
		}
		if decoded.Type != "/problems/validation_failed" || decoded.Code != string(protocol.ValidationFailed) {
			t.Errorf("type = %q, code = %q, want validation_failed", decoded.Type, decoded.Code)
		}
		if len(decoded.Errors) != 1 || decoded.Errors[0].Location != "body.name" || decoded.Errors[0].Message == "" {
			t.Errorf("errors = %+v, want one body.name location with a message", decoded.Errors)
		}
		if decoded.RequestID == "" {
			t.Error("requestId is empty")
		}
		if strings.Contains(body, "secret-rejected-value") || strings.Contains(body, `"value"`) {
			t.Errorf("validation response echoes rejected input: %s", body)
		}
	})

	t.Run("typed handler failure", func(t *testing.T) {
		status, _, body := postJSON(t, server, `{"name":"bad"}`)
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", status, http.StatusBadRequest, body)
		}

		var decoded struct {
			Type      string `json:"type"`
			Detail    string `json:"detail"`
			Code      string `json:"code"`
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("decode problem: %v: %s", err, body)
		}
		if decoded.Code != string(protocol.InvalidRequest) || decoded.Detail != "Rejected by the test handler." {
			t.Errorf("problem = %+v, want the handler's invalid_request detail", decoded)
		}
		if decoded.RequestID == "" {
			t.Error("requestId is empty")
		}
	})

	for _, failure := range []struct {
		name    string
		payload string
	}{
		{name: "error", payload: "err"},
		{name: "panic", payload: "pan"},
	} {
		t.Run("unexpected "+failure.name, func(t *testing.T) {
			status, _, body := postJSON(t, server, `{"name":"`+failure.payload+`"}`)
			if status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d: %s", status, http.StatusInternalServerError, body)
			}

			var decoded struct {
				Type      string `json:"type"`
				Detail    string `json:"detail"`
				Code      string `json:"code"`
				RequestID string `json:"requestId"`
			}
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("decode problem: %v: %s", err, body)
			}
			if decoded.Code != string(protocol.InternalError) || decoded.Type != "/problems/internal_error" {
				t.Errorf("problem = %+v, want internal_error", decoded)
			}
			if decoded.Detail != "The service encountered an unexpected error." {
				t.Errorf("detail = %q, want the generic internal detail", decoded.Detail)
			}
			if decoded.RequestID == "" {
				t.Error("requestId is empty")
			}
			if strings.Contains(body, "secret internal detail") {
				t.Errorf("internal failure response leaks the cause: %s", body)
			}
		})
	}

	t.Run("documented error schema", func(t *testing.T) {
		response, err := server.Client().Get(server.URL + "/openapi.json")
		if err != nil {
			t.Fatalf("GET /openapi.json: %v", err)
		}
		defer func() { _ = response.Body.Close() }()

		var document struct {
			Components struct {
				Schemas map[string]struct {
					Required   []string `json:"required"`
					Properties map[string]struct {
						Format string `json:"format"`
					} `json:"properties"`
				} `json:"schemas"`
			} `json:"components"`
		}
		if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
			t.Fatalf("decode OpenAPI document: %v", err)
		}

		var found bool
		for _, schema := range document.Components.Schemas {
			if schema.Properties["type"].Format != "uri-reference" {
				continue
			}
			found = true
			for _, required := range []string{"type", "title", "status", "detail", "code", "requestId"} {
				if !contains(schema.Required, required) {
					t.Errorf("error schema does not require %q", required)
				}
			}
		}
		if !found {
			t.Error("OpenAPI document has no problem schema with a uri-reference type")
		}
	})
}

// newFailureTestServer builds the service API on a real listener and adds the
// handler failure modes a JSON route can produce, so the shared error model is
// exercised the way production routes will use it.
func newFailureTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api := newAPI(mux, nil, logger)
	huma.Register(api, huma.Operation{
		OperationID: "TestEcho",
		Method:      http.MethodPost,
		Path:        "/test-echo",
	}, func(ctx context.Context, input *struct {
		Body struct {
			Name string `json:"name" maxLength:"3" doc:"Name to echo"`
		}
	}) (*struct{}, error) {
		switch input.Body.Name {
		case "bad":
			return nil, NewProblem(ctx, protocol.InvalidRequest, "Rejected by the test handler.")
		case "err":
			return nil, errors.New("secret internal detail")
		case "pan":
			panic("secret internal detail")
		}
		return nil, nil
	})

	server := httptest.NewServer(withRequestID(withRecovery(mux, logger)))
	t.Cleanup(server.Close)
	return server
}

func postJSON(t *testing.T, server *httptest.Server, body string) (int, string, string) {
	t.Helper()

	response, err := server.Client().Post(server.URL+"/test-echo", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /test-echo: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, response.Header.Get("Content-Type"), string(payload)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
