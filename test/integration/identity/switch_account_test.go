//go:build integration

// Account switching is exercised the way a Sender experiences it: one HTTP
// request over the composed service, a disposable real PostgreSQL holding the
// shipped schema, and the faked WeChat boundary.
package identity_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// switchAccountBody is the wire body of one Account switch.
const switchAccountBody = `{"mode":"switch_account"}`

// decodeProblem decodes one whole-request failure payload.
func decodeProblem(t *testing.T, payload []byte) problem {
	t.Helper()

	decoded := problem{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, payload)
	}
	return decoded
}

// associationState reads one Pairing's stored lifecycle state.
func (e *loginEnvironment) associationState(t *testing.T, pairingID string) string {
	t.Helper()

	var state string
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT state FROM pin_glide.pairing_pairings WHERE id = $1`, pairingID).Scan(&state); err != nil {
		t.Fatalf("read association %s: %v", pairingID, err)
	}
	return state
}

// sessionState reads one Pairing Session's stored lifecycle state.
func (e *loginEnvironment) sessionState(t *testing.T, sessionID string) string {
	t.Helper()

	var state string
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT state FROM pin_glide.pairing_sessions WHERE id = $1`, sessionID).Scan(&state); err != nil {
		t.Fatalf("read session %s: %v", sessionID, err)
	}
	return state
}

// registrationState reads one Sender registration's stored lifecycle state.
func (e *loginEnvironment) registrationState(t *testing.T, senderID string) string {
	t.Helper()

	var state string
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT state FROM pin_glide.identity_sender_registrations WHERE id = $1`, senderID).Scan(&state); err != nil {
		t.Fatalf("read registration %s: %v", senderID, err)
	}
	return state
}

// seedPairing registers one Receiver and one association for a Sender. The
// association is seeded directly because Pairing activation arrives in its own
// slice, and what these tests assert is that a credential change removes
// exactly the rows that belong to one Sender.
func (e *loginEnvironment) seedPairing(t *testing.T, receiverID, pairingID, senderID string) {
	t.Helper()

	e.exec(t, `INSERT INTO pin_glide.identity_receiver_registrations (id, state)
		VALUES ($1, 'pending') ON CONFLICT DO NOTHING`, receiverID)
	e.exec(t, `INSERT INTO pin_glide.pairing_pairings (id, sender_id, receiver_id, state)
		VALUES ($1, $2, $3, 'pending')`, pairingID, senderID, receiverID)
}

// switchAccountOK requires one accepted Account switch: an empty 204 that
// returns no credential and is never cacheable.
func switchAccountOK(t *testing.T, environment *loginEnvironment, bearer string) {
	t.Helper()

	status, payload, header := environment.logout(t, bearer, switchAccountBody)
	if status != http.StatusNoContent {
		t.Fatalf("switch status = %d, want %d: %s", status, http.StatusNoContent, payload)
	}
	if len(payload) != 0 {
		t.Fatalf("switch returned a body: %s", payload)
	}
	if cacheControl := header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("switch Cache-Control = %q, want no-store", cacheControl)
	}
}

// seedUnfinishedWork seeds one Sender's claimed Pairing Session together with
// the reservation it holds for the Receiver: the unfinished authorized work a
// credential change exists to cancel.
func (e *loginEnvironment) seedUnfinishedWork(t *testing.T, sessionID, receiverID, pairingID, senderID string) {
	t.Helper()

	e.seedPairing(t, receiverID, pairingID, senderID)
	e.exec(t, `INSERT INTO pin_glide.pairing_sessions
			(id, mode, receiver_id, authorized_sender_id, pending_pairing_id, state, token_digest, expires_at)
		VALUES ($1, 'new_receiver', $2, $3, $4, 'awaiting_confirmation', $5, now() + interval '5 minutes')`,
		sessionID, receiverID, senderID, pairingID, "digest-"+sessionID)
}

// waitForPausedConfirmation waits until the key confirmation is inside the
// pause that holds its session row, so the request it races starts while that
// row is already locked.
func waitForPausedConfirmation(t *testing.T, environment *loginEnvironment) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for environment.count(t, `SELECT count(*) FROM pg_stat_activity WHERE wait_event = 'PgSleep'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the key confirmation never reached its session")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An Account switch removes exactly the switching Sender's own associations
// and revokes its whole family, in one commit: another Sender sharing the
// Receiver keeps its association, the Receiver keeps its registration, and the
// switching installation registers again without the old ID.
func TestAccountSwitchRemovesOnlyThatSendersOwnAssociations(t *testing.T) {
	environment := startLogin(t)
	for _, openID := range []string{"switcher", "bystander"} {
		environment.provider.Accept("code-"+openID, identity.Identity{AppID: "wx-app", OpenID: "openid-" + openID})
	}
	switcher := environment.loginOK(t, `{"wechatCode":"code-switcher"}`)
	bystander := environment.loginOK(t, `{"wechatCode":"code-bystander"}`)
	environment.seedPairing(t, "receiver-shared", "pairing-switcher", switcher.SenderID)
	environment.seedPairing(t, "receiver-shared", "pairing-bystander", bystander.SenderID)

	switchAccountOK(t, environment, switcher.UserSession.Token)

	for _, credential := range []struct {
		token string
		scope identity.Scope
	}{
		{token: switcher.UserSession.Token, scope: identity.ScopeUserSession},
		{token: switcher.Uploader.Token, scope: identity.ScopeUploader},
		{token: switcher.Refresh.Token, scope: identity.ScopeRefresh},
	} {
		noAuthority(t, environment, credential.token, credential.scope)
	}
	if state := environment.associationState(t, "pairing-switcher"); state != "removed" {
		t.Errorf("switching Sender's association state = %q, want removed", state)
	}
	if state := environment.associationState(t, "pairing-bystander"); state != "pending" {
		t.Errorf("the other Sender's association state = %q, want the untouched pending one", state)
	}
	if registered := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_registrations WHERE id = 'receiver-shared'`); registered != 1 {
		t.Errorf("switching removed the shared Receiver registration: %d rows", registered)
	}
	if closed := environment.count(t, `SELECT count(*) FROM pin_glide.identity_sender_registrations WHERE state = 'revoked'`); closed != 0 {
		t.Errorf("an Account switch permanently closed %d Sender registrations", closed)
	}
	authorizes(t, environment, bystander.UserSession.Token, identity.ScopeUserSession)

	// The switched installation registers again without its old ID, so it never
	// inherits the registration the switch left behind.
	environment.provider.Accept("code-switcher-new", identity.Identity{AppID: "wx-app", OpenID: "openid-switcher"})
	registered := environment.loginOK(t, `{"wechatCode":"code-switcher-new"}`)
	if registered.SenderID == switcher.SenderID {
		t.Errorf("the new registration reused the switched Sender ID %q", registered.SenderID)
	}
}

// An Account switch cancels the switching Sender's unfinished authorized
// Pairing work, so the session that held it can never activate an association
// its Sender no longer owns.
func TestAccountSwitchCancelsUnfinishedPairingWork(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-switch-pending", identity.Identity{AppID: "wx-app", OpenID: "openid-switch-pending"})
	switcher := environment.loginOK(t, `{"wechatCode":"code-switch-pending"}`)
	environment.seedUnfinishedWork(t, "session-switch-pending", "receiver-switch-pending",
		"pairing-switch-pending", switcher.SenderID)

	switchAccountOK(t, environment, switcher.UserSession.Token)

	if state := environment.sessionState(t, "session-switch-pending"); state != "canceled" {
		t.Errorf("unfinished session state = %q, want canceled", state)
	}
	if held := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE id = 'pairing-switch-pending'`); held != 0 {
		t.Errorf("the canceled session kept its reservation: %d rows", held)
	}
}

