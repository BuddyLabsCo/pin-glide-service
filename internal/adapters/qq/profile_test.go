package qq

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// Login uses only the token whose application binding was verified, and QQ's
// avatar rather than the similarly named Qzone avatar.
func TestVerifyLoginFetchesAuthorizedDisplayProfile(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"qq-access-token"}`))
		case "/me":
			_, _ = w.Write([]byte(`{"client_id":"qq-app","openid":"qq-openid"}`))
		case "/profile":
			for key, want := range map[string]string{
				"access_token": "qq-access-token", "oauth_consumer_key": "qq-app", "openid": "qq-openid",
			} {
				if got := r.URL.Query().Get(key); got != want {
					t.Errorf("profile %s = %q, want %q", key, got, want)
				}
			}
			_, _ = w.Write([]byte(`{"ret":0,"nickname":"QQ name","figureurl_2":"https://qzone.example/avatar","figureurl_qq_1":"https://qq.example/small","figureurl_qq_2":"https://qq.example/large"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	client := New("qq-app", "qq-app-key")
	client.httpClient = provider.Client()
	client.tokenEndpoint = provider.URL + "/token"
	client.openIDEndpoint = provider.URL + "/me"
	client.profileEndpoint = provider.URL + "/profile"
	verified, err := client.VerifyLogin(context.Background(), "authorization-code")
	if err != nil {
		t.Fatal(err)
	}
	if verified.Profile == nil || verified.Profile.Nickname != "QQ name" || verified.Profile.AvatarURL != "https://qq.example/large" {
		t.Fatalf("profile = %+v, want authorized QQ name and large QQ avatar", verified.Profile)
	}
}

func TestVerifyLoginProfileFailuresDoNotFailIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		status     int
		wantAvatar string
	}{
		{name: "permission denied", body: `{"ret":100030,"msg":"qq-access-token raw provider explanation"}`},
		{name: "HTTP failure", body: "raw provider response", status: http.StatusBadGateway},
		{name: "malformed", body: "not JSON"},
		{name: "missing status", body: `{"nickname":"Name","figureurl_qq_1":"https://qq.example/small"}`},
		{name: "partial data", body: `{"ret":0,"is_lost":1,"nickname":"Name","figureurl_qq_1":"https://qq.example/small"}`},
		{name: "missing name", body: `{"ret":0,"figureurl_qq_1":"https://qq.example/small"}`},
		{name: "missing QQ avatar", body: `{"ret":0,"nickname":"Name","figureurl_2":"https://qzone.example/avatar"}`},
		{name: "unusable URL", body: `{"ret":0,"nickname":"Name","figureurl_qq_1":"javascript:alert(1)"}`},
		{name: "small QQ avatar fallback", body: `{"ret":0,"nickname":"Name","figureurl_qq_1":"https://qq.example/small"}`, wantAvatar: "https://qq.example/small"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token":
					_, _ = w.Write([]byte(`{"access_token":"qq-access-token"}`))
				case "/me":
					_, _ = w.Write([]byte(`{"client_id":"qq-app","openid":"qq-openid"}`))
				case "/profile":
					if test.status != 0 {
						w.WriteHeader(test.status)
					}
					_, _ = w.Write([]byte(test.body))
				}
			}))
			defer provider.Close()
			client := New("qq-app", "qq-app-key")
			client.httpClient = provider.Client()
			client.tokenEndpoint = provider.URL + "/token"
			client.openIDEndpoint = provider.URL + "/me"
			client.profileEndpoint = provider.URL + "/profile"
			verified, err := client.VerifyLogin(context.Background(), "authorization-code")
			if err != nil || verified.OpenID != "qq-openid" {
				t.Fatalf("profile failure invalidated identity: %+v %v", verified, err)
			}
			if test.wantAvatar == "" {
				if verified.Profile != nil {
					t.Fatalf("failed profile must not replace cache: %+v", verified.Profile)
				}
			} else if verified.Profile == nil || verified.Profile.AvatarURL != test.wantAvatar {
				t.Fatalf("profile = %+v, want fallback %s", verified.Profile, test.wantAvatar)
			}
		})
	}
}

func TestProfileFetchRequiresVerifiedLoginAndNeverRunsForProofs(t *testing.T) {
	for _, binding := range []string{"qq-app", "foreign-app"} {
		t.Run(binding, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token":
					_, _ = w.Write([]byte(`{"access_token":"qq-access-token"}`))
				case "/me":
					_, _ = w.Write([]byte(`{"client_id":"` + binding + `","openid":"qq-openid"}`))
				default:
					t.Error("profile requested for a proof or unverified AppID")
				}
			}))
			defer provider.Close()
			client := New("qq-app", "qq-app-key")
			client.httpClient = provider.Client()
			client.tokenEndpoint = provider.URL + "/token"
			client.openIDEndpoint = provider.URL + "/me"
			client.profileEndpoint = provider.URL + "/profile"
			verified, err := client.Verify(context.Background(), "authorization-code")
			if binding == "qq-app" {
				if err != nil || verified.Profile != nil {
					t.Fatalf("proof = %+v, %v", verified, err)
				}
			} else {
				_, err = client.VerifyLogin(context.Background(), "authorization-code")
				if !errors.Is(err, identity.ErrIdentityProvider) {
					t.Fatalf("binding mismatch error = %v", err)
				}
				assertNoQQSecret(t, err)
			}
		})
	}
}
