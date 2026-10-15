//go:build integration

// Owner security revocation is exercised the way an Account owner experiences
// it: one HTTP request over the composed service, a disposable real PostgreSQL
// holding the shipped schema, and the faked WeChat boundary.
package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// revocationPath is the implemented owner security revocation operation.
const revocationPath = "/v1/devices/"

// deviceRevocationFixturePath is the accepted owner revocation contract,
// relative to this package.
const deviceRevocationFixturePath = "../../fixtures/contract/http/device_revocation_v1.json"

// deviceRevocationFixture is the accepted contract of owner revocation. Every
// failure status lists the codes the accepted contract defines for it.
type deviceRevocationFixture struct {
	OperationID string `json:"operationId"`
	Path        string `json:"path"`
	Method      string `json:"method"`
	Request     struct {
		Schema     string   `json:"schema"`
		Required   []string `json:"required"`
		Properties []string `json:"properties"`
	} `json:"request"`
	Success struct {
		Status string `json:"status"`
	} `json:"success"`
	Errors map[string][]string `json:"errors"`
}

// revokeDevice posts one owner revocation with a raw body and returns the
// status, payload and headers.
func (e *loginEnvironment) revokeDevice(t *testing.T, senderID, bearer, body string) (int, []byte, http.Header) {
	t.Helper()

	request, err := http.NewRequest(http.MethodPost,
		e.service.BaseURL+revocationPath+senderID+"/revoke", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build revocation request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := e.service.Client.Do(request)
	if err != nil {
		t.Fatalf("POST %s%s/revoke: %v", revocationPath, senderID, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read revocation response: %v", err)
	}
	return response.StatusCode, payload, response.Header
}

// revokeOK requires one accepted owner revocation: an empty 204 that returns
// no credential and is never cacheable.
func revokeOK(t *testing.T, environment *loginEnvironment, senderID, bearer, body string) {
	t.Helper()

	status, payload, header := environment.revokeDevice(t, senderID, bearer, body)
	if status != http.StatusNoContent {
		t.Fatalf("revocation status = %d, want %d: %s", status, http.StatusNoContent, payload)
	}
	if len(payload) != 0 {
		t.Fatalf("revocation returned a body: %s", payload)
	}
	if cacheControl := header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("revocation Cache-Control = %q, want no-store", cacheControl)
	}
}

// revokeFails requires one rejected owner revocation and checks the status and
// code the accepted contract defines for it.
func revokeFails(t *testing.T, environment *loginEnvironment, senderID, bearer, body string, wantStatus int, wantCode string) {
	t.Helper()

	if accepted := readDeviceRevocationFixture(t).Errors[strconv.Itoa(wantStatus)]; !slices.Contains(accepted, wantCode) {
		t.Fatalf("the accepted contract defines no %s for status %d", wantCode, wantStatus)
	}
	status, payload, _ := environment.revokeDevice(t, senderID, bearer, body)
	if status != wantStatus {
		t.Fatalf("revocation status = %d, want %d: %s", status, wantStatus, payload)
	}
	decoded := problem{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, payload)
	}
	if decoded.Code != wantCode {
		t.Fatalf("revocation code = %q, want %q: %s", decoded.Code, wantCode, payload)
	}
	if decoded.RequestID == "" {
		t.Errorf("revocation problem carries no requestId: %s", payload)
	}
}

// readDeviceRevocationFixture reads the accepted owner revocation contract.
func readDeviceRevocationFixture(t *testing.T) deviceRevocationFixture {
	t.Helper()

	contents, err := os.ReadFile(deviceRevocationFixturePath)
	if err != nil {
		t.Fatalf("read revocation fixture: %v", err)
	}
	fixture := deviceRevocationFixture{}
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatalf("decode revocation fixture: %v", err)
	}
	return fixture
}

