//go:build integration

// Ordinary Sender logout is exercised the way a Sender experiences it: one HTTP
// request over the composed service, a disposable real PostgreSQL holding the
// shipped schema, and the faked WeChat boundary.
package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// logoutPath is the implemented ordinary logout operation.
const logoutPath = "/v1/auth/logout"

// logoutFixturePath is the accepted logout contract, relative to this package.
const logoutFixturePath = "../../fixtures/contract/http/logout_v1.json"

// logoutFixture is the accepted contract of ordinary logout. Every failure
// status lists the codes the accepted contract defines for it.
type logoutFixture struct {
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

// logoutRefreshBody is the wire body of one logout that presents the renewal
// credential instead of a bearer access credential.
func logoutRefreshBody(refreshToken string) string {
	return fmt.Sprintf(`{"refreshToken":%q}`, refreshToken)
}

// logout posts one logout with either credential source and returns the status,
// payload and headers. The request carries no body unless the caller supplies
// one, so a test can state a request with no credential source at all.
func (e *loginEnvironment) logout(t *testing.T, bearer, body string) (int, []byte, http.Header) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(http.MethodPost, e.service.BaseURL+logoutPath, reader)
	if err != nil {
		t.Fatalf("build logout request: %v", err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := e.service.Client.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", logoutPath, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read logout response: %v", err)
	}
	return response.StatusCode, payload, response.Header
}

// logoutOK requires one accepted ordinary logout: an empty 204 that returns no
// credential and is never cacheable.
func logoutOK(t *testing.T, environment *loginEnvironment, bearer, body string) {
	t.Helper()

	status, payload, header := environment.logout(t, bearer, body)
	if status != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d: %s", status, http.StatusNoContent, payload)
	}
	if len(payload) != 0 {
		t.Fatalf("logout returned a body: %s", payload)
	}
	if cacheControl := header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("logout Cache-Control = %q, want no-store", cacheControl)
	}
}

// logoutFails requires one rejected logout and checks that the composed service
// reports the status and code the accepted contract defines.
func logoutFails(t *testing.T, environment *loginEnvironment, bearer, body string, wantStatus int, wantCode string) problem {
	t.Helper()

	if accepted := readLogoutFixture(t).Errors[strconv.Itoa(wantStatus)]; !slices.Contains(accepted, wantCode) {
		t.Fatalf("the accepted contract defines no %s for status %d", wantCode, wantStatus)
	}
	status, payload, _ := environment.logout(t, bearer, body)
	if status != wantStatus {
		t.Fatalf("logout status = %d, want %d: %s", status, wantStatus, payload)
	}

	decoded := problem{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, payload)
	}
	if decoded.Code != wantCode {
		t.Fatalf("logout code = %q, want %q: %s", decoded.Code, wantCode, payload)
	}
	if decoded.Status != wantStatus {
		t.Fatalf("problem status field = %d, want %d", decoded.Status, wantStatus)
	}
	// The shared problem model stamps every failure with the operation's
	// request identifier, whichever layer reports the outcome.
	if decoded.RequestID == "" {
		t.Errorf("logout problem carries no requestId: %s", payload)
	}
	return decoded
}

// authorizes requires that one credential still authorizes the scope it was
// issued for.
func authorizes(t *testing.T, environment *loginEnvironment, token string, scope identity.Scope) {
	t.Helper()

	if _, err := identity.NewService(environment.pool, nil).Authenticate(context.Background(), token, scope); err != nil {
		t.Errorf("the %s credential does not authorize: %v", scope, err)
	}
}

// noAuthority requires that one credential authorizes nothing any more, which
// is what a Sender observes of the family its logout revoked.
func noAuthority(t *testing.T, environment *loginEnvironment, token string, scope identity.Scope) {
	t.Helper()

	if _, err := identity.NewService(environment.pool, nil).Authenticate(context.Background(), token, scope); !errors.Is(err, identity.ErrCredentialInvalid) {
		t.Errorf("the revoked %s credential reports %v, want credential_invalid", scope, err)
	}
}

