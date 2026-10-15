//go:build integration

package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

// The composed service reports successful health while a disposable PostgreSQL
// answers, sanitized unavailability once it stops, and shuts down cleanly.
func TestHealthReflectsPostgreSQLAvailability(t *testing.T) {
	postgres := harness.StartPostgres(t)
	// Health does not exercise the WeChat boundary, so this service is
	// composed without a provider.
	service := harness.StartService(t, postgres.DSN, nil, nil)

	assertHealth(t, service, http.StatusOK, "ok")

	if err := postgres.Stop(); err != nil {
		t.Fatalf("stop disposable PostgreSQL: %v", err)
	}
	assertHealthEventuallyUnavailable(t, service)

	if err := service.Stop(); err != nil {
		t.Fatalf("stop composed service: %v", err)
	}
}

func assertHealth(t *testing.T, service *harness.Service, status int, statusField string) {
	t.Helper()

	response := service.Get(t, "/health")
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read health body: %v", err)
	}
	if response.StatusCode != status {
		t.Fatalf("health status = %d, want %d: %s", response.StatusCode, status, body)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode health body: %v: %s", err, body)
	}
	if status == http.StatusOK {
		if len(payload) != 1 || payload["status"] != statusField {
			t.Fatalf("health body = %s, want exactly {\"status\":%q}", body, statusField)
		}
		return
	}
	for _, leaked := range []string{"sql", "postgres", "password", "127.0.0.1"} {
		if strings.Contains(strings.ToLower(string(body)), leaked) {
			t.Fatalf("unavailable health leaks %q: %s", leaked, body)
		}
	}
}

// PostgreSQL unavailability is observed through the public health contract
// only; the service keeps serving while the database is gone.
func assertHealthEventuallyUnavailable(t *testing.T, service *harness.Service) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		response := service.Get(t, "/health")
		_ = response.Body.Close()
		if response.StatusCode == http.StatusServiceUnavailable {
			assertHealth(t, service, http.StatusServiceUnavailable, "unavailable")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health still reports %d after the database stopped", response.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
