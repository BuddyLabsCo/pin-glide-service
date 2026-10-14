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
	server, api := newFailureTestServer(t)

	t.Run("validation failure", func(t *testing.T) {
		status, contentType, body := requestJSON(t, server, http.MethodPost, "/test-echo", `{"name":"secret-rejected-value"}`)
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
		status, _, body := requestJSON(t, server, http.MethodPost, "/test-echo", `{"name":"bad"}`)
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
			status, _, body := requestJSON(t, server, http.MethodPost, "/test-echo", `{"name":"`+failure.payload+`"}`)
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

	// An authentication hook failure reports through the same model with its
	// status and code intact, and its text is as untrusted as any other internal
	// error, so it never becomes a client-visible validation item.
	t.Run("authentication hook failure", func(t *testing.T) {
		status, contentType, body := requestJSON(t, server, http.MethodPut, "/test-auth-hook", `{}`)
		if contentType != "application/problem+json" {
			t.Fatalf("content type = %q, want application/problem+json", contentType)
		}

		var decoded struct {
			Type      string            `json:"type"`
			Status    int               `json:"status"`
			Code      string            `json:"code"`
			RequestID string            `json:"requestId"`
			Errors    []json.RawMessage `json:"errors"`
		}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("decode problem: %v: %s", err, body)
		}
		if decoded.Code == "" || decoded.Status != status || decoded.Type != "/problems/"+decoded.Code {
			t.Errorf("problem = %+v, want code, type and status to describe response %d", decoded, status)
		}
		if decoded.RequestID == "" {
			t.Error("requestId is empty")
		}
		if strings.Contains(body, "secret provider response") {
			t.Errorf("hook failure response leaks internal text: %s", body)
		}
		if len(decoded.Errors) != 0 {
			t.Errorf("errors = %s, want none for a failure without a validation location", decoded.Errors)
		}
	})

	t.Run("documented error schema", func(t *testing.T) {
		document, err := json.Marshal(api.OpenAPI())
		if err != nil {
			t.Fatalf("marshal OpenAPI document: %v", err)
		}

		var decoded struct {
			Components struct {
				Schemas map[string]struct {
					Required   []string `json:"required"`
					Properties map[string]struct {
						Format string `json:"format"`
					} `json:"properties"`
				} `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(document, &decoded); err != nil {
			t.Fatalf("decode OpenAPI document: %v", err)
		}

		var found bool
		for _, schema := range decoded.Components.Schemas {
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
// failure modes a JSON route or authentication hook can produce, so the shared
// error model is exercised the way production routes will use it.
func newFailureTestServer(t *testing.T) (*httptest.Server, huma.API) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api := newAPI(mux, Dependencies{Logger: logger})
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
	huma.Register(api, huma.Operation{
		OperationID: "TestAuthHook",
		Method:      http.MethodPut,
		Path:        "/test-auth-hook",
	}, func(ctx context.Context, input *secretCredential) (*struct{}, error) {
		return nil, nil
	})

	server := httptest.NewServer(withRequestID(withRecovery(mux, logger)))
	t.Cleanup(server.Close)
	return server, api
}

// secretCredential is an authentication hook that fails the way a provider or
// storage call does: with plain error text the client must never receive.
type secretCredential struct{}

func (c *secretCredential) Resolve(ctx huma.Context) []error {
	return []error{errors.New("secret provider response")}
}

func requestJSON(t *testing.T, server *httptest.Server, method string, path string, body string) (int, string, string) {
	t.Helper()

	request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
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