// readLogoutFixture reads the accepted logout contract.
func readLogoutFixture(t *testing.T) logoutFixture {
	t.Helper()

	contents, err := os.ReadFile(logoutFixturePath)
	if err != nil {
		t.Fatalf("read logout fixture: %v", err)
	}
	fixture := logoutFixture{}
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatalf("decode logout fixture: %v", err)
	}
	return fixture
}

// Each of a Sender's own credential sources — its management and upload access
// credentials in the Authorization header and its renewal credential in the
// body — revokes exactly that Sender's whole family. The registration and its
// Pairings survive, other installations keep working, and the same Sender signs
// in again afterwards.
func TestLogoutRevokesOnlyThePresentingSendersFamily(t *testing.T) {
	environment := startLogin(t)
	for _, openID := range []string{"managed", "uploaded", "renewed", "bystander"} {
		environment.provider.Accept("code-"+openID, identity.Identity{AppID: "wx-app", OpenID: "openid-" + openID})
	}
	managed := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-managed"}`)
	uploaded := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-uploaded"}`)
	renewed := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-renewed"}`)
	bystander := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-bystander"}`)

	// Pairing activation arrives in a later slice, so the association is seeded
	// directly to prove ordinary logout does not remove it.
	environment.exec(t, `INSERT INTO pin_glide.identity_receiver_registrations (id, state)
		VALUES ('receiver-logout', 'pending')`)
	environment.exec(t, `INSERT INTO pin_glide.pairing_pairings (id, sender_id, receiver_id, state)
		VALUES ('pairing-logout', $1, 'receiver-logout', 'pending')`, managed.SenderID)

	for _, source := range []struct {
		name      string
		presented bundle
		bearer    string
		body      string
	}{
		{name: "management credential", presented: managed, bearer: managed.UserSession.Token},
		{name: "upload credential", presented: uploaded, bearer: uploaded.Uploader.Token},
		{name: "renewal credential", presented: renewed, body: logoutRefreshBody(renewed.Refresh.Token)},
	} {
		logoutOK(t, environment, source.bearer, source.body)

		for _, credential := range []struct {
			token string
			scope identity.Scope
		}{
			{token: source.presented.UserSession.Token, scope: identity.ScopeUserSession},
			{token: source.presented.Uploader.Token, scope: identity.ScopeUploader},
			{token: source.presented.Refresh.Token, scope: identity.ScopeRefresh},
		} {
			noAuthority(t, environment, credential.token, credential.scope)
		}

		// An installation that never logs out keeps its access throughout.
		authorizes(t, environment, bystander.UserSession.Token, identity.ScopeUserSession)
	}

	// Logout revokes credentials, not the installation: the registration stays
	// active with its Pairings and is restored by a later login.
	if closed := environment.count(t, `SELECT count(*) FROM pin_glide.identity_sender_registrations WHERE state = 'revoked'`); closed != 0 {
		t.Errorf("logout permanently closed %d Sender registrations", closed)
	}
	if pairings := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE sender_id = $1`, managed.SenderID); pairings != 1 {
		t.Errorf("logout left the Sender %d Pairings, want its seeded one", pairings)
	}
	environment.provider.Accept("code-managed-again", identity.Identity{AppID: "wx-app", OpenID: "openid-managed"})
	restored := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-managed-again","senderId":"`+managed.SenderID+`"}`)
	if restored.SenderID != managed.SenderID {
		t.Errorf("login restored sender %q, want %q", restored.SenderID, managed.SenderID)
	}
}