// An Account switch is a management action: a renewal credential and an
// unauthenticated request authorize nothing, and a credential an earlier
// operation already revoked reports its own outcome. None of them removes an
// association or changes a registration.
func TestAccountSwitchRequiresManagementAuthority(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-switch-auth", identity.Identity{AppID: "wx-app", OpenID: "openid-switch-auth"})
	environment.provider.Accept("code-switch-revoked", identity.Identity{AppID: "wx-app", OpenID: "openid-switch-revoked"})
	issued := environment.loginOK(t, `{"wechatCode":"code-switch-auth"}`)
	revoked := environment.loginOK(t, `{"wechatCode":"code-switch-revoked"}`)
	environment.seedPairing(t, "receiver-switch-auth", "pairing-switch-auth", issued.SenderID)
	environment.exec(t, `UPDATE pin_glide.identity_credentials
		SET revoked_at = now() WHERE sender_id = $1`, revoked.SenderID)

	status, payload, _ := environment.logout(t, issued.Refresh.Token, switchAccountBody)
	if status != http.StatusUnauthorized {
		t.Fatalf("renewal-credential switch status = %d, want 401: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "credential_invalid" {
		t.Errorf("renewal-credential switch code = %q, want credential_invalid", code)
	}
	status, payload, _ = environment.logout(t, revoked.UserSession.Token, switchAccountBody)
	if status != http.StatusUnauthorized || decodeProblem(t, payload).Code != "credential_invalid" {
		t.Fatalf("revoked-family switch = %d %s, want 401 credential_invalid", status, payload)
	}

	if state := environment.associationState(t, "pairing-switch-auth"); state != "pending" {
		t.Errorf("a refused switch changed the association state to %q", state)
	}
	authorizes(t, environment, issued.UserSession.Token, identity.ScopeUserSession)
}

// A switch whose cleanup cannot commit reports the standard failure and leaves
// both the credentials and the associations as they were, so a client cannot
// mistake it for achieved local cleanup.
func TestAccountSwitchRollsBackWhenCleanupCannotCommit(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-switch-rollback", identity.Identity{AppID: "wx-app", OpenID: "openid-switch-rollback"})
	issued := environment.loginOK(t, `{"wechatCode":"code-switch-rollback"}`)
	environment.seedPairing(t, "receiver-switch-rollback", "pairing-switch-rollback", issued.SenderID)

	environment.exec(t, `CREATE FUNCTION pin_glide.pairing_pairings_test_reject_removal() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected association removal failure'; END $$`)
	environment.exec(t, `CREATE TRIGGER trg_pairing_pairings_test_reject_removal
		BEFORE UPDATE ON pin_glide.pairing_pairings
		FOR EACH ROW EXECUTE FUNCTION pin_glide.pairing_pairings_test_reject_removal()`)

	logoutFails(t, environment, issued.UserSession.Token, switchAccountBody,
		http.StatusServiceUnavailable, "service_unavailable")
	authorizes(t, environment, issued.UserSession.Token, identity.ScopeUserSession)
	if state := environment.associationState(t, "pairing-switch-rollback"); state != "pending" {
		t.Errorf("a failed switch changed the association state to %q", state)
	}

	environment.exec(t, `DROP TRIGGER trg_pairing_pairings_test_reject_removal ON pin_glide.pairing_pairings`)
	environment.exec(t, `DROP FUNCTION pin_glide.pairing_pairings_test_reject_removal()`)
	switchAccountOK(t, environment, issued.UserSession.Token)
	noAuthority(t, environment, issued.UserSession.Token, identity.ScopeUserSession)
}

// A delayed Account switch cannot disturb a family a fresh login already
// authenticated: the switch presents authority an earlier operation replaced,
// so it is refused and changes nothing.
func TestDelayedAccountSwitchCannotRevokeASuccessorFamily(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-switch-serial", identity.Identity{AppID: "wx-app", OpenID: "openid-switch-serial"})
	issued := environment.loginOK(t, `{"wechatCode":"code-switch-serial"}`)
	environment.seedPairing(t, "receiver-switch-serial", "pairing-switch-serial", issued.SenderID)

	environment.provider.Accept("code-switch-serial-again", identity.Identity{AppID: "wx-app", OpenID: "openid-switch-serial"})
	restored := environment.loginOK(t, `{"wechatCode":"code-switch-serial-again","senderId":"`+issued.SenderID+`"}`)
	status, payload, _ := environment.logout(t, issued.UserSession.Token, switchAccountBody)
	if status != http.StatusUnauthorized || decodeProblem(t, payload).Code != "credential_invalid" {
		t.Fatalf("delayed switch = %d %s, want 401 credential_invalid", status, payload)
	}
	authorizes(t, environment, restored.UserSession.Token, identity.ScopeUserSession)
	if state := environment.associationState(t, "pairing-switch-serial"); state != "pending" {
		t.Errorf("a superseded switch changed the association state to %q", state)
	}
}

// A fresh login replaces the credential family of one Sender, and the family it
// issues survives a switch racing that login: the switch either commits first,
// so the login replaced the family it revoked, or it presents authority the
// login already replaced and revokes nothing. A switch that revoked the
// replacement instead would leave the restored installation without authority.
func TestAccountSwitchCannotDisturbAFreshlyAuthenticatedFamily(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-switch-race", identity.Identity{AppID: "wx-app", OpenID: "openid-switch-race"})
	issued := environment.loginOK(t, `{"wechatCode":"code-switch-race"}`)

	for round := range 3 {
		code := fmt.Sprintf("code-switch-race-%d", round)
		environment.provider.Accept(code, identity.Identity{AppID: "wx-app", OpenID: "openid-switch-race"})

		var wait sync.WaitGroup
		var switchedStatus int
		var switchedPayload []byte
		var switchFailure error
		var loginPayload []byte
		var loginFailure error
		wait.Add(2)
		go func() {
			defer wait.Done()
			switchedStatus, switchedPayload, _, switchFailure = concurrentPost(
				environment, logoutPath, switchAccountBody, issued.UserSession.Token)
		}()
		go func() {
			defer wait.Done()
			status, payload, err := concurrentLogin(environment,
				fmt.Sprintf(`{"wechatCode":%q,"senderId":%q}`, code, issued.SenderID))
			loginPayload = payload
			switch {
			case err != nil:
				loginFailure = err
			case status != http.StatusOK:
				loginFailure = fmt.Errorf("login status = %d: %s", status, payload)
			}
		}()
		wait.Wait()
		if switchFailure != nil {
			t.Fatalf("racing switch: %v", switchFailure)
		}
		if loginFailure != nil {
			t.Fatalf("racing login: %v", loginFailure)
		}
		switch {
		case switchedStatus == http.StatusNoContent || switchedStatus == http.StatusUnauthorized:
		default:
			t.Fatalf("racing switch status = %d, want 204 or 401: %s", switchedStatus, switchedPayload)
		}

		// Whichever committed first, the family the login issued authorizes.
		issued = decodeBundle(t, loginPayload)
		authorizes(t, environment, issued.UserSession.Token, identity.ScopeUserSession)
		authorizes(t, environment, issued.Refresh.Token, identity.ScopeRefresh)
	}
}

// A switch waits for the Sender's unfinished session work before it takes the
// Sender's own lock, which is the order a claim, a completion and an explicit
// removal take those two locks in. The reverse order lets a switch and a key
// confirmation deadlock, which PostgreSQL resolves by aborting one of them as
// unavailable instead of serializing them.
func TestAccountSwitchSerializesWithAnInFlightKeyConfirmation(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-switch-race-lock", identity.Identity{AppID: "wx-app", OpenID: "openid-switch-race-lock"})
	switcher := environment.loginOK(t, `{"wechatCode":"code-switch-race-lock"}`)
	environment.seedUnfinishedWork(t, "session-switch-race-lock", "receiver-switch-race-lock",
		"pairing-switch-race-lock", switcher.SenderID)
	// The Receiver's key is already confirmed, so the Sender's report is the one
	// that activates the pairing and takes the Sender's lock afterwards.
	environment.exec(t, `UPDATE pin_glide.pairing_sessions SET receiver_confirmed_at = now()
		WHERE id = 'session-switch-race-lock'`)
	// The pause sits exactly where a key confirmation has locked its session row
	// and has not taken the Sender's lock yet, which is the overlap a switch has
	// to wait out.
	environment.exec(t, `CREATE FUNCTION pin_glide.pairing_sessions_test_pause_confirmation() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
			IF NEW.sender_confirmed_at IS NOT NULL AND OLD.sender_confirmed_at IS NULL THEN
				PERFORM pg_sleep(2);
			END IF;
			RETURN NEW;
		END $$`)
	environment.exec(t, `CREATE TRIGGER trg_pairing_sessions_test_pause_confirmation
		BEFORE UPDATE ON pin_glide.pairing_sessions
		FOR EACH ROW EXECUTE FUNCTION pin_glide.pairing_sessions_test_pause_confirmation()`)

	var wait sync.WaitGroup
	var confirmedStatus int
	var confirmedPayload []byte
	var confirmedFailure error
	wait.Add(1)
	go func() {
		defer wait.Done()
		confirmedStatus, confirmedPayload, _, confirmedFailure = concurrentPost(environment,
			"/v1/pairing-sessions/session-switch-race-lock/complete",
			`{"keyConfirmation":"confirmed"}`, switcher.UserSession.Token)
	}()
	waitForPausedConfirmation(t, environment)

	switchedStatus, switchedPayload, _, switchFailure := concurrentPost(
		environment, logoutPath, switchAccountBody, switcher.UserSession.Token)
	wait.Wait()
	if switchFailure != nil {
		t.Fatalf("switch racing a key confirmation: %v", switchFailure)
	}
	if switchedStatus != http.StatusNoContent {
		t.Fatalf("switch racing a key confirmation = %d, want %d: %s",
			switchedStatus, http.StatusNoContent, switchedPayload)
	}
	if confirmedFailure != nil {
		t.Fatalf("racing key confirmation: %v", confirmedFailure)
	}
	if confirmedStatus != http.StatusOK {
		t.Fatalf("racing key confirmation = %d, want %d: %s",
			confirmedStatus, http.StatusOK, confirmedPayload)
	}
	if state := environment.associationState(t, "pairing-switch-race-lock"); state != "removed" {
		t.Errorf("the switched Sender's association state = %q, want removed", state)
	}
}
