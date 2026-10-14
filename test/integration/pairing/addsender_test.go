//go:build integration

// Package pairing_test exercises attaching one more Sender to an already
// credentialed Receiver and replacing one Sender's own Pairing the way a
// client experiences them: HTTP and real WSS requests against the composed
// service, a disposable real PostgreSQL holding the shipped schema, and a fake
// WeChat boundary.
package pairing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// addSender opens one add_sender session with the credential the Receiver
// already holds and returns its restricted credential.
func (e *environment) addSender(t *testing.T, receiverID, credential string) (sessionView, string) {
	t.Helper()

	status, payload := e.post(t, "/v1/pairing-sessions",
		`{"mode":"add_sender","receiverId":"`+receiverID+`"}`, credential)
	if status != http.StatusCreated {
		t.Fatalf("create add_sender session status = %d, want 201: %s", status, payload)
	}
	var created struct {
		Session      sessionView `json:"session"`
		PairingToken string      `json:"pairingToken"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode created add_sender session: %v: %s", err, payload)
	}
	if created.Session.Mode != "add_sender" || created.Session.ReceiverID != receiverID || created.PairingToken == "" {
		t.Fatalf("created add_sender session is incomplete: %s", payload)
	}
	return created.Session, created.PairingToken
}

// paired drives one new_receiver session to an active association and returns
// that association and the credential its delivery connection uses.
func (e *environment) paired(t *testing.T, sender bundle, alias string) (sessionView, issuedCredential) {
	t.Helper()

	claimed, token := e.claimNewReceiver(t, sender, alias)
	if ready := e.activate(t, claimed.SessionID, sender.UserSession.Token, token); ready.State != "credential_ready" {
		t.Fatalf("activated session = %+v, want credential_ready", ready)
	}
	status, issued, payload := e.redeem(t, claimed.SessionID, token, "op-pair-"+claimed.SessionID, 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}
	return claimed, issued
}

// association is the stored state of one Pairing, the owning Sender's own
// alias, and the association its reservation expects to replace.
type association struct {
	state    string
	alias    string
	replaces string
}

// associationOf reads one Pairing's stored facts.
func (e *environment) associationOf(t *testing.T, pairingID string) association {
	t.Helper()
	if pairingID == "" {
		t.Fatal("the session allocated no Pairing ID")
	}

	var row association
	var replaces *string
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT state, alias, replaces_pairing_id FROM pin_glide.pairing_pairings WHERE id = $1`,
		pairingID).Scan(&row.state, &row.alias, &replaces); err != nil {
		t.Fatalf("read association %s: %v", pairingID, err)
	}
	if replaces != nil {
		row.replaces = *replaces
	}
	return row
}

