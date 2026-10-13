package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// The composed service is exercised over a real loopback HTTP transport, the
// same way deployment health checks reach it.

func TestServeReportsSanitizedHealthWithoutDatabase(t *testing.T) {
	service := startService(t)

	response, err := service.client.Get(service.baseURL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read health body: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.StatusCode, http.StatusServiceUnavailable, body)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("content type = %q, want application/problem+json", contentType)
	}

	var decoded problem
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, body)
	}
	if decoded.Type != "/problems/service_unavailable" {
		t.Errorf("type = %q, want /problems/service_unavailable", decoded.Type)
	}
	if decoded.Title != "Service unavailable" {
		t.Errorf("title = %q, want Service unavailable", decoded.Title)
	}
	if decoded.Status != http.StatusServiceUnavailable {
		t.Errorf("status field = %d, want %d", decoded.Status, http.StatusServiceUnavailable)
	}
	if decoded.Code != string(protocol.ServiceUnavailable) {
		t.Errorf("code = %q, want %q", decoded.Code, protocol.ServiceUnavailable)
	}
	headerRequestID := response.Header.Get(observability.RequestIDHeader)
	if decoded.RequestID == "" || decoded.RequestID != headerRequestID {
		t.Errorf("requestId = %q, header = %q, want one shared identifier", decoded.RequestID, headerRequestID)
	}
	for _, leaked := range []string{"sql", "postgres", "password", "127.0.0.1"} {
		if strings.Contains(strings.ToLower(string(body)), leaked) {
			t.Errorf("health response leaks %q: %s", leaked, body)
		}
	}
}

func TestServeAnswersRoutingFallbacksWithProblems(t *testing.T) {
	service := startService(t)

	t.Run("unknown path", func(t *testing.T) {
		decoded := service.problem(t, http.MethodGet, "/missing", "")
		if decoded.Status != http.StatusNotFound || decoded.Code != string(protocol.ResourceNotFound) {
			t.Fatalf("problem = %+v, want 404 resource_not_found", decoded)
		}
	})

	t.Run("unsupported method", func(t *testing.T) {
		response, err := service.do(t, http.MethodPost, "/health", "")
		if err != nil {
			t.Fatalf("POST /health: %v", err)
		}
		defer func() { _ = response.Body.Close() }()

		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
		}
		if allow := response.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Fatalf("Allow = %q, want it to include GET", allow)
		}
		var decoded problem
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
		if decoded.Code != string(protocol.MethodNotAllowed) {
			t.Fatalf("code = %q, want %q", decoded.Code, protocol.MethodNotAllowed)
		}
	})

	t.Run("v1 responses are not cacheable", func(t *testing.T) {
		response, err := service.do(t, http.MethodGet, "/v1/messages/example", "")
		if err != nil {
			t.Fatalf("GET /v1/messages/example: %v", err)
		}
		defer func() { _ = response.Body.Close() }()

		if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", cacheControl)
		}
	})

	// The published contract travels as an offline release artifact, so the
	// process must not re-expose Huma's schema or documentation routes.
	t.Run("contract routes are not served", func(t *testing.T) {
		for _, path := range []string{"/openapi.json", "/docs", "/schemas/Problem"} {
			decoded := service.problem(t, http.MethodGet, path, "")
			if decoded.Status != http.StatusNotFound || decoded.Code != string(protocol.ResourceNotFound) {
				t.Fatalf("%s problem = %+v, want 404 resource_not_found", path, decoded)
			}
		}
	})
}

func TestServeStopsWithinItsShutdownWindow(t *testing.T) {
	service := startService(t)

	response, err := service.client.Get(service.baseURL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_ = response.Body.Close()

	if err := service.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if connection, err := net.DialTimeout("tcp", strings.TrimPrefix(service.baseURL, "http://"), time.Second); err == nil {
		_ = connection.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}
}

type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	RequestID string `json:"requestId"`
}

type testService struct {
	baseURL string
	client  *http.Client
	cancel  context.CancelFunc
	served  <-chan error
	stopErr error
	stopped bool
}

func startService(t *testing.T) *testService {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- api.Serve(ctx, listener, api.Dependencies{Logger: discardLogger()})
	}()

	service := &testService{
		baseURL: "http://" + listener.Addr().String(),
		client:  &http.Client{Timeout: 5 * time.Second},
		cancel:  cancel,
		served:  served,
	}
	t.Cleanup(func() {
		if err := service.stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return service
}

// stop joins the serving goroutine through the real shutdown path. It is safe
// to call explicitly and again from test cleanup.
func (s *testService) stop() error {
	if s.stopped {
		return s.stopErr
	}
	s.stopped = true
	s.cancel()
	select {
	case s.stopErr = <-s.served:
	case <-time.After(5 * time.Second):
		s.stopErr = errors.New("Serve did not return within its shutdown window")
	}
	return s.stopErr
}

func (s *testService) do(t *testing.T, method string, path string, accept string) (*http.Response, error) {
	t.Helper()

	request, err := http.NewRequest(method, s.baseURL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	return s.client.Do(request)
}

func (s *testService) problem(t *testing.T, method string, path string, accept string) problem {
	t.Helper()

	response, err := s.do(t, method, path, accept)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("%s %s content type = %q, want application/problem+json", method, path, contentType)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read problem body: %v", err)
	}
	var decoded problem
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, body)
	}
	if decoded.Status != response.StatusCode {
		t.Fatalf("%s %s status field = %d, response status = %d", method, path, decoded.Status, response.StatusCode)
	}
	return decoded
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
