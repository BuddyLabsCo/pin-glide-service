package wechat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// The adapter is the only place the provider protocol exists: it must accept a
// verified identity, separate a rejected authorization from a provider failure,
// and never put a code, secret or provider message into an error.
func TestVerifyMapsProviderAnswersToIdentityOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    error
		wantOpenID string
		wantUnion  string
	}{
		{
			name:       "verified identity",
			status:     http.StatusOK,
			body:       `{"access_token":"access-token-value","refresh_token":"refresh-token-value","openid":"openid-a","unionid":"union-a"}`,
			wantOpenID: "openid-a",
			wantUnion:  "union-a",
		},
		{
			name:       "verified identity without a unionid",
			status:     http.StatusOK,
			body:       `{"openid":"openid-b"}`,
			wantOpenID: "openid-b",
		},
		{
			name:    "unusable authorization code",
			status:  http.StatusOK,
			body:    `{"errcode":40029,"errmsg":"invalid code"}`,
			wantErr: identity.ErrWeChatAuthorizationInvalid,
		},
		{
			name:    "already used authorization code",
			status:  http.StatusOK,
			body:    `{"errcode":40163,"errmsg":"code been used"}`,
			wantErr: identity.ErrWeChatAuthorizationInvalid,
		},
		{
			name:    "provider-reported failure",
			status:  http.StatusOK,
			body:    `{"errcode":-1,"errmsg":"system busy"}`,
			wantErr: identity.ErrIdentityProvider,
		},
		{
			name:    "answer without a verified identity",
			status:  http.StatusOK,
			body:    `{"access_token":"access-token-value"}`,
			wantErr: identity.ErrIdentityProvider,
		},
		{
			name:    "malformed answer",
			status:  http.StatusOK,
			body:    `not a provider answer`,
			wantErr: identity.ErrIdentityProvider,
		},
		{
			name:    "provider transport failure",
			status:  http.StatusInternalServerError,
			body:    `upstream error text`,
			wantErr: identity.ErrIdentityProvider,
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

			client := New("wx-app", "wx-secret")
			client.endpoint = provider.URL
			client.httpClient = provider.Client()

			verified, err := client.Verify(context.Background(), "code-one")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Verify error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				assertNoProviderSecret(t, err, "code-one")
				return
			}
			if verified.AppID != "wx-app" || verified.OpenID != test.wantOpenID || verified.UnionID != test.wantUnion {
				t.Fatalf("verified identity = %+v, want AppID wx-app, OpenID %q, UnionID %q",
					verified, test.wantOpenID, test.wantUnion)
			}

			query := received.URL.Query()
			for name, want := range map[string]string{
				"appid":      "wx-app",
				"secret":     "wx-secret",
				"code":       "code-one",
				"grant_type": "authorization_code",
			} {
				if got := query.Get(name); got != want {
					t.Errorf("provider request %s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// A process without configured credentials must fail the provider boundary
// instead of calling WeChat with empty values.
func TestVerifyWithoutConfiguredCredentialsDoesNotCallTheProvider(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unconfigured client called the provider")
	}))
	defer provider.Close()

	client := New("", "")
	client.endpoint = provider.URL
	client.httpClient = provider.Client()

	if _, err := client.Verify(context.Background(), "code-one"); !errors.Is(err, identity.ErrIdentityProvider) {
		t.Fatalf("Verify error = %v, want %v", err, identity.ErrIdentityProvider)
	}
}

// assertNoProviderSecret keeps provider messages and exchanged values out of
// every error the adapter reports.
func assertNoProviderSecret(t *testing.T, err error, forbidden ...string) {
	t.Helper()

	message := err.Error()
	for _, value := range append(forbidden, "wx-secret", "access-token-value", "invalid code", "code been used", "system busy", "upstream error text") {
		if strings.Contains(message, value) {
			t.Errorf("error %q leaks provider input %q", message, value)
		}
	}
}