// currentCredential reads the number and the highest generation of a
// Receiver's current credentials.
func (e *environment) currentCredential(t *testing.T, receiverID string) (int64, int64) {
	t.Helper()

	var current int64
	var generation *int64
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT count(*), max(generation) FROM pin_glide.identity_receiver_credentials
		WHERE receiver_id = $1 AND revoked_at IS NULL`, receiverID).Scan(&current, &generation); err != nil {
		t.Fatalf("read Receiver credentials: %v", err)
	}
	if generation == nil {
		return current, 0
	}
	return current, *generation
}

// add_sender creation is authorized by the credential the Receiver already
// holds: a request without it, with an unrelated value or with a target that
// value does not authenticate never opens a session, so only the Receiver that
// proved possession of its own credential can offer one more Sender to it.
func TestAddSenderCreationAuthenticatesTheReceiversCurrentCredential(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-add-create", "openid-add-create")
	held, issued := environment.paired(t, sender, "Phone")

	body := `{"mode":"add_sender","receiverId":"` + held.ReceiverID + `"}`
	for _, refused := range []struct {
		name       string
		credential string
		code       string
	}{
		{name: "no credential", code: "credential_missing"},
		{name: "unknown credential", credential: "credential-that-was-never-issued", code: "credential_invalid"},
	} {
		status, payload := environment.post(t, "/v1/pairing-sessions", body, refused.credential)
		if status != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401: %s", refused.name, status, payload)
			continue
		}
		if code := decodeProblem(t, payload).Code; code != refused.code {
			t.Errorf("%s code = %q, want %q", refused.name, code, refused.code)
		}
	}

	// A credential authenticates exactly the Receiver it was issued for, so a
	// different Receiver stays invisible to its holder.
	other, _ := environment.createSession(t, "new_receiver", "", "")
	if status, _, payload := environment.authorize(t, other.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim of the other Receiver = %d: %s", status, payload)
	}
	status, payload := environment.post(t, "/v1/pairing-sessions",
		`{"mode":"add_sender","receiverId":"`+other.ReceiverID+`"}`, issued.ReceiverToken)
	if status != http.StatusNotFound {
		t.Fatalf("add_sender for a Receiver the credential does not authenticate = %d, want 404: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "resource_not_found" {
		t.Errorf("target mismatch code = %q, want resource_not_found", code)
	}

	// The Receiver's own credential creates the session anonymously: it fixes
	// the target and the restricted credential, not the claiming Sender.
	session, token := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	if session.State != "awaiting_authorization" || session.SenderID != "" || session.PairingID != "" {
		t.Errorf("created add_sender session = %+v, want an unclaimed awaiting_authorization session", session)
	}
	if token == issued.ReceiverToken {
		t.Error("creation handed the Receiver's own credential back as the restricted one")
	}
}

// The credential a Receiver presents proves possession of the generation it
// was issued for, so creating an add_sender session consumes the issuance
// grant that is still outstanding and the grant cannot rotate the credential
// afterwards.
func TestAddSenderCreationConsumesAnOutstandingIssuanceGrant(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-add-grant", "openid-add-grant")

	created, token := environment.createSession(t, "new_receiver", "", "")
	if status, _, payload := environment.authorize(t, created.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}
	environment.activate(t, created.SessionID, sender.UserSession.Token, token)
	status, issued, payload := environment.redeem(t, created.SessionID, token, "op-add-grant", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}
	if state := environment.sessionState(t, created.SessionID); state != "credential_issued" {
		t.Fatalf("session state after redemption = %q, want credential_issued", state)
	}

	if _, addSenderToken := environment.addSender(t, issued.ReceiverID, issued.ReceiverToken); addSenderToken == "" {
		t.Fatal("add_sender creation returned no restricted credential")
	}
	if state := environment.sessionState(t, created.SessionID); state != "completed" {
		t.Errorf("session state after its credential created a session = %q, want completed", state)
	}
	if status, _, payload := environment.redeem(t, created.SessionID, token, "op-add-grant", 1); status != http.StatusGone {
		t.Errorf("redemption of the consumed grant = %d, want 410: %s", status, payload)
	}
	if current, _ := environment.currentCredential(t, issued.ReceiverID); current != 1 {
		t.Errorf("current Receiver credentials = %d, want the one the grant issued", current)
	}

	// The consumed grant says nothing about the credential itself: the
	// Receiver still opens its delivery connection with it.
	receiverConnected(t, environment, issued.ReceiverToken)
}

// A second Sender of the owning Account attaches to a Receiver that already
// serves another Sender. Both participants confirm over their own live
// channel, the new Pairing activates for the claiming Sender alone, and the
// Receiver keeps its credential, its delivery connection and the existing
// association of the other Sender.
func TestAddSenderAttachesOneMoreSenderWithoutDisturbingSharedAccess(t *testing.T) {
	environment := start(t)
	first := environment.sender(t, "code-shared-first", "openid-shared")
	second := environment.sender(t, "code-shared-second", "openid-shared")
	foreign := environment.sender(t, "code-shared-foreign", "openid-shared-foreign")

	held, issued := environment.paired(t, first, "Phone")
	delivery := receiverConnected(t, environment, issued.ReceiverToken)
	current, generation := environment.currentCredential(t, issued.ReceiverID)

	created, token := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	if status, _, payload := environment.authorize(t, created.SessionID, "", foreign.UserSession.Token); status != http.StatusNotFound {
		t.Fatalf("claim by another Account = %d, want 404: %s", status, payload)
	}

	status, claimed, payload := environment.authorize(t, created.SessionID, "Laptop", second.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}
	if claimed.State != "awaiting_confirmation" || claimed.PairingID == "" || claimed.PairingID == held.PairingID {
		t.Fatalf("claimed session = %+v, want a new pending Pairing next to %q", claimed, held.PairingID)
	}

	// Both sides hold their own live channel while the key confirmation
	// travels: the claiming Sender's management credential and the Receiver's
	// restricted one.
	var frame struct {
		Type    string      `json:"type"`
		Side    string      `json:"side"`
		Payload string      `json:"payload"`
		Session sessionView `json:"session"`
	}
	senderSocket := environment.dial(t, created.SessionID, second.UserSession.Token)
	readFrame(t, senderSocket, &frame)
	receiverSocket := environment.dial(t, created.SessionID, token)
	readFrame(t, receiverSocket, &frame)

	writeFrame(t, senderSocket, relayFrame(created.SessionID, "cGFpci1pbml0"))
	readFrame(t, receiverSocket, &frame)
	if frame.Type != "pairing_relay" || frame.Side != "sender" || frame.Payload != "cGFpci1pbml0" {
		t.Fatalf("relayed frame = %+v, want the Sender's opaque payload", frame)
	}

	if status, completed, payload := environment.complete(t, created.SessionID, second.UserSession.Token); status != http.StatusOK {
		t.Fatalf("Sender report status = %d: %s", status, payload)
	} else if completed.State != "awaiting_confirmation" {
		t.Fatalf("one-sided report = %+v, want awaiting_confirmation", completed)
	}
	if state := environment.associationOf(t, claimed.PairingID).state; state != "pending" {
		t.Fatalf("Pairing state after one report = %q, want pending", state)
	}
	status, completed, payload := environment.complete(t, created.SessionID, token)
	if status != http.StatusOK {
		t.Fatalf("Receiver report status = %d: %s", status, payload)
	}
	if completed.State != "completed" || !completed.SenderConfirmed || !completed.ReceiverConfirmed {
		t.Fatalf("both reports = %+v, want a completed session without credential issue", completed)
	}
	if state := environment.associationOf(t, claimed.PairingID).state; state != "active" {
		t.Errorf("new Pairing state = %q, want active", state)
	}

	// The other Sender's association, the Receiver's credential and its
	// delivery connection are untouched.
	if existing := environment.associationOf(t, held.PairingID); existing.state != "active" || existing.alias != "Phone" {
		t.Errorf("existing association = %+v, want the unchanged active Phone association", existing)
	}
	if now, nowGeneration := environment.currentCredential(t, issued.ReceiverID); now != current || nowGeneration != generation {
		t.Errorf("Receiver credentials = %d generation %d, want %d generation %d", now, nowGeneration, current, generation)
	}
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusConflict, "receiver_already_connected")
	readTimeout, cancelRead := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelRead()
	if _, _, err := delivery.Read(readTimeout); err == nil {
		t.Error("the shared delivery connection received unexpected traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("the shared delivery connection was closed: %v", err)
	}

	// An add_sender session never issues or replaces a Receiver credential.
	status, payload = environment.post(t, "/v1/pairing-sessions/"+created.SessionID+"/receiver-token",
		`{"operationId":"op-add-sender","attempt":0}`, token)
	if status != http.StatusConflict {
		t.Fatalf("redemption through an add_sender session = %d, want 409: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "mode_forbids_credential_issue" {
		t.Errorf("redemption code = %q, want mode_forbids_credential_issue", code)
	}
	if now, nowGeneration := environment.currentCredential(t, issued.ReceiverID); now != current || nowGeneration != generation {
		t.Errorf("forbidden redemption changed credentials: %d generation %d", now, nowGeneration)
	}
}

// Re-pairing reserves its own Pairing ID next to the working association: the
// old association stays active with the Sender's own alias until both
// confirmations atomically retire it, and a failed or lapsed replacement
// releases only its reservation.
func TestRePairingReplacesTheAssociationOnlyAfterBothConfirmations(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-replace", "openid-replace")
	held, issued := environment.paired(t, sender, "Phone")

	// A replacement reserves its own Pairing ID and inherits the Sender's own
	// alias unless the claim supplies one.
	created, token := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	if status, _, payload := environment.authorize(t, created.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}
	status, reserved, payload := environment.authorize(t, created.SessionID, "", sender.UserSession.Token)
	if status != http.StatusOK || reserved.PairingID == held.PairingID || reserved.PairingID == "" {
		t.Fatalf("repeated claim = %d %+v, want one new reserved Pairing ID: %s", status, reserved, payload)
	}
	reservation := environment.associationOf(t, reserved.PairingID)
	if reservation.state != "pending" || reservation.replaces != held.PairingID || reservation.alias != "Phone" {
		t.Fatalf("reservation = %+v, want a pending replacement of %q that keeps the alias", reservation, held.PairingID)
	}
	if existing := environment.associationOf(t, held.PairingID); existing.state != "active" {
		t.Fatalf("working association = %+v, want it still active", existing)
	}

	// One report does not commit anything, and canceling the replacement
	// releases its reservation only.
	if status, reported, payload := environment.complete(t, created.SessionID, token); status != http.StatusOK || reported.State != "awaiting_confirmation" {
		t.Fatalf("one-sided report = %d %+v: %s", status, reported, payload)
	}
	if state := environment.associationOf(t, reserved.PairingID).state; state != "pending" {
		t.Errorf("reservation state after one report = %q, want pending", state)
	}
	if status, payload := environment.request(t, http.MethodDelete,
		"/v1/pairing-sessions/"+created.SessionID, "", sender.UserSession.Token); status != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204: %s", status, payload)
	}
	if existing := environment.associationOf(t, held.PairingID); existing.state != "active" {
		t.Errorf("canceled replacement changed the working association: %+v", existing)
	}
	if count := environment.count(t,
		`SELECT count(*) FROM pin_glide.pairing_pairings WHERE sender_id = $1 AND receiver_id = $2`,
		sender.SenderID, held.ReceiverID); count != 1 {
		t.Errorf("canceled replacement left %d associations, want the working one", count)
	}

	// A replacement that lapses releases its reservation the same way.
	lapsed, lapsedToken := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	status, lapsedClaim, payload := environment.authorize(t, lapsed.SessionID, "", sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("lapsing claim status = %d: %s", status, payload)
	}
	environment.exec(t, `UPDATE pin_glide.pairing_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, lapsed.SessionID)
	if status, _, payload := environment.complete(t, lapsed.SessionID, lapsedToken); status != http.StatusUnauthorized {
		t.Errorf("lapsed restricted credential report = %d, want 401: %s", status, payload)
	}
	if status, _, payload := environment.complete(t, lapsed.SessionID, sender.UserSession.Token); status != http.StatusGone {
		t.Errorf("lapsed session report from its Sender = %d, want 410: %s", status, payload)
	}
	if existing := environment.associationOf(t, held.PairingID); existing.state != "active" {
		t.Errorf("lapsed replacement changed the working association: %+v", existing)
	}
	if state := environment.associationOf(t, lapsedClaim.PairingID).state; state != "pending" {
		t.Errorf("lapsed reservation state = %q, want the reservation never committed", state)
	}

	// Both confirmations commit the replacement: the Sender's own alias is
	// replaced when the claim supplies one, and only the expected association
	// is retired.
	replaced, replacedToken := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	status, committed, payload := environment.authorize(t, replaced.SessionID, "Work", sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("replacement claim status = %d: %s", status, payload)
	}
	if status, _, payload := environment.complete(t, replaced.SessionID, sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("Sender report status = %d: %s", status, payload)
	}
	status, completed, payload := environment.complete(t, replaced.SessionID, replacedToken)
	if status != http.StatusOK || completed.State != "completed" {
		t.Fatalf("receiver report = %d %+v: %s", status, completed, payload)
	}
	if existing := environment.associationOf(t, held.PairingID); existing.state != "removed" {
		t.Errorf("replaced association = %+v, want it retired", existing)
	}
	replacement := environment.associationOf(t, committed.PairingID)
	if replacement.state != "active" || replacement.alias != "Work" {
		t.Errorf("replacement association = %+v, want the active Work association", replacement)
	}
	if count := environment.count(t,
		`SELECT count(*) FROM pin_glide.pairing_pairings
		WHERE sender_id = $1 AND receiver_id = $2 AND state = 'active'`,
		sender.SenderID, held.ReceiverID); count != 1 {
		t.Errorf("active associations for one Receiver = %d, want exactly the replacement", count)
	}
}