// Owner security revocation permanently closes one registration inside the
// Account that proves it, and leaves the shared Receiver's registration, its
// credential and every other Sender's association untouched. The closed ID can
// never be restored, while a login without it registers a new installation.
func TestOwnerRevocationClosesOnlyThatSenderRegistration(t *testing.T) {
	environment := startLogin(t)
	for _, openID := range []string{"revoked", "sharing"} {
		environment.provider.Accept("code-"+openID, identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-" + openID})
	}
	revoked := environment.loginOK(t, `{"wechatCode":"code-revoked"}`)
	sharing := environment.loginOK(t, `{"wechatCode":"code-sharing"}`)
	environment.seedPairing(t, "receiver-revoke", "pairing-revoked", revoked.SenderID)
	environment.seedPairing(t, "receiver-revoke", "pairing-sharing", sharing.SenderID)

	environment.provider.Accept("code-revoke-owner", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoked"})
	revokeOK(t, environment, revoked.SenderID, revoked.UserSession.Token,
		`{"wechatCode":"code-revoke-owner"}`)

	// Every credential of the closed registration reports revocation rather
	// than an unknown value.
	t.Logf("registration state %q; credential rows %d, revoked %d",
		environment.registrationState(t, revoked.SenderID),
		environment.count(t, `SELECT count(*) FROM pin_glide.identity_credentials WHERE sender_id = $1`, revoked.SenderID),
		environment.count(t, `SELECT count(*) FROM pin_glide.identity_credentials WHERE sender_id = $1 AND revoked_at IS NOT NULL`, revoked.SenderID))
	for _, credential := range []struct {
		token string
		scope identity.Scope
	}{
		{token: revoked.UserSession.Token, scope: identity.ScopeUserSession},
		{token: revoked.Uploader.Token, scope: identity.ScopeUploader},
		{token: revoked.Refresh.Token, scope: identity.ScopeRefresh},
	} {
		if _, err := identity.NewService(environment.pool, nil).Authenticate(
			context.Background(), credential.token, credential.scope); !errors.Is(err, identity.ErrCredentialRevoked) {
			t.Errorf("the revoked %s credential reports %v, want credential_revoked", credential.scope, err)
		}
	}
	if state := environment.associationState(t, "pairing-revoked"); state != "removed" {
		t.Errorf("the revoked Sender's association state = %q, want removed", state)
	}
	if state := environment.associationState(t, "pairing-sharing"); state != "pending" {
		t.Errorf("the other Sender's association state = %q, want the untouched pending one", state)
	}
	if created := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_credentials
		WHERE receiver_id = 'receiver-revoke'`); created != 0 {
		t.Errorf("revocation changed the shared Receiver's credentials: %d rows", created)
	}
	if registered := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_registrations
		WHERE id = 'receiver-revoke' AND state = 'pending'`); registered != 1 {
		t.Errorf("revocation changed the shared Receiver registration: %d rows", registered)
	}

	// The registration stays, permanently closed, and its ID is never
	// restorable by a fresh login of the Account that owns it.
	if closed := environment.count(t, `SELECT count(*) FROM pin_glide.identity_sender_registrations
		WHERE id = $1 AND state = 'revoked' AND revoked_at IS NOT NULL`, revoked.SenderID); closed != 1 {
		t.Errorf("the revoked registration is not stored as permanently closed: %d rows", closed)
	}
	environment.provider.Accept("code-revoke-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoked"})
	environment.loginFails(t, `{"wechatCode":"code-revoke-again","senderId":"`+revoked.SenderID+`"}`, http.StatusConflict)
	if recreated := environment.count(t, `SELECT count(*) FROM pin_glide.identity_sender_registrations
		WHERE account_id = $1`, revoked.AccountID); recreated != 1 {
		t.Errorf("a refused restore changed the Account's registrations: %d rows", recreated)
	}

	// The same Account can still register a new installation, which is the
	// supported way back from a revocation.
	registered := environment.loginOK(t, `{"wechatCode":"code-revoke-again"}`)
	if registered.SenderID == revoked.SenderID {
		t.Errorf("a new registration reused the closed Sender ID %q", revoked.SenderID)
	}
	authorizes(t, environment, sharing.UserSession.Token, identity.ScopeUserSession)

	// A live installation of the same Account learns that the registration it
	// names is already closed instead of closing it twice, while another
	// Account's view of the same ID stays invisible.
	environment.provider.Accept("code-revoke-closed", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoked"})
	revokeFails(t, environment, revoked.SenderID, registered.UserSession.Token,
		`{"wechatCode":"code-revoke-closed"}`, http.StatusConflict, "device_registration_revoked")
	revokeFails(t, environment, revoked.SenderID, sharing.UserSession.Token,
		`{"wechatCode":"code-sharing"}`, http.StatusNotFound, "resource_not_found")
}

// Owner revocation needs both halves of its authority: an owned registration
// and fresh WeChat proof of the Account that owns it. An absent or unusable
// management credential, a missing, unusable or foreign proof, an unknown
// registration and another Account's registration all leave every registration
// and association as they were.
func TestOwnerRevocationRequiresAnOwnedRegistrationAndFreshSameAccountProof(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-owned", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-owned"})
	environment.provider.Accept("code-foreign", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-foreign"})
	owned := environment.loginOK(t, `{"wechatCode":"code-owned"}`)
	foreign := environment.loginOK(t, `{"wechatCode":"code-foreign"}`)
	environment.seedPairing(t, "receiver-owner", "pairing-owner", owned.SenderID)

	environment.provider.Accept("code-owned-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-owned"})
	environment.provider.Accept("code-foreign-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-foreign"})
	environment.provider.Fail("code-broken", identity.ErrIdentityProvider)
	environment.provider.Fail("code-invalid", identity.ErrWeChatAuthorizationInvalid)

	// The management credential authorizes the request, so an absent or wrongly
	// scoped one never reaches the owner proof.
	revokeFails(t, environment, owned.SenderID, "", `{"wechatCode":"code-owned-again"}`,
		http.StatusUnauthorized, "credential_missing")
	revokeFails(t, environment, owned.SenderID, "credential-that-was-never-issued", `{"wechatCode":"code-owned-again"}`,
		http.StatusUnauthorized, "credential_invalid")
	revokeFails(t, environment, owned.SenderID, owned.Uploader.Token, `{"wechatCode":"code-owned-again"}`,
		http.StatusUnauthorized, "credential_invalid")
	environment.provider.Accept("code-owned-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-owned"})

	// Fresh proof is mandatory and must name the Account that owns the
	// registration; another Account's proof revokes nothing.
	revokeFails(t, environment, owned.SenderID, owned.UserSession.Token, `{}`,
		http.StatusUnprocessableEntity, "validation_failed")
	revokeFails(t, environment, owned.SenderID, owned.UserSession.Token, `{"wechatCode":"code-foreign-again"}`,
		http.StatusForbidden, "fresh_identity_required")
	revokeFails(t, environment, owned.SenderID, owned.UserSession.Token, `{"wechatCode":"code-invalid"}`,
		http.StatusUnauthorized, "wechat_authorization_invalid")
	revokeFails(t, environment, owned.SenderID, owned.UserSession.Token, `{"wechatCode":"code-broken"}`,
		http.StatusBadGateway, "identity_provider_error")

	// An unknown registration and another Account's registration stay
	// indistinguishably invisible.
	revokeFails(t, environment, "sender-that-never-existed", owned.UserSession.Token, `{"wechatCode":"code-owned-again"}`,
		http.StatusNotFound, "resource_not_found")
	revokeFails(t, environment, owned.SenderID, foreign.UserSession.Token, `{"wechatCode":"code-foreign-again"}`,
		http.StatusNotFound, "resource_not_found")

	authorizes(t, environment, owned.UserSession.Token, identity.ScopeUserSession)
	authorizes(t, environment, foreign.UserSession.Token, identity.ScopeUserSession)
	if state := environment.associationState(t, "pairing-owner"); state != "pending" {
		t.Errorf("a refused revocation changed the association state to %q", state)
	}

	// An owned registration that is already closed reports its own permanent
	// outcome instead of being closed twice, and its owner alone learns that.
	environment.provider.Accept("code-owned-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-owned"})
	revokeOK(t, environment, owned.SenderID, owned.UserSession.Token, `{"wechatCode":"code-owned-again"}`)
	environment.provider.Accept("code-closed-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-owned"})
	// The credential itself is revoked with its registration, so the caller
	// reports that before the already closed registration is considered.
	revokeFails(t, environment, owned.SenderID, owned.UserSession.Token, `{"wechatCode":"code-closed-again"}`,
		http.StatusUnauthorized, "credential_revoked")
}

