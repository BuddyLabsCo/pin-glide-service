//go:build integration

// Account switching is exercised the way a Sender experiences it: one HTTP
// request over the composed service, a disposable real PostgreSQL holding the
// shipped schema, and the faked WeChat boundary.
package identity_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// concurrentSwitch posts one Account switch without using testing.T, so a test
// can race it against another request on the same Sender.
func concurrentSwitch(environment *loginEnvironment, bearer string) (int, []byte, error) {
	request, err := http.NewRequest(http.MethodPost,
		environment.service.BaseURL+logoutPath, strings.NewReader(switchAccountBody))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	response, err := environment.service.Client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, payload, nil
}

// An Account switch removes exactly the switching Sender's own associations
// and revokes its whole family, in one commit: another Sender sharing the
// Receiver keeps its association, the Receiver keeps its registration, and the
// switching installation registers again without the old ID.
func TestAccountSwitchRemovesOnlyThatSendersOwnAssociations(t *testing.T) {
	environment := startLogin(t)
	for _, openID := range []string{"switcher", "bystander"} {
		environment.provider.Accept("code-"+openID, identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-" + openID})
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
	environment.provider.Accept("code-switcher-new", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switcher"})
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
	environment.provider.Accept("code-switch-pending", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-pending"})
	switcher := environment.loginOK(t, `{"wechatCode":"code-switch-pending"}`)
	environment.seedPairing(t, "receiver-switch-pending", "pairing-switch-pending", switcher.SenderID)
	environment.exec(t, `UPDATE pin_glide.pairing_pairings SET state = 'pending' WHERE id = 'pairing-switch-pending'`)
	environment.exec(t, `INSERT INTO pin_glide.pairing_sessions
			(id, mode, receiver_id, authorized_sender_id, pending_pairing_id, state, token_digest, expires_at)
		VALUES ('session-switch-pending', 'new_receiver', 'receiver-switch-pending', $1,
			'pairing-switch-pending', 'awaiting_confirmation', 'digest-switch-pending', now() + interval '5 minutes')`,
		switcher.SenderID)

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
	environment.provider.Accept("code-switch-auth", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-auth"})
	environment.provider.Accept("code-switch-revoked", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-revoked"})
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
	environment.provider.Accept("code-switch-rollback", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-rollback"})
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
	environment.provider.Accept("code-switch-serial", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-serial"})
	issued := environment.loginOK(t, `{"wechatCode":"code-switch-serial"}`)
	environment.seedPairing(t, "receiver-switch-serial", "pairing-switch-serial", issued.SenderID)

	environment.provider.Accept("code-switch-serial-again", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-serial"})
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
	environment.provider.Accept("code-switch-race", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-race"})
	issued := environment.loginOK(t, `{"wechatCode":"code-switch-race"}`)

	for round := range 3 {
		code := fmt.Sprintf("code-switch-race-%d", round)
		environment.provider.Accept(code, identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-race"})

		var wait sync.WaitGroup
		var switchedStatus int
		var switchedPayload []byte
		var switchFailure error
		var loginPayload []byte
		var loginFailure error
		wait.Add(2)
		go func() {
			defer wait.Done()
			switchedStatus, switchedPayload, switchFailure = concurrentSwitch(environment, issued.UserSession.Token)
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
func TestAccountSwitchWaitsForASessionWithoutTheSenderLock(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-switch-order", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-switch-order"})
	switcher := environment.loginOK(t, `{"wechatCode":"code-switch-order"}`)
	environment.seedPairing(t, "receiver-switch-order", "pairing-switch-order", switcher.SenderID)
	environment.exec(t, `UPDATE pin_glide.pairing_pairings SET state = 'pending' WHERE id = 'pairing-switch-order'`)
	environment.exec(t, `INSERT INTO pin_glide.pairing_sessions
			(id, mode, receiver_id, authorized_sender_id, pending_pairing_id, state, token_digest, expires_at)
		VALUES ('session-switch-order', 'new_receiver', 'receiver-switch-order', $1,
			'pairing-switch-order', 'awaiting_confirmation', 'digest-switch-order', now() + interval '5 minutes')`,
		switcher.SenderID)

	ctx := context.Background()
	prober, err := environment.pool.Conn(ctx)
	if err != nil {
		t.Fatalf("take a probe connection: %v", err)
	}
	t.Cleanup(func() { _ = prober.Close() })
	// One completion holds the session row of the work the switch has to wait
	// for, which is what a key confirmation holds while it takes the Sender's
	// own lock afterwards.
	holder, err := environment.pool.Conn(ctx)
	if err != nil {
		t.Fatalf("take a holder connection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = holder.ExecContext(context.Background(), `ROLLBACK`)
		_ = holder.Close()
	})
	if _, err := holder.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("begin the holder transaction: %v", err)
	}
	if _, err := holder.ExecContext(ctx,
		`SELECT id FROM pin_glide.pairing_sessions WHERE id = 'session-switch-order' FOR UPDATE`); err != nil {
		t.Fatalf("hold the session row: %v", err)
	}

	type answer struct {
		status  int
		payload []byte
		err     error
	}
	answered := make(chan answer, 1)
	go func() {
		status, payload, err := concurrentSwitch(environment, switcher.UserSession.Token)
		answered <- answer{status: status, payload: payload, err: err}
	}()

	// The switch waits for that row without taking the Sender's lock, so the
	// lock stays free for the work it waits for.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var acquired bool
		if err := prober.QueryRowContext(ctx,
			`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, switcher.SenderID).Scan(&acquired); err != nil {
			t.Fatalf("probe the Sender's lock: %v", err)
		}
		if !acquired {
			t.Fatalf("the switch took the Sender's lock while it waits for the session that lock protects")
		}
		if _, err := prober.ExecContext(ctx,
			`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, switcher.SenderID); err != nil {
			t.Fatalf("release the probe: %v", err)
		}
		select {
		case got := <-answered:
			t.Fatalf("the switch answered %d while the session it waits for is held: %s", got.status, got.payload)
		case <-time.After(25 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			break
		}
	}

	if _, err := holder.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("release the session row: %v", err)
	}
	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatalf("the waited switch: %v", got.err)
		}
		if got.status != http.StatusNoContent {
			t.Fatalf("the waited switch status = %d, want %d: %s", got.status, http.StatusNoContent, got.payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the switch never answered after the session row was released")
	}
}
