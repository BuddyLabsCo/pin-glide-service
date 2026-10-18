package qq

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// The adapter owns QQ's authorization-code exchange and OpenID binding check.
// It must accept only the configured application's provider-verified identity,
// distinguish an unusable authorization from a provider failure, and keep the
// AppKey, code, access token and raw provider response out of errors.
func TestVerifyExchangesAuthorizationCodeForConfiguredIdentity(t *testing.T) {
	var tokenRequest, openIDRequest *http.Request
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			tokenRequest = request
			_, _ = writer.Write([]byte(`{"access_token":"qq-access-token","expires_in":7776000,"refresh_token":"qq-refresh-token"}`))
		case "/me":
			openIDRequest = request
			_, _ = writer.Write([]byte(`{"client_id":"qq-app","openid":"qq-openid"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer provider.Close()

	client := New("qq-app", "qq-app-key")
	client.tokenEndpoint = provider.URL + "/token"
	client.openIDEndpoint = provider.URL + "/me"
	client.httpClient = provider.Client()

	verified, err := client.Verify(context.Background(), "authorization-code")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified.Provider != identity.ProviderQQ || verified.AppID != "qq-app" ||
		verified.OpenID != "qq-openid" || verified.UnionID != "" {
		t.Fatalf("verified identity = %+v, want QQ app qq-app and the provider OpenID", verified)
	}

	tokenQuery := tokenRequest.URL.Query()
	for name, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "qq-app",
		"client_secret": "qq-app-key",
		"code":          "authorization-code",
		"fmt":           "json",
	} {
		if got := tokenQuery.Get(name); got != want {
			t.Errorf("token request %s = %q, want %q", name, got, want)
		}
	}
	if tokenQuery.Has("redirect_uri") {
		t.Error("mobile server-side exchange sent a website redirect_uri")
	}

	openIDQuery := openIDRequest.URL.Query()
	if got := openIDQuery.Get("access_token"); got != "qq-access-token" {
		t.Errorf("OpenID request access_token = %q, want the exchanged token", got)
	}
	if got := openIDQuery.Get("fmt"); got != "json" {
		t.Errorf("OpenID request fmt = %q, want json", got)
	}
}

func TestVerifyMapsUnusableAuthorizationCodes(t *testing.T) {
	for _, code := range []int{100019, 100020, 100067, 100068} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`{"error":` + strconv.Itoa(code) + `,"error_description":"raw provider explanation"}`))
			}))
			defer provider.Close()

			client := New("qq-app", "qq-app-key")
			client.tokenEndpoint = provider.URL
			client.httpClient = provider.Client()

			_, err := client.Verify(context.Background(), "authorization-code")
			if !errors.Is(err, identity.ErrQQAuthorizationInvalid) {
				t.Fatalf("Verify error = %v, want %v", err, identity.ErrQQAuthorizationInvalid)
			}
			assertNoQQSecret(t, err)
		})
	}
}

func TestVerifyRejectsUnusableProviderAnswers(t *testing.T) {
	tests := []struct {
		name       string
		tokenBody  string
		openIDBody string
		wantErr    error
	}{
		{
			name:       "application mismatch",
			tokenBody:  `{"access_token":"qq-access-token"}`,
			openIDBody: `{"client_id":"another-app","openid":"qq-openid"}`,
			wantErr:    identity.ErrIdentityProvider,
		},
		{
			name:       "missing OpenID",
			tokenBody:  `{"access_token":"qq-access-token"}`,
			openIDBody: `{"client_id":"qq-app"}`,
			wantErr:    identity.ErrIdentityProvider,
		},
		{
			name:       "missing application binding",
			tokenBody:  `{"access_token":"qq-access-token"}`,
			openIDBody: `{"openid":"qq-openid"}`,
			wantErr:    identity.ErrIdentityProvider,
		},
		{
			name:       "missing access token",
			tokenBody:  `{"expires_in":7776000}`,
			openIDBody: `{"client_id":"qq-app","openid":"qq-openid"}`,
			wantErr:    identity.ErrIdentityProvider,
		},
		{
			name:       "malformed token answer",
			tokenBody:  `not json`,
			openIDBody: `{"client_id":"qq-app","openid":"qq-openid"}`,
			wantErr:    identity.ErrIdentityProvider,
		},
		{
			name:       "malformed OpenID answer",
			tokenBody:  `{"access_token":"qq-access-token"}`,
			openIDBody: `not json`,
			wantErr:    identity.ErrIdentityProvider,
		},
		{
			name:       "provider-reported token failure",
			tokenBody:  `{"error":100062,"error_description":"raw provider explanation"}`,
			openIDBody: `{"client_id":"qq-app","openid":"qq-openid"}`,
			wantErr:    identity.ErrIdentityProvider,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/token":
					_, _ = writer.Write([]byte(test.tokenBody))
				case "/me":
					_, _ = writer.Write([]byte(test.openIDBody))
				default:
					http.NotFound(writer, request)
				}
			}))
			defer provider.Close()

			client := New("qq-app", "qq-app-key")
			client.tokenEndpoint = provider.URL + "/token"
			client.openIDEndpoint = provider.URL + "/me"
			client.httpClient = provider.Client()

			_, err := client.Verify(context.Background(), "authorization-code")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Verify error = %v, want %v", err, test.wantErr)
			}
			assertNoQQSecret(t, err)
		})
	}
}

func TestVerifyMapsTransportAndHTTPFailuresWithoutLeakingRequestURL(t *testing.T) {
	for _, path := range []string{"/token", "/me"} {
		t.Run(path, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/token" {
					_, _ = writer.Write([]byte(`{"access_token":"qq-access-token"}`))
					return
				}
				writer.WriteHeader(http.StatusInternalServerError)
				_, _ = writer.Write([]byte("raw provider response"))
			}))
			defer provider.Close()

			client := New("qq-app", "qq-app-key")
			client.tokenEndpoint = provider.URL + "/token"
			client.openIDEndpoint = provider.URL + "/me"
			client.httpClient = provider.Client()

			_, err := client.Verify(context.Background(), "authorization-code")
			if !errors.Is(err, identity.ErrIdentityProvider) {
				t.Fatalf("Verify error = %v, want %v", err, identity.ErrIdentityProvider)
			}
			assertNoQQSecret(t, err)
		})
	}

	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := provider.URL
	provider.Close()

	client := New("qq-app", "qq-app-key")
	client.tokenEndpoint = unreachable + "/token?code=authorization-code&client_secret=qq-app-key"
	_, err := client.Verify(context.Background(), "authorization-code")
	if !errors.Is(err, identity.ErrIdentityProvider) {
		t.Fatalf("Verify error = %v, want %v", err, identity.ErrIdentityProvider)
	}
	assertNoQQSecret(t, err)
}

func TestVerifyWithoutConfiguredCredentialsDoesNotCallTheProvider(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unconfigured client called the provider")
	}))
	defer provider.Close()

	for _, test := range []struct {
		name  string
		appID string
		key   string
	}{
		{name: "neither"},
		{name: "missing app key", appID: "qq-app"},
		{name: "missing app id", key: "qq-app-key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := New(test.appID, test.key)
			client.tokenEndpoint = provider.URL + "/token"
			client.openIDEndpoint = provider.URL + "/me"
			client.httpClient = provider.Client()

			if _, err := client.Verify(context.Background(), "authorization-code"); !errors.Is(err, identity.ErrIdentityProvider) {
				t.Fatalf("Verify error = %v, want %v", err, identity.ErrIdentityProvider)
			}
		})
	}
}

// assertNoQQSecret keeps credentials, authorization codes and provider text out
// of every error the adapter reports.
func assertNoQQSecret(t *testing.T, err error) {
	t.Helper()

	message := err.Error()
	for _, value := range []string{
		"qq-app-key", "authorization-code", "qq-access-token", "qq-refresh-token",
		"raw provider explanation", "raw provider response",
	} {
		if strings.Contains(message, value) {
			t.Errorf("error %q leaks provider input %q", message, value)
		}
	}
}