// Two replacements of the same association cannot both commit: the one whose
// expected association already changed is reported as superseded, releases its
// own reservation and leaves the association that won untouched.
func TestStaleConcurrentReplacementIsSuperseded(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-stale-replacement", "openid-stale-replacement")
	held, issued := environment.paired(t, sender, "Phone")

	left, leftToken := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	right, rightToken := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	var sessions [2]sessionView
	for index, session := range []sessionView{left, right} {
		status, claimed, payload := environment.authorize(t, session.SessionID, "", sender.UserSession.Token)
		if status != http.StatusOK {
			t.Fatalf("claim %d status = %d: %s", index, status, payload)
		}
		sessions[index] = claimed
		if existing := environment.associationOf(t, claimed.PairingID); existing.replaces != held.PairingID {
			t.Fatalf("reservation %d = %+v, want it to replace %q", index, existing, held.PairingID)
		}
	}

	environment.activate(t, left.SessionID, sender.UserSession.Token, leftToken)
	if state := environment.associationOf(t, sessions[0].PairingID).state; state != "active" {
		t.Fatalf("committed replacement state = %q, want active", state)
	}

	// The other replacement reports both sides, but its expected association
	// changed before it committed.
	if status, _, payload := environment.complete(t, right.SessionID, sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("stale Sender report status = %d: %s", status, payload)
	}
	status, _, payload := environment.complete(t, right.SessionID, rightToken)
	if status != http.StatusConflict {
		t.Fatalf("stale replacement status = %d, want 409: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "operation_superseded" {
		t.Errorf("stale replacement code = %q, want operation_superseded", code)
	}
	if state := environment.sessionState(t, right.SessionID); state != "superseded" {
		t.Errorf("stale session state = %q, want superseded", state)
	}
	if count := environment.count(t,
		`SELECT count(*) FROM pin_glide.pairing_pairings WHERE id = $1`, sessions[1].PairingID); count != 0 {
		t.Errorf("stale replacement left %d reservations, want it released", count)
	}

	// The committed replacement is the Sender's one active association, and
	// the Receiver's credential is still the one its connection uses.
	if state := environment.associationOf(t, sessions[0].PairingID).state; state != "active" {
		t.Errorf("winning replacement state = %q, want active", state)
	}
	if count := environment.count(t,
		`SELECT count(*) FROM pin_glide.pairing_pairings
		WHERE sender_id = $1 AND receiver_id = $2 AND state = 'active'`,
		sender.SenderID, held.ReceiverID); count != 1 {
		t.Errorf("active associations = %d, want the winning replacement only", count)
	}
	receiverConnected(t, environment, issued.ReceiverToken)
}
