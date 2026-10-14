//go:build integration

// Credential renewal is exercised the way a Sender experiences it: ordered
// attempts over the composed HTTP service, a disposable real PostgreSQL
// holding the shipped schema, and the faked WeChat boundary.
package identity_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// refreshPath is the implemented credential renewal operation.
const refreshPath = "/v1/auth/refresh"

// refreshFixturePath is the accepted renewal contract, relative to this
// package.
const refreshFixturePath = "../../fixtures/contract/http/refresh_v1.json"

// refreshFixture is the accepted contract of credential renewal. The composed
// service and the published description are both checked against it, so a
// client-visible rename cannot pass one check and fail the other. Every failure
// status lists the codes the accepted contract defines for it.
type refreshFixture struct {
	OperationID string `json:"operationId"`
	Path        string `json:"path"`
	Method      string `json:"method"`
	Request     struct {
		Schema     string   `json:"schema"`
		Required   []string `json:"required"`
		Properties []string `json:"properties"`
	} `json:"request"`
	Success struct {
		Schema   string   `json:"schema"`
		Status   string   `json:"status"`
		Required []string `json:"required"`
	} `json:"success"`
	Credential struct {
		Schema   string   `json:"schema"`
		Required []string `json:"required"`
	} `json:"credential"`
	Errors map[string][]string `json:"errors"`
}

// refreshResponse is the decoded renewal success payload: the replaced bundle
// plus the operation attempt the service committed.
type refreshResponse struct {
	bundle
	OperationID string `json:"operationId"`
	Attempt     int    `json:"attempt"`
}

// post sends one JSON body to a composed service path and returns the status,
// payload and headers.
func post(t *testing.T, environment *loginEnvironment, path, body string) (int, []byte, http.Header) {
	t.Helper()

	status, payload, header, err := concurrentPost(environment, path, body)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return status, payload, header
}

// refreshBody is the wire body of one ordered renewal attempt.
func refreshBody(refreshToken, operationID string, attempt int) string {
	return fmt.Sprintf(`{"refreshToken":%q,"operationId":%q,"attempt":%d}`, refreshToken, operationID, attempt)
}

// refreshAttempt posts one ordered renewal attempt of one operation.
func refreshAttempt(t *testing.T, environment *loginEnvironment, refreshToken, operationID string, attempt int) (int, []byte, http.Header) {
	t.Helper()

	return post(t, environment, refreshPath, refreshBody(refreshToken, operationID, attempt))
}

// refreshOK requires one accepted renewal and checks it against the accepted
// success, credential and cache-control contract.
func refreshOK(t *testing.T, environment *loginEnvironment, refreshToken, operationID string, attempt int) refreshResponse {
	t.Helper()

	status, payload, header := refreshAttempt(t, environment, refreshToken, operationID, attempt)
	if status != http.StatusOK {
		t.Fatalf("refresh status = %d, want %d: %s", status, http.StatusOK, payload)
	}
	if cacheControl := header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("refresh Cache-Control = %q, want no-store", cacheControl)
	}
	assertRefreshFields(t, payload)

	renewed := decodeRefresh(t, payload)
	if renewed.OperationID != operationID || renewed.Attempt != attempt {
		t.Errorf("refresh committed %q attempt %d, want %q attempt %d",
			renewed.OperationID, renewed.Attempt, operationID, attempt)
	}
	return renewed
}

// refreshFails requires one rejected renewal and checks that the composed
// service reports the status and code the accepted contract defines.
func refreshFails(t *testing.T, environment *loginEnvironment, refreshToken, operationID string, attempt, wantStatus int, wantCode string) problem {
	t.Helper()

	if accepted := readRefreshFixture(t).Errors[strconv.Itoa(wantStatus)]; !slices.Contains(accepted, wantCode) {
		t.Fatalf("the accepted contract defines no %s for status %d", wantCode, wantStatus)
	}

	status, payload, _ := refreshAttempt(t, environment, refreshToken, operationID, attempt)
	if status != wantStatus {
		t.Fatalf("refresh status = %d, want %d: %s", status, wantStatus, payload)
	}

	decoded := problem{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, payload)
	}
	if decoded.Code != wantCode {
		t.Fatalf("refresh code = %q, want %q: %s", decoded.Code, wantCode, payload)
	}
	if decoded.Status != wantStatus {
		t.Fatalf("problem status field = %d, want %d", decoded.Status, wantStatus)
	}
	return decoded
}

