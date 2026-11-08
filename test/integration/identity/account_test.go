//go:build integration

package identity_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// The public read path must observe the shared login cache without changing
// Account or device identity, including after a provider profile failure.
func TestQQLoginCachesSharedAccountProfile(t *testing.T) {
	e := startLogin(t)
	e.qq.Accept("first", identity.Identity{AppID: "qq-app", OpenID: "owner",
		Profile: &identity.DisplayProfile{Nickname: "First name", AvatarURL: "https://qq.example/first"}})
	first := e.qqLoginOK(t, `{"qqCode":"first"}`)
	assertOwnAccount(t, e, first.Uploader.Token, first.AccountID, "First name", "https://qq.example/first")

	e.qq.Accept("next", identity.Identity{AppID: "qq-app", OpenID: "owner",
		Profile: &identity.DisplayProfile{Nickname: "New name", AvatarURL: "https://qq.example/next"}})
	other := e.qqLoginOK(t, `{"qqCode":"next"}`)
	if other.AccountID != first.AccountID || other.SenderID == first.SenderID {
		t.Fatalf("second installation = %+v, want same Account and different Sender", other)
	}
	assertOwnAccount(t, e, first.Uploader.Token, first.AccountID, "New name", "https://qq.example/next")

	e.qq.Accept("restore", identity.Identity{AppID: "qq-app", OpenID: "owner",
		Profile: &identity.DisplayProfile{Nickname: "Restored name", AvatarURL: "https://qq.example/restored"}})
	restored := e.qqLoginOK(t, `{"qqCode":"restore","senderId":"`+first.SenderID+`"}`)
	if restored.AccountID != first.AccountID || restored.SenderID != first.SenderID {
		t.Fatalf("restoration changed identity: %+v", restored)
	}
	assertOwnAccount(t, e, other.Uploader.Token, first.AccountID, "Restored name", "https://qq.example/restored")
	e.qq.Accept("profile-failure", identity.Identity{AppID: "qq-app", OpenID: "owner"})
	failedProfile := e.qqLoginOK(t, `{"qqCode":"profile-failure"}`)
	assertOwnAccount(t, e, failedProfile.Uploader.Token, first.AccountID, "Restored name", "https://qq.example/restored")

	e.qq.Accept("uncached", identity.Identity{AppID: "qq-app", OpenID: "uncached"})
	uncached := e.qqLoginOK(t, `{"qqCode":"uncached"}`)
	assertOwnAccount(t, e, uncached.Uploader.Token, uncached.AccountID, "", "")
	e.qq.Fail("denied", identity.ErrQQAuthorizationInvalid)
	e.qqLoginFails(t, `{"qqCode":"denied"}`, http.StatusUnauthorized, "qq_authorization_invalid")
	assertOwnAccount(t, e, failedProfile.Uploader.Token, first.AccountID, "Restored name", "https://qq.example/restored")
	e.qq.Accept("foreign-restore", identity.Identity{AppID: "qq-app", OpenID: "owner",
		Profile: &identity.DisplayProfile{Nickname: "Must not commit", AvatarURL: "https://qq.example/wrong"}})
	e.qqLoginFails(t, `{"qqCode":"foreign-restore","senderId":"`+uncached.SenderID+`"}`, http.StatusNotFound, "resource_not_found")
	assertOwnAccount(t, e, failedProfile.Uploader.Token, first.AccountID, "Restored name", "https://qq.example/restored")
}

// Sender credentials keep their existing scope, expiry and permanent
// registration-revocation outcomes on the new read path.
func TestOwnAccountRejectsInvalidSenderAuthority(t *testing.T) {
	e := startLogin(t)
	e.provider.Accept("wechat", identity.Identity{AppID: "wx-app", OpenID: "owner"})
	issued := e.loginOK(t, `{"wechatCode":"wechat"}`)
	assertOwnAccount(t, e, issued.Uploader.Token, issued.AccountID, "", "")
	for _, test := range []struct{ token, code string }{
		{"", "credential_missing"}, {"invented", "credential_invalid"},
		{issued.UserSession.Token, "credential_invalid"}, {issued.Refresh.Token, "credential_invalid"},
	} {
		status, body := ownAccount(t, e, test.token)
		if status != http.StatusUnauthorized || body["code"] != test.code {
			t.Fatalf("credential rejection = %d %v, want %s", status, body, test.code)
		}
	}
	e.exec(t, `UPDATE pin_glide.identity_credentials SET expires_at = now() - interval '1 second'
		WHERE sender_id = $1 AND scope = 'uploader'`, issued.SenderID)
	status, body := ownAccount(t, e, issued.Uploader.Token)
	if status != http.StatusUnauthorized || body["code"] != "credential_expired" {
		t.Fatalf("expired U = %d %v", status, body)
	}
	e.exec(t, `UPDATE pin_glide.identity_sender_registrations SET state = 'revoked', revoked_at = now() WHERE id = $1`, issued.SenderID)
	status, body = ownAccount(t, e, issued.Uploader.Token)
	if status != http.StatusConflict || body["code"] != "device_registration_revoked" {
		t.Fatalf("revoked Sender = %d %v", status, body)
	}
}

func ownAccount(t *testing.T, e *loginEnvironment, token string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, e.service.BaseURL+"/v1/self/account", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := e.service.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Error("Account response must not be cacheable")
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("Account response: %v: %s", err, payload)
	}
	return response.StatusCode, body
}

func assertOwnAccount(t *testing.T, e *loginEnvironment, token, accountID, nickname, avatar string) {
	t.Helper()
	status, body := ownAccount(t, e, token)
	var wantName, wantAvatar any
	if nickname != "" {
		wantName = nickname
	}
	if avatar != "" {
		wantAvatar = avatar
	}
	if status != http.StatusOK || len(body) != 3 || body["accountId"] != accountID || body["nickname"] != wantName || body["avatarUrl"] != wantAvatar {
		t.Fatalf("Account = %d %v, want %s/%v/%v", status, body, accountID, wantName, wantAvatar)
	}
}
