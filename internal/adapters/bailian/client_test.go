package bailian

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/model"
)

// The adapter is the only place the provider protocol exists: one request must
// carry the adopted lifetime to the configured workspace, and every answer
// that is not a usable temporary key must become the provider outcome without
// repeating the parent key or the provider's own body.
func TestTemporaryKeyMapsProviderAnswersToOutcomes(t *testing.T) {
	// The provider states an expiry of its own, which can differ from the
	// requested lifetime and must not be replaced with it.
	actualExpiry := time.Now().UTC().Add(20 * time.Minute).Unix()

	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    error
		wantKey    string
		wantExpiry time.Time
	}{
		{
			name:       "issued key with the provider's actual expiry",
			status:     http.StatusOK,
			body:       fmt.Sprintf(`{"token":"st-issued","expires_at":%d}`, actualExpiry),
			wantKey:    "st-issued",
			wantExpiry: time.Unix(actualExpiry, 0).UTC(),
		},
		{
			name:    "provider rejects the parent key",
			status:  http.StatusUnauthorized,
			body:    `{"code":"InvalidApiKey","message":"Invalid API-key provided.","request_id":"req-one"}`,
			wantErr: model.ErrProvider,
		},
		{
			name:    "provider failure",
			status:  http.StatusInternalServerError,
			body:    `upstream error text`,
			wantErr: model.ErrProvider,
		},
		{
			name:    "answer that is not JSON",
			status:  http.StatusOK,
			body:    `not a provider answer`,
			wantErr: model.ErrProvider,
		},
		{
			name:    "answer without a key",
			status:  http.StatusOK,
			body:    fmt.Sprintf(`{"expires_at":%d}`, actualExpiry),
			wantErr: model.ErrProvider,
		},
		{
			name:    "answer without an expiry",
			status:  http.StatusOK,
			body:    `{"token":"st-issued"}`,
			wantErr: model.ErrProvider,
		},
		{
			name:    "answer whose key is already expired",
			status:  http.StatusOK,
			body:    `{"token":"st-issued","expires_at":1}`,
			wantErr: model.ErrProvider,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var received *http.Request
			provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				received = request
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer provider.Close()

			client := New("sk-parent")
			// The workspace host comes from the domain; the test substitutes the
			// transport so the request shape is observable without the network.
			client.endpoint = func(workspaceID string) string {
				return provider.URL + "/" + workspaceID + temporaryKeyPath
			}
			client.httpClient = provider.Client()

			credential, err := client.TemporaryKey(context.Background(), "llm-workspace")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("TemporaryKey error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				assertNoProviderSecret(t, err, "sk-parent", "st-issued")
				return
			}
			if credential.APIKey != test.wantKey || !credential.ExpiresAt.Equal(test.wantExpiry) {
				t.Fatalf("temporary key = %+v, want %q expiring %s", credential, test.wantKey, test.wantExpiry)
			}

			if received.Method != http.MethodPost {
				t.Errorf("provider method = %s, want POST", received.Method)
			}
			if want := "/llm-workspace" + temporaryKeyPath; received.URL.Path != want {
				t.Errorf("provider path = %s, want %s", received.URL.Path, want)
			}
			want := strconv.Itoa(int(model.TemporaryKeyLifetime.Seconds()))
			if lifetime := received.URL.Query().Get(temporaryKeyParameter); lifetime != want {
				t.Errorf("requested lifetime = %q, want %s seconds", lifetime, want)
			}
			if authorization := received.Header.Get("Authorization"); authorization != "Bearer sk-parent" {
				t.Errorf("provider authorization = %q, want the configured parent key", authorization)
			}
		})
	}
}

// A transport failure never reports the request URL, which the adapter builds
// from the configured workspace.
func TestTemporaryKeyTransportFailureDoesNotReportTheRequestURL(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := provider.URL
	provider.Close()

	client := New("sk-parent")
	client.endpoint = func(string) string { return unreachable + temporaryKeyPath }

	_, err := client.TemporaryKey(context.Background(), "llm-workspace")
	if !errors.Is(err, model.ErrProvider) {
		t.Fatalf("TemporaryKey error = %v, want %v", err, model.ErrProvider)
	}
	assertNoProviderSecret(t, err, "sk-parent", unreachable)
}

// A provider that does not answer must fail the operation inside the adapter's
// own deadline instead of holding the Sender's request open.
func TestTemporaryKeyFailsWhenTheProviderDoesNotAnswer(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer provider.Close()

	client := New("sk-parent")
	client.endpoint = func(string) string { return provider.URL + temporaryKeyPath }
	client.httpClient = provider.Client()
	client.httpClient.Timeout = 50 * time.Millisecond

	if _, err := client.TemporaryKey(context.Background(), "llm-workspace"); !errors.Is(err, model.ErrProvider) {
		t.Fatalf("TemporaryKey error = %v, want %v", err, model.ErrProvider)
	}
}

// A process without the protected parent key cannot issue anything, so it
// fails the provider boundary instead of calling Bailian with an empty key.
func TestTemporaryKeyWithoutParentKeyDoesNotCallTheProvider(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unconfigured client called the provider")
	}))
	defer provider.Close()

	client := New("")
	client.endpoint = func(string) string { return provider.URL + temporaryKeyPath }
	client.httpClient = provider.Client()

	if _, err := client.TemporaryKey(context.Background(), "llm-workspace"); !errors.Is(err, model.ErrProvider) {
		t.Fatalf("TemporaryKey error = %v, want %v", err, model.ErrProvider)
	}
}

// assertNoProviderSecret keeps the parent key, the issued key and the
// provider's own text out of every error the adapter reports.
func assertNoProviderSecret(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("want a provider failure, got no error")
	}

	message := err.Error()
	for _, value := range append(forbidden, "Invalid API-key provided.", "upstream error text") {
		if strings.Contains(message, value) {
			t.Errorf("provider error %q contains %q", message, value)
		}
	}
}