// decodeRefresh decodes one renewal success payload.
func decodeRefresh(t *testing.T, payload []byte) refreshResponse {
	t.Helper()

	renewed := refreshResponse{}
	if err := json.Unmarshal(payload, &renewed); err != nil {
		t.Fatalf("decode renewal payload: %v: %s", err, payload)
	}
	return renewed
}

// decodeBundle decodes one credential bundle payload.
func decodeBundle(t *testing.T, payload []byte) bundle {
	t.Helper()

	issued := bundle{}
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode credential bundle: %v: %s", err, payload)
	}
	return issued
}

// assertRefreshFields checks the published field set of the accepted renewal
// contract.
func assertRefreshFields(t *testing.T, payload []byte) {
	t.Helper()

	fixture := readRefreshFixture(t)
	decoded := map[string]any{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode renewal payload: %v: %s", err, payload)
	}
	if fields := sortedKeys(t, decoded); !equalFields(fields, fixture.Success.Required) {
		t.Fatalf("renewal fields = %v, want %v", fields, fixture.Success.Required)
	}
	for _, name := range []string{"userSession", "uploader", "refresh"} {
		credential, ok := decoded[name].(map[string]any)
		if !ok {
			t.Fatalf("renewal %s is not a credential: %v", name, decoded[name])
		}
		if fields := sortedKeys(t, credential); !equalFields(fields, fixture.Credential.Required) {
			t.Fatalf("renewal %s fields = %v, want %v", name, fields, fixture.Credential.Required)
		}
	}
}

