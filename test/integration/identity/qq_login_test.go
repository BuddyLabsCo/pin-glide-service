//go:build integration

package identity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	qqfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/qq"
)

const qqLoginPath = "/v1/auth/qq"

func (e *loginEnvironment) qqLoginOK(t *testing.T, body string) bundle {
	t.Helper()

	status, payload, header := post(t, e, qqLoginPath, body)
	if status != http.StatusOK {
		t.Fatalf("QQ login status = %d, want %d: %s", status, http.StatusOK, payload)
	}
	if cacheControl := header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("QQ login Cache-Control = %q, want no-store", cacheControl)
	}
	issued := bundle{}
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode QQ credential bundle: %v: %s", err, payload)
	}
	if issued.AccountID == "" || issued.SenderID == "" ||
		issued.UserSession.Token == "" || issued.Uploader.Token == "" || issued.Refresh.Token == "" {
		t.Fatalf("QQ login issued an incomplete bundle: %+v", issued)
	}
	return issued
}

func (e *loginEnvironment) qqLoginFails(t *testing.T, body string, wantStatus int, wantCode string) {
	t.Helper()

	status, payload, _ := post(t, e, qqLoginPath, body)
	if status != wantStatus {
		t.Fatalf("QQ login status = %d, want %d: %s", status, wantStatus, payload)
	}
	decoded := problem{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode QQ problem: %v: %s", err, payload)
	}
	if decoded.Code != wantCode {
		t.Fatalf("QQ login code = %q, want %q: %s", decoded.Code, wantCode, payload)
	}
}

// QQ login shares Account and Sender issuance with WeChat while keeping the
// provider namespaces distinct for equal application and OpenID values.
func TestQQLoginCreatesRestoresAndSeparatesAccounts(t *testing.T) {
	environment := startLogin(t)
	environment.qq.Accept("qq-first", identity.Identity{AppID: "qq-app", OpenID: "openid-shared"})
	environment.qq.Accept("qq-second", identity.Identity{AppID: "qq-app", OpenID: "openid-shared"})
	environment.qq.Accept("qq-foreign", identity.Identity{AppID: "qq-app", OpenID: "openid-foreign"})
	environment.provider.Accept("wechat-same", identity.Identity{AppID: "qq-app", OpenID: "openid-shared"})

	first := environment.qqLoginOK(t, `{"qqCode":"qq-first"}`)
	second := environment.qqLoginOK(t, `{"qqCode":"qq-second"}`)
	if first.AccountID != second.AccountID {
		t.Errorf("repeated QQ identity produced Accounts %q and %q", first.AccountID, second.AccountID)
	}
	if first.SenderID == second.SenderID {
		t.Error("QQ login without senderId reused another installation's Sender")
	}

	restored := environment.qqLoginOK(t, `{"qqCode":"qq-first","senderId":"`+first.SenderID+`"}`)
	if restored.AccountID != first.AccountID || restored.SenderID != first.SenderID {
		t.Errorf("QQ restore = %+v, want Account %q and Sender %q",
			restored, first.AccountID, first.SenderID)
	}
	if restored.UserSession.Token == first.UserSession.Token || restored.Refresh.Token == first.Refresh.Token {
		t.Error("fresh QQ login reused the replaced credential family")
	}
	credentials := identity.NewService(environment.pool, nil)
	if _, err := credentials.Authenticate(context.Background(), first.UserSession.Token, identity.ScopeUserSession); err == nil {
		t.Error("the replaced QQ family still authorizes management")
	}
	if senderID, err := credentials.Authenticate(context.Background(), restored.UserSession.Token, identity.ScopeUserSession); err != nil || senderID != first.SenderID {
		t.Errorf("the fresh QQ family does not authorize management: %q %v", senderID, err)
	}

	wechat := environment.loginOK(t, `{"wechatCode":"wechat-same"}`)
	if wechat.AccountID == first.AccountID {
		t.Errorf("equal AppID and OpenID across WeChat and QQ merged into Account %q", first.AccountID)
	}

	environment.qqLoginFails(t, `{"qqCode":"qq-foreign","senderId":"`+first.SenderID+`"}`,
		http.StatusNotFound, "resource_not_found")
	environment.qqLoginFails(t, `{"qqCode":"qq-unregistered"}`,
		http.StatusUnauthorized, "qq_authorization_invalid")
	environment.qq.Fail("qq-broken", identity.ErrIdentityProvider)
	environment.qqLoginFails(t, `{"qqCode":"qq-broken"}`,
		http.StatusBadGateway, "identity_provider_error")
	environment.qqLoginFails(t, `{"qqCode":""}`,
		http.StatusUnprocessableEntity, "validation_failed")

	environment.qq.Accept("qq-revoked", identity.Identity{AppID: "qq-app", OpenID: "openid-revoked"})
	environment.exec(t, `INSERT INTO pin_glide.identity_accounts (id, provider, app_id, open_id)
		VALUES ('account-qq-revoked', 'qq', 'qq-app', 'openid-revoked')`)
	environment.exec(t, `INSERT INTO pin_glide.identity_sender_registrations (id, account_id, state, revoked_at)
		VALUES ('sender-qq-revoked', 'account-qq-revoked', 'revoked', now())`)
	environment.qqLoginFails(t, `{"qqCode":"qq-revoked","senderId":"sender-qq-revoked"}`,
		http.StatusConflict, "device_registration_revoked")

	request, err := http.NewRequest(http.MethodGet, environment.service.BaseURL+"/v1/self/receivers", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+restored.UserSession.Token)
	response, err := environment.service.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("QQ credential authorized protected operation with status %d", response.StatusCode)
	}
}

// A configured provider is independent of an absent provider pair.
func TestQQLoginWorksWithoutWeChatConfiguration(t *testing.T) {
	qqProvider := qqfake.New()
	environment := startLoginWithProviders(t,
		observability.NewLogger(os.Stderr, "integration"), qqProvider)
	environment.qq = qqProvider
	qqProvider.Accept("qq-only", identity.Identity{AppID: "qq-app", OpenID: "openid-only"})

	issued := environment.qqLoginOK(t, `{"qqCode":"qq-only"}`)
	if issued.AccountID == "" || issued.SenderID == "" {
		t.Fatalf("QQ-only login issued an incomplete bundle: %+v", issued)
	}
	environment.loginFails(t, `{"wechatCode":"wechat-missing"}`,
		http.StatusBadGateway)
}

// A rejected QQ proof cannot create an Account or Sender.
func TestQQLoginFailureDoesNotIssueIdentity(t *testing.T) {
	environment := startLogin(t)
	environment.qq.Fail("qq-invalid", identity.ErrQQAuthorizationInvalid)

	environment.qqLoginFails(t, `{"qqCode":"qq-invalid"}`,
		http.StatusUnauthorized, "qq_authorization_invalid")
	if count := environment.count(t,
		`SELECT count(*) FROM pin_glide.identity_accounts WHERE provider = 'qq'`); count != 0 {
		t.Errorf("failed QQ login created %d Accounts", count)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.identity_sender_registrations`); count != 0 {
		t.Errorf("failed QQ login created %d Senders", count)
	}

	request, err := http.NewRequest(http.MethodGet, environment.service.BaseURL+"/v1/self/receivers", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 16))
	response, err := environment.service.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invented credential status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}