// Access expiry never requires a renewal before self-revocation: the renewal
// credential alone authorizes logout, and logout issues nothing.
func TestLogoutAcceptsExpiredAccessWithAValidRefreshCredential(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-expired", identity.Identity{AppID: "wx-app", OpenID: "openid-expired"})
	issued := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-expired"}`)

	environment.exec(t, `UPDATE pin_glide.identity_credentials
		SET expires_at = now() - interval '1 second'
		WHERE sender_id = $1 AND scope <> 'refresh'`, issued.SenderID)
	if _, err := identity.NewService(environment.pool, nil).Authenticate(
		context.Background(), issued.UserSession.Token, identity.ScopeUserSession); !errors.Is(err, identity.ErrCredentialExpired) {
		t.Fatalf("the expired management credential reports %v, want credential_expired", err)
	}

	stored := environment.count(t, `SELECT count(*) FROM pin_glide.identity_credentials WHERE sender_id = $1`, issued.SenderID)
	logoutOK(t, environment, "", logoutRefreshBody(issued.Refresh.Token))
	if issuedNow := environment.count(t, `SELECT count(*) FROM pin_glide.identity_credentials WHERE sender_id = $1`, issued.SenderID); issuedNow != stored {
		t.Errorf("logout issued %d credentials, want none", issuedNow-stored)
	}

	for _, credential := range []struct {
		token string
		scope identity.Scope
	}{
		{token: issued.UserSession.Token, scope: identity.ScopeUserSession},
		{token: issued.Uploader.Token, scope: identity.ScopeUploader},
		{token: issued.Refresh.Token, scope: identity.ScopeRefresh},
	} {
		noAuthority(t, environment, credential.token, credential.scope)
	}
}

// Ordinary logout accepts exactly one usable credential source. A request that
// offers two, none, an unknown, a wrongly scoped or an expired one is rejected
// with the accepted status and code and revokes nothing.
func TestLogoutRejectsConflictingUnknownAndUnusableSources(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-sources", identity.Identity{AppID: "wx-app", OpenID: "openid-sources"})
	environment.provider.Accept("code-late-expiry", identity.Identity{AppID: "wx-app", OpenID: "openid-late-expiry"})
	issued := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-sources"}`)
	expiring := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-late-expiry"}`)

	// Two sources state no single authority, so the request is never guessed at.
	logoutFails(t, environment, issued.UserSession.Token, logoutRefreshBody(issued.Refresh.Token),
		http.StatusUnprocessableEntity, "validation_failed")
	// Neither source present, with and without an empty JSON body.
	logoutFails(t, environment, "", "", http.StatusUnauthorized, "credential_missing")
	logoutFails(t, environment, "", "{}", http.StatusUnauthorized, "credential_missing")
	// An unknown credential authorizes nothing, whichever source carries it.
	logoutFails(t, environment, "credential-that-was-never-issued", "",
		http.StatusUnauthorized, "credential_invalid")
	logoutFails(t, environment, "", logoutRefreshBody("credential-that-was-never-issued"),
		http.StatusUnauthorized, "credential_invalid")
	// Each source carries its own scope: a renewal credential is not access
	// authority, and an access credential is not the body's renewal credential.
	logoutFails(t, environment, issued.Refresh.Token, "", http.StatusUnauthorized, "credential_invalid")
	logoutFails(t, environment, "", logoutRefreshBody(issued.Uploader.Token),
		http.StatusUnauthorized, "credential_invalid")
	// Switching an Account removes associations, so a request that presents no
	// management credential in the header cannot ask for it.
	logoutFails(t, environment, "", `{"mode":"switch_account"}`,
		http.StatusUnauthorized, "credential_missing")
	logoutFails(t, environment, "", `{"mode":"switch_account","refreshToken":"`+issued.Refresh.Token+`"}`,
		http.StatusUnauthorized, "credential_missing")
	logoutFails(t, environment, issued.Uploader.Token, `{"mode":"switch_account"}`,
		http.StatusUnauthorized, "credential_invalid")

	// Every rejection left the family that presented them untouched.
	authorizes(t, environment, issued.UserSession.Token, identity.ScopeUserSession)
	authorizes(t, environment, issued.Refresh.Token, identity.ScopeRefresh)

	// An expired source is its own outcome and revokes nothing either.
	environment.exec(t, `UPDATE pin_glide.identity_credentials
		SET expires_at = now() - interval '1 second'
		WHERE sender_id = $1 AND scope = 'refresh'`, expiring.SenderID)
	logoutFails(t, environment, "", logoutRefreshBody(expiring.Refresh.Token),
		http.StatusUnauthorized, "credential_expired")
	authorizes(t, environment, expiring.UserSession.Token, identity.ScopeUserSession)

	// A family this logout already revoked is reported as revoked, so a client
	// learns its own family is gone instead of retrying an unknown credential.
	logoutOK(t, environment, issued.UserSession.Token, "")
	logoutFails(t, environment, issued.UserSession.Token, "", http.StatusUnauthorized, "credential_revoked")
	logoutFails(t, environment, "", logoutRefreshBody(issued.Refresh.Token),
		http.StatusUnauthorized, "credential_revoked")
}