// A Sender renews its whole credential family through one operation: the
// current refresh token rotates, the consumed predecessor recovers one lost
// response, and ordinary rotation leaves existing access tokens working.
func TestRefreshRotatesTheBundleAndRecoversOneLostResponse(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-rotate", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-rotate"})
	issued := environment.loginOK(t, `{"wechatCode":"code-rotate"}`)

	// An expired access token never blocks renewal: the refresh token alone
	// authorizes the rotation.
	environment.exec(t, `UPDATE pin_glide.identity_credentials
		SET expires_at = now() - interval '1 second'
		WHERE sender_id = $1 AND scope <> 'refresh'`, issued.SenderID)

	rotatedAt := time.Now().UTC()
	rotated := refreshOK(t, environment, issued.Refresh.Token, "operation-a", 0)
	assertIssuedFamily(t, rotated.bundle, rotatedAt)
	if rotated.AccountID != issued.AccountID || rotated.SenderID != issued.SenderID {
		t.Errorf("renewal moved the Sender: %s/%s, want %s/%s",
			rotated.AccountID, rotated.SenderID, issued.AccountID, issued.SenderID)
	}
	for _, replaced := range []struct{ name, before, after string }{
		{name: "userSession", before: issued.UserSession.Token, after: rotated.UserSession.Token},
		{name: "uploader", before: issued.Uploader.Token, after: rotated.Uploader.Token},
		{name: "refresh", before: issued.Refresh.Token, after: rotated.Refresh.Token},
	} {
		if replaced.before == replaced.after {
			t.Errorf("renewal reused the %s credential", replaced.name)
		}
	}

	// Ordinary rotation revokes nothing: the renewed credentials authorize.
	credentials := identity.NewService(environment.pool, nil)
	ctx := context.Background()
	if _, err := credentials.Authenticate(ctx, rotated.UserSession.Token, identity.ScopeUserSession); err != nil {
		t.Errorf("the renewed management credential does not authorize: %v", err)
	}
	if _, err := credentials.Authenticate(ctx, rotated.Uploader.Token, identity.ScopeUploader); err != nil {
		t.Errorf("the renewed upload credential does not authorize: %v", err)
	}

	// The consumed token recovers one lost response: attempt 1 of the same
	// operation mints a replacement bundle instead of replaying a successor.
	recovered := refreshOK(t, environment, issued.Refresh.Token, "operation-a", 1)
	if recovered.Refresh.Token == rotated.Refresh.Token {
		t.Error("recovery replayed the lost bundle instead of minting a replacement")
	}

	// The accepted replacement carries the family forward.
	continued := refreshOK(t, environment, recovered.Refresh.Token, "operation-b", 0)
	if continued.Refresh.Token == recovered.Refresh.Token {
		t.Error("the family did not advance from the replacement bundle")
	}

	// The family has no absolute age limit: a token issued long before still
	// renews and slides the whole window forward from now.
	environment.exec(t, `UPDATE pin_glide.identity_credentials
		SET issued_at = now() - interval '8 days'
		WHERE sender_id = $1 AND scope = 'refresh' AND revoked_at IS NULL`, issued.SenderID)
	slid := refreshOK(t, environment, continued.Refresh.Token, "operation-slide", 0)
	assertIssuedFamily(t, slid.bundle, time.Now().UTC())

	// Only digests are stored, so no row can hand a caller a raw renewal
	// credential.
	if stored := environment.count(t, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE token_digest IN ($1, $2, $3, $4)`,
		rotated.Refresh.Token, recovered.Refresh.Token, continued.Refresh.Token, slid.Refresh.Token); stored != 0 {
		t.Errorf("%d credential rows contain a raw renewal credential", stored)
	}
}

// The attempt order of one operation decides the outcome: attempt 1 performs
// the rotation when it arrives first, and a recorded attempt never issues again
// or revokes the family, even after later logical renewals.
func TestRefreshKeepsRecordedAttemptsSuperseded(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-order", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-order"})
	issued := environment.loginOK(t, `{"wechatCode":"code-order"}`)

	// Attempt 1 reaches the service first while its parent is still current and
	// performs the original rotation.
	accepted := refreshOK(t, environment, issued.Refresh.Token, "operation-out-of-order", 1)
	credentials := identity.NewService(environment.pool, nil)
	ctx := context.Background()
	if _, err := credentials.Authenticate(ctx, accepted.Refresh.Token, identity.ScopeRefresh); err != nil {
		t.Fatalf("the attempt 1 bundle does not authorize: %v", err)
	}
	// Ordinary rotation revokes nothing: the access credentials the family
	// already held keep working until their own expiry.
	if _, err := credentials.Authenticate(ctx, issued.UserSession.Token, identity.ScopeUserSession); err != nil {
		t.Errorf("ordinary rotation revoked the earlier management credential: %v", err)
	}

	// The delayed attempt 0 is a known lower attempt: superseded without
	// issuance and without revoking the family it would otherwise replay.
	refreshFails(t, environment, issued.Refresh.Token, "operation-out-of-order", 0,
		http.StatusConflict, "operation_superseded")
	if _, err := credentials.Authenticate(ctx, accepted.Refresh.Token, identity.ScopeRefresh); err != nil {
		t.Errorf("a recorded lower attempt revoked the family: %v", err)
	}

	// A later logical renewal advances the family without clearing the guard.
	later := refreshOK(t, environment, accepted.Refresh.Token, "operation-later", 0)
	for _, attempt := range []int{0, 1} {
		refreshFails(t, environment, issued.Refresh.Token, "operation-out-of-order", attempt,
			http.StatusConflict, "operation_superseded")
	}
	if _, err := credentials.Authenticate(ctx, later.Refresh.Token, identity.ScopeRefresh); err != nil {
		t.Errorf("a stale retransmission revoked the family: %v", err)
	}

	// Only the adopted attempts exist: any other attempt is a whole-request
	// validation failure that rotates nothing.
	refreshFails(t, environment, later.Refresh.Token, "operation-attempt", 2,
		http.StatusUnprocessableEntity, "validation_failed")
	// The attempt is required as well: omitting it cannot become attempt 0.
	status, payload, _ := post(t, environment, refreshPath,
		fmt.Sprintf(`{"refreshToken":%q,"operationId":"operation-incomplete"}`, later.Refresh.Token))
	if status != http.StatusUnprocessableEntity {
		t.Errorf("renewal without an attempt status = %d, want %d: %s", status, http.StatusUnprocessableEntity, payload)
	}
	refreshOK(t, environment, later.Refresh.Token, "operation-attempt", 0)
}

// A request whose operation ID is already recorded but whose token no
// operation has consumed is not reuse: the presented token is the family's
// current renewal credential, so it renews the family and revokes nothing.
func TestRefreshRenewsAnUnconsumedTokenWithoutRevoking(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-unconsumed", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-unconsumed"})
	issued := environment.loginOK(t, `{"wechatCode":"code-unconsumed"}`)

	// The successor the recorded operation issued has not been consumed, so
	// the higher attempt of that operation renews the family with it.
	rotated := refreshOK(t, environment, issued.Refresh.Token, "operation-consumed", 0)
	renewed := refreshOK(t, environment, rotated.Refresh.Token, "operation-consumed", 1)
	if renewed.Refresh.Token == rotated.Refresh.Token {
		t.Error("the retransmission did not advance the family")
	}

	// A stale operation ID from the family a fresh login replaced cannot
	// revoke the family that login authenticated either.
	refreshOK(t, environment, renewed.Refresh.Token, "operation-stale", 0)
	environment.provider.Accept("code-unconsumed-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-unconsumed"})
	authenticated := environment.loginOK(t, `{"wechatCode":"code-unconsumed-again","senderId":"`+issued.SenderID+`"}`)
	continued := refreshOK(t, environment, authenticated.Refresh.Token, "operation-stale", 1)
	if continued.AccountID != authenticated.AccountID || continued.SenderID != authenticated.SenderID {
		t.Errorf("renewal moved the Sender: %s/%s, want %s/%s",
			continued.AccountID, continued.SenderID, authenticated.AccountID, authenticated.SenderID)
	}

	credentials := identity.NewService(environment.pool, nil)
	if _, err := credentials.Authenticate(context.Background(), authenticated.UserSession.Token, identity.ScopeUserSession); err != nil {
		t.Errorf("a stale operation revoked the family the login authenticated: %v", err)
	}
}

// Genuine reuse of a spent token revokes the family that token belongs to and
// nothing else.
func TestRefreshRevokesOnlyTheReusedFamily(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-reuse", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-reuse"})
	environment.provider.Accept("code-unrelated", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-unrelated"})
	issued := environment.loginOK(t, `{"wechatCode":"code-reuse"}`)
	unrelated := environment.loginOK(t, `{"wechatCode":"code-unrelated"}`)

	rotated := refreshOK(t, environment, issued.Refresh.Token, "operation-c", 0)

	// A genuinely new operation presenting the consumed token is reuse.
	refreshFails(t, environment, issued.Refresh.Token, "operation-d", 0,
		http.StatusUnauthorized, "refresh_reuse_detected")

	// The revocation covers the whole family, including its current successor.
	refreshFails(t, environment, rotated.Refresh.Token, "operation-e", 0,
		http.StatusUnauthorized, "credential_revoked")

	// Another Sender's family is untouched by that revocation.
	refreshOK(t, environment, unrelated.Refresh.Token, "operation-unrelated", 0)

	// Replay revocation ends a family, not the registration: the same Sender
	// signs in again and receives a new family.
	environment.provider.Accept("code-reuse-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-reuse"})
	restored := environment.loginOK(t, `{"wechatCode":"code-reuse-again","senderId":"`+issued.SenderID+`"}`)
	if restored.SenderID != issued.SenderID {
		t.Errorf("login restored sender %q, want %q", restored.SenderID, issued.SenderID)
	}
}

// Unknown and expired renewal credentials are rejected without guessing or
// revoking the family a caller might claim.
func TestRefreshRejectsUnknownAndExpiredTokensWithoutRevoking(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-expiry", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-expiry"})
	environment.provider.Accept("code-bystander", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-bystander"})
	issued := environment.loginOK(t, `{"wechatCode":"code-expiry"}`)
	bystander := environment.loginOK(t, `{"wechatCode":"code-bystander"}`)

	refreshFails(t, environment, "credential-that-was-never-issued", "operation-unknown", 0,
		http.StatusUnauthorized, "credential_invalid")

	// An expired token cannot revoke its own family, and an unknown token
	// cannot revoke an unrelated one.
	environment.exec(t, `UPDATE pin_glide.identity_credentials
		SET expires_at = now() - interval '1 second'
		WHERE sender_id = $1 AND scope = 'refresh'`, issued.SenderID)
	refreshFails(t, environment, issued.Refresh.Token, "operation-expired", 0,
		http.StatusUnauthorized, "credential_expired")
	refreshFails(t, environment, issued.Refresh.Token, "operation-expired", 1,
		http.StatusUnauthorized, "credential_expired")

	credentials := identity.NewService(environment.pool, nil)
	if _, err := credentials.Authenticate(context.Background(), issued.UserSession.Token, identity.ScopeUserSession); err != nil {
		t.Errorf("an expired token revoked its family: %v", err)
	}
	refreshOK(t, environment, bystander.Refresh.Token, "operation-bystander", 0)
}

// One lost response gets exactly one recovery: the retransmission must arrive
// inside the overlap and before the consumed token's own expiry.
func TestRefreshRecoveryIsBoundedByOverlapAndParentExpiry(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-window", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-window"})
	environment.provider.Accept("code-parent-expiry", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-parent-expiry"})
	windowed := environment.loginOK(t, `{"wechatCode":"code-window"}`)
	expiring := environment.loginOK(t, `{"wechatCode":"code-parent-expiry"}`)

	// Past the overlap the consumed token is genuine reuse of a spent token.
	overlapped := refreshOK(t, environment, windowed.Refresh.Token, "operation-window", 0)
	environment.exec(t, `UPDATE pin_glide.identity_refresh_operations
		SET committed_at = now() - interval '31 seconds'
		WHERE sender_id = $1 AND operation_id = $2`, windowed.SenderID, "operation-window")
	refreshFails(t, environment, windowed.Refresh.Token, "operation-window", 1,
		http.StatusUnauthorized, "refresh_reuse_detected")
	refreshFails(t, environment, overlapped.Refresh.Token, "operation-after-window", 0,
		http.StatusUnauthorized, "credential_revoked")

	// A consumed token that has reached its own expiry is an expired
	// credential instead, and it revokes nothing: the family keeps its
	// current successor.
	aged := refreshOK(t, environment, expiring.Refresh.Token, "operation-aging", 0)
	environment.exec(t, `UPDATE pin_glide.identity_credentials AS credential
		SET expires_at = now() - interval '1 second'
		WHERE credential.sender_id = $1
		AND EXISTS (
			SELECT 1 FROM pin_glide.identity_refresh_operations AS operation
			WHERE operation.parent_digest = credential.token_digest
		)`, expiring.SenderID)
	refreshFails(t, environment, expiring.Refresh.Token, "operation-aging", 1,
		http.StatusUnauthorized, "credential_expired")
	refreshOK(t, environment, aged.Refresh.Token, "operation-after-expiry", 0)
}

// readRefreshFixture reads the accepted renewal contract.
func readRefreshFixture(t *testing.T) refreshFixture {
	t.Helper()

	contents, err := os.ReadFile(refreshFixturePath)
	if err != nil {
		t.Fatalf("read renewal fixture: %v", err)
	}
	fixture := refreshFixture{}
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatalf("decode renewal fixture: %v", err)
	}
	return fixture
}