// A revocation whose cleanup cannot commit reports the standard failure and
// leaves the registration open, so a client cannot mistake an uncommitted
// revocation for a closed registration, and the retry succeeds.
func TestOwnerRevocationRollsBackWhenCleanupCannotCommit(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-revoke-rollback", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoke-rollback"})
	owned := environment.loginOK(t, `{"wechatCode":"code-revoke-rollback"}`)
	environment.seedPairing(t, "receiver-revoke-rollback", "pairing-revoke-rollback", owned.SenderID)
	environment.provider.Accept("code-revoke-rollback-owner", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoke-rollback"})

	environment.exec(t, `CREATE FUNCTION pin_glide.identity_sender_registrations_test_reject_close() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected registration close failure'; END $$`)
	environment.exec(t, `CREATE TRIGGER trg_identity_sender_registrations_test_reject_close
		BEFORE UPDATE ON pin_glide.identity_sender_registrations
		FOR EACH ROW EXECUTE FUNCTION pin_glide.identity_sender_registrations_test_reject_close()`)

	revokeFails(t, environment, owned.SenderID, owned.UserSession.Token,
		`{"wechatCode":"code-revoke-rollback-owner"}`, http.StatusServiceUnavailable, "service_unavailable")
	authorizes(t, environment, owned.UserSession.Token, identity.ScopeUserSession)
	if state := environment.associationState(t, "pairing-revoke-rollback"); state != "pending" {
		t.Errorf("a failed revocation changed the association state to %q", state)
	}
	// Nothing of the failed attempt survived, so the same credentials and proof
	// close the registration on the retry instead of reporting it as closed.
	environment.exec(t, `DROP TRIGGER trg_identity_sender_registrations_test_reject_close ON pin_glide.identity_sender_registrations`)
	environment.exec(t, `DROP FUNCTION pin_glide.identity_sender_registrations_test_reject_close()`)
	revokeOK(t, environment, owned.SenderID, owned.UserSession.Token, `{"wechatCode":"code-revoke-rollback-owner"}`)
	environment.provider.Accept("code-revoke-rollback-restore", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoke-rollback"})
	environment.loginFails(t, `{"wechatCode":"code-revoke-rollback-restore","senderId":"`+owned.SenderID+`"}`,
		http.StatusConflict)
}

// A Sender revocation permanently closes only the presented registration: once
// the owner revokes one, the Sender's own credentials can no longer revoke a
// fresh family or another registration, because the closed registration
// authorizes nothing.
func TestRevokedSenderCannotActOnItsOwnRegistration(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-revoke-quiet", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoke-quiet"})
	owned := environment.loginOK(t, `{"wechatCode":"code-revoke-quiet"}`)
	environment.provider.Accept("code-revoke-quiet-owner", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoke-quiet"})
	revokeOK(t, environment, owned.SenderID, owned.UserSession.Token, `{"wechatCode":"code-revoke-quiet-owner"}`)

	// The closed registration cannot act again: every credential it held is
	// revoked with it.
	environment.provider.Accept("code-revoke-quiet-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-revoke-quiet"})
	revokeFails(t, environment, owned.SenderID, owned.UserSession.Token,
		`{"wechatCode":"code-revoke-quiet-again"}`, http.StatusUnauthorized, "credential_revoked")
	status, payload, _ := environment.logout(t, owned.UserSession.Token, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("logout with a closed registration = %d, want 401: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "credential_revoked" {
		t.Errorf("logout with a closed registration code = %q, want credential_revoked", code)
	}
}