// Logout serializes with renewal on the same Sender: a renewal that committed
// first is the family logout ends, and a logout that committed first leaves a
// revoked family that cannot renew itself.
func TestLogoutSerializesWithRenewal(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-renewal-first", identity.Identity{AppID: "wx-app", OpenID: "openid-renewal-first"})
	environment.provider.Accept("code-logout-first", identity.Identity{AppID: "wx-app", OpenID: "openid-logout-first"})
	renewalFirst := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-renewal-first"}`)
	logoutFirst := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-logout-first"}`)

	// Renewal then logout: the rotated family is the one logout revokes, and the
	// access credentials ordinary rotation kept valid are revoked with it.
	rotated := refreshOK(t, environment, renewalFirst.Refresh.Token, "operation-before-logout", 0)
	logoutOK(t, environment, "", logoutRefreshBody(rotated.Refresh.Token))
	for _, credential := range []struct {
		token string
		scope identity.Scope
	}{
		{token: renewalFirst.UserSession.Token, scope: identity.ScopeUserSession},
		{token: renewalFirst.Uploader.Token, scope: identity.ScopeUploader},
		{token: renewalFirst.Refresh.Token, scope: identity.ScopeRefresh},
		{token: rotated.UserSession.Token, scope: identity.ScopeUserSession},
		{token: rotated.Uploader.Token, scope: identity.ScopeUploader},
		{token: rotated.Refresh.Token, scope: identity.ScopeRefresh},
	} {
		noAuthority(t, environment, credential.token, credential.scope)
	}

	// Logout then renewal: the revoked family authorizes no renewal.
	logoutOK(t, environment, logoutFirst.UserSession.Token, "")
	refreshFails(t, environment, logoutFirst.Refresh.Token, "operation-after-logout", 0,
		http.StatusUnauthorized, "credential_revoked")
}

// A fresh login replaces the credential family of one Sender. A delayed logout
// carrying the replaced family's authority is refused and leaves the family the
// login authenticated untouched, whether it arrives after the login or races it.
func TestLogoutCannotDisturbAFreshlyAuthenticatedFamily(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-delayed", identity.Identity{AppID: "wx-app", OpenID: "openid-delayed"})
	environment.provider.Accept("code-racing", identity.Identity{AppID: "wx-app", OpenID: "openid-racing"})
	delayed := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-delayed"}`)
	racing := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-racing"}`)

	environment.provider.Accept("code-delayed-again", identity.Identity{AppID: "wx-app", OpenID: "openid-delayed"})
	authenticated := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-delayed-again","senderId":"`+delayed.SenderID+`"}`)
	logoutFails(t, environment, delayed.UserSession.Token, "", http.StatusUnauthorized, "credential_revoked")
	logoutFails(t, environment, "", logoutRefreshBody(delayed.Refresh.Token),
		http.StatusUnauthorized, "credential_revoked")
	authorizes(t, environment, authenticated.UserSession.Token, identity.ScopeUserSession)

	// A logout racing a fresh login serializes on the Sender, so the login's
	// family authorizes afterwards either way.
	var wait sync.WaitGroup
	var loggedOutStatus int
	var loggedOutPayload []byte
	var logoutFailure error
	var loginPayload []byte
	var loginFailure error
	wait.Add(2)
	go func() {
		defer wait.Done()
		loggedOutStatus, loggedOutPayload, _, logoutFailure = concurrentPost(
			environment, logoutPath, logoutRefreshBody(racing.Refresh.Token), "")
	}()
	go func() {
		defer wait.Done()
		status, payload, err := concurrentLogin(environment,
			`{"deviceName":"Test Sender","wechatCode":"code-racing","senderId":"`+racing.SenderID+`"}`)
		loginPayload = payload
		switch {
		case err != nil:
			loginFailure = err
		case status != http.StatusOK:
			loginFailure = fmt.Errorf("login status = %d: %s", status, payload)
		}
	}()
	wait.Wait()
	if logoutFailure != nil {
		t.Fatalf("racing logout: %v", logoutFailure)
	}
	if loginFailure != nil {
		t.Fatalf("racing login: %v", loginFailure)
	}

	credentials := identity.NewService(environment.pool, nil)
	fresh := decodeBundle(t, loginPayload)
	if _, err := credentials.Authenticate(context.Background(), fresh.UserSession.Token, identity.ScopeUserSession); err != nil {
		t.Errorf("the logged-in family does not authorize after the race: %v", err)
	}
	switch loggedOutStatus {
	case http.StatusNoContent:
		// The logout committed first, so the login replaced that family.
		if _, err := credentials.Authenticate(context.Background(), racing.UserSession.Token, identity.ScopeUserSession); !errors.Is(err, identity.ErrCredentialInvalid) {
			t.Errorf("the replaced family still authorizes: %v", err)
		}
	case http.StatusUnauthorized:
		rejected := problem{}
		if err := json.Unmarshal(loggedOutPayload, &rejected); err != nil {
			t.Fatalf("decode racing rejection: %v: %s", err, loggedOutPayload)
		}
		if rejected.Code != "credential_revoked" {
			t.Errorf("racing logout code = %q, want credential_revoked", rejected.Code)
		}
	default:
		t.Fatalf("racing logout status = %d, want 204 or 401: %s", loggedOutStatus, loggedOutPayload)
	}
}

// A logout whose revocation does not commit reports the standard failure and
// leaves the family usable, so a client cannot mistake it for remote sign-out.
func TestLogoutRollsBackWhenRevocationDoesNotCommit(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-rollback", identity.Identity{AppID: "wx-app", OpenID: "openid-rollback"})
	issued := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-rollback"}`)

	environment.exec(t, `CREATE FUNCTION pin_glide.identity_credentials_test_reject_revocation() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected credential revocation failure'; END $$`)
	environment.exec(t, `CREATE TRIGGER trg_identity_credentials_test_reject_revocation
		BEFORE UPDATE ON pin_glide.identity_credentials
		FOR EACH ROW EXECUTE FUNCTION pin_glide.identity_credentials_test_reject_revocation()`)

	logoutFails(t, environment, issued.UserSession.Token, "",
		http.StatusServiceUnavailable, "service_unavailable")
	authorizes(t, environment, issued.UserSession.Token, identity.ScopeUserSession)

	environment.exec(t, `DROP TRIGGER trg_identity_credentials_test_reject_revocation ON pin_glide.identity_credentials`)
	environment.exec(t, `DROP FUNCTION pin_glide.identity_credentials_test_reject_revocation()`)

	// Nothing of the failed attempt survived, so the same credential commits the
	// logout on the retry instead of being revoked or reported as superseded.
	logoutOK(t, environment, issued.UserSession.Token, "")
	noAuthority(t, environment, issued.UserSession.Token, identity.ScopeUserSession)
}
