//go:build integration

// Package pairing_test exercises Pairing Session activation and Receiver
// credential issuance the way a client experiences them: HTTP and real WSS
// requests against the composed service, a disposable real PostgreSQL holding
// the shipped schema, and a fake WeChat boundary.
package pairing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// complete posts one key-confirmation report and decodes the resulting view.
func (e *environment) complete(t *testing.T, sessionID, credential string) (int, sessionView, []byte) {
	t.Helper()

	status, payload := e.post(t, "/v1/pairing-sessions/"+sessionID+"/complete",
		`{"keyConfirmation":"confirmed"}`, credential)
	if status != http.StatusOK {
		return status, sessionView{}, payload
	}
	var view sessionView
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatalf("decode completed session: %v: %s", err, payload)
	}
	return status, view, payload
}

// issuedCredential is the decoded redemption payload.
type issuedCredential struct {
	ReceiverID    string `json:"receiverId"`
	ReceiverToken string `json:"receiverToken"`
}

// redeem posts one credential redemption and decodes its success payload.
func (e *environment) redeem(t *testing.T, sessionID, credential, operationID string, attempt int) (int, issuedCredential, []byte) {
	t.Helper()

	body := fmt.Sprintf(`{"operationId":%q,"attempt":%d}`, operationID, attempt)
	status, payload := e.post(t, "/v1/pairing-sessions/"+sessionID+"/receiver-token", body, credential)
	if status != http.StatusOK {
		return status, issuedCredential{}, payload
	}
	var issued issuedCredential
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode redeemed credential: %v: %s", err, payload)
	}
	if issued.ReceiverID == "" || issued.ReceiverToken == "" {
		t.Fatalf("redeemed credential is incomplete: %s", payload)
	}
	return status, issued, payload
}

// activate reports both sides' key confirmations and returns the ready view.
func (e *environment) activate(t *testing.T, sessionID, senderToken, receiverToken string) sessionView {
	t.Helper()

	if status, _, payload := e.complete(t, sessionID, senderToken); status != http.StatusOK {
		t.Fatalf("sender completion status = %d: %s", status, payload)
	}
	status, view, payload := e.complete(t, sessionID, receiverToken)
	if status != http.StatusOK {
		t.Fatalf("receiver completion status = %d: %s", status, payload)
	}
	return view
}

// readySession drives a new_receiver session to credential_ready and returns
// its created view, restricted credential and claiming Sender.
func (e *environment) readySession(t *testing.T, sender bundle) (sessionView, string) {
	t.Helper()

	created, token := e.createSession(t, "new_receiver", "", "")
	if status, _, payload := e.authorize(t, created.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}
	ready := e.activate(t, created.SessionID, sender.UserSession.Token, token)
	if ready.State != "credential_ready" {
		t.Fatalf("activated session = %+v, want credential_ready", ready)
	}
	return ready, token
}

// postResponse issues one authenticated JSON request and returns its headers
// together with the body, so a response policy such as Cache-Control can be
// asserted where it matters.
func (e *environment) postResponse(t *testing.T, path, body, credential string) (*http.Response, []byte) {
	t.Helper()

	request, err := http.NewRequest(http.MethodPost, e.service.BaseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := e.service.Client.Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	payload, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read POST %s: %v", path, err)
	}
	return response, payload
}

// Both authenticated reports are required before the reservation activates:
// one side alone leaves the Pairing pending, a foreign Sender cannot report,
// and repeating a committed report changes nothing.
func TestPairingActivationRequiresBothAuthenticatedReports(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-activate", "openid-activate")
	foreign := environment.sender(t, "code-foreign", "openid-foreign")

	created, token := environment.createSession(t, "new_receiver", "", "")
	status, claimed, payload := environment.authorize(t, created.SessionID, "", sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}

	if status, _, payload := environment.complete(t, created.SessionID, ""); status != http.StatusUnauthorized {
		t.Fatalf("report without a credential = %d, want 401: %s", status, payload)
	}
	if status, _, payload := environment.complete(t, created.SessionID, foreign.UserSession.Token); status != http.StatusNotFound {
		t.Fatalf("foreign Sender report = %d, want 404: %s", status, payload)
	}

	// One side alone does not activate the association.
	status, reported, payload := environment.complete(t, created.SessionID, token)
	if status != http.StatusOK {
		t.Fatalf("receiver report status = %d: %s", status, payload)
	}
	if reported.State != "awaiting_confirmation" || !reported.ReceiverConfirmed || reported.SenderConfirmed {
		t.Fatalf("one-sided report = %+v, want only the Receiver side confirmed", reported)
	}
	if state := environment.pairingState(t, claimed.PairingID); state != "pending" {
		t.Fatalf("pairing state after one report = %q, want pending", state)
	}

	// Repeating the committed report is idempotent, including its timestamp.
	firstConfirmed := environment.confirmedAt(t, created.SessionID, "receiver_confirmed_at")
	time.Sleep(10 * time.Millisecond)
	if status, repeated, payload := environment.complete(t, created.SessionID, token); status != http.StatusOK ||
		repeated.State != "awaiting_confirmation" {
		t.Fatalf("repeated report = %d %+v: %s", status, repeated, payload)
	}
	if repeated := environment.confirmedAt(t, created.SessionID, "receiver_confirmed_at"); !repeated.Equal(firstConfirmed) {
		t.Errorf("repeated report changed the confirmation time %v -> %v", firstConfirmed, repeated)
	}

	// The second authenticated side activates the reservation.
	status, activated, payload := environment.complete(t, created.SessionID, sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("sender report status = %d: %s", status, payload)
	}
	if activated.State != "credential_ready" || !activated.SenderConfirmed || !activated.ReceiverConfirmed {
		t.Fatalf("both reports = %+v, want an activated credential_ready session", activated)
	}
	if state := environment.pairingState(t, claimed.PairingID); state != "active" {
		t.Fatalf("pairing state after both reports = %q, want active", state)
	}
	if activated := environment.confirmedAt(t, created.SessionID, "sender_confirmed_at"); activated.IsZero() {
		t.Error("activation recorded no Sender confirmation time")
	}

	// Once the session holds its credential grant, a repeated report is still
	// the same committed side and changes nothing.
	if status, repeated, payload := environment.complete(t, created.SessionID, sender.UserSession.Token); status != http.StatusOK ||
		repeated.State != "credential_ready" {
		t.Fatalf("report after activation = %d %+v: %s", status, repeated, payload)
	}
}

// The session hands the first Receiver credential only to the restricted
// credential holder: the Sender side is told the state, never the credential,
// and the response is a no-store payload whose raw value the service stores
// only as a digest.
func TestCredentialReadyNotifiesOnlyTheReceiverAndRedemptionReturnsItOnce(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-redeem", "openid-redeem")

	created, token := environment.createSession(t, "new_receiver", "", "")
	if status, _, payload := environment.authorize(t, created.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}

	receiverSocket := environment.dial(t, created.SessionID, token)
	var snapshot struct {
		Type    string      `json:"type"`
		Session sessionView `json:"session"`
	}
	readFrame(t, receiverSocket, &snapshot)
	senderSocket := environment.dial(t, created.SessionID, sender.UserSession.Token)
	readFrame(t, senderSocket, &snapshot)

	if status, _, payload := environment.complete(t, created.SessionID, sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("sender report status = %d: %s", status, payload)
	}
	readFrame(t, receiverSocket, &snapshot)
	readFrame(t, senderSocket, &snapshot)

	status, ready, payload := environment.complete(t, created.SessionID, token)
	if status != http.StatusOK || ready.State != "credential_ready" {
		t.Fatalf("receiver report = %d %+v: %s", status, ready, payload)
	}

	// Both participants learn the new state; only the Receiver side is told
	// that the credential became redeemable.
	readFrame(t, senderSocket, &snapshot)
	if snapshot.Type != "pairing_state" || snapshot.Session.State != "credential_ready" {
		t.Fatalf("Sender-side state = %+v, want the credential_ready view", snapshot)
	}
	readFrame(t, receiverSocket, &snapshot)
	if snapshot.Type != "pairing_state" || snapshot.Session.State != "credential_ready" {
		t.Fatalf("Receiver-side state = %+v, want the credential_ready view", snapshot)
	}
	var readyFrame struct {
		Type       string    `json:"type"`
		SessionID  string    `json:"sessionId"`
		ReceiverID string    `json:"receiverId"`
		ExpiresAt  time.Time `json:"expiresAt"`
	}
	readFrame(t, receiverSocket, &readyFrame)
	if readyFrame.Type != "credential_ready" || readyFrame.SessionID != created.SessionID ||
		readyFrame.ReceiverID != created.ReceiverID || readyFrame.ExpiresAt.IsZero() {
		t.Fatalf("credential_ready = %+v, want the session's Receiver and expiry", readyFrame)
	}
	assertNoFrame(t, senderSocket, "the Sender side received a credential notification")

	// The Sender's own responses never carry the credential either.
	if strings.Contains(string(payload), "receiverToken") {
		t.Errorf("session view leaked a Receiver credential: %s", payload)
	}

	response, payload := environment.postResponse(t,
		"/v1/pairing-sessions/"+created.SessionID+"/receiver-token",
		`{"operationId":"op-redeem","attempt":0}`, token)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", response.StatusCode, payload)
	}
	if cache := response.Header.Get("Cache-Control"); cache != "no-store" {
		t.Errorf("redemption Cache-Control = %q, want no-store", cache)
	}
	var issued issuedCredential
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode redemption: %v: %s", err, payload)
	}
	if issued.ReceiverID != created.ReceiverID || issued.ReceiverToken == "" {
		t.Fatalf("redeemed credential = %+v, want the session's Receiver", issued)
	}

	// Only a digest is stored, and the session records exactly one issuance.
	if stored := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_credentials
		WHERE token_digest = $1`, issued.ReceiverToken); stored != 0 {
		t.Errorf("%d Receiver credential rows contain the raw credential", stored)
	}
	if stored := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_credentials
		WHERE receiver_id = $1 AND revoked_at IS NULL`, created.ReceiverID); stored != 1 {
		t.Errorf("current Receiver credentials = %d, want 1", stored)
	}
	if state := environment.sessionState(t, created.SessionID); state != "credential_issued" {
		t.Errorf("session state after redemption = %q, want credential_issued", state)
	}

	// A Sender management credential, an unknown credential and another
	// session's restricted credential cannot redeem the grant.
	for _, rejected := range []struct {
		name       string
		credential string
	}{
		{name: "management credential", credential: sender.UserSession.Token},
		{name: "unknown credential", credential: "credential-that-was-never-issued"},
	} {
		if status, _, payload := environment.redeem(t, created.SessionID, rejected.credential, "op-other", 0); status != http.StatusUnauthorized {
			t.Errorf("%s redemption = %d, want 401: %s", rejected.name, status, payload)
		}
	}
}

// One issuance operation orders its attempts: a competing operation and an
// equal or lower attempt are superseded without touching the credential, and
// the permitted higher attempt is the retransmission that recovers a lost
// response by replacing the unclaimed credential.
func TestIssuanceAttemptsAreOrderedAndRetransmitOnce(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-attempts", "openid-attempts")

	created, token := environment.createSession(t, "new_receiver", "", "")
	if status, _, payload := environment.authorize(t, created.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}
	environment.activate(t, created.SessionID, sender.UserSession.Token, token)

	// The original attempt commits, and its retransmission replaces the
	// unclaimed credential with a fresh value.
	status, original, payload := environment.redeem(t, created.SessionID, token, "op-ordered", 0)
	if status != http.StatusOK {
		t.Fatalf("original redemption = %d: %s", status, payload)
	}
	status, retransmitted, payload := environment.redeem(t, created.SessionID, token, "op-ordered", 1)
	if status != http.StatusOK {
		t.Fatalf("retransmission = %d: %s", status, payload)
	}
	if retransmitted.ReceiverToken == original.ReceiverToken {
		t.Error("the retransmission returned the credential whose response was lost")
	}

	// Every later attempt is superseded: the equal and lower attempts of the
	// same operation and any competing operation, at any attempt.
	for _, superseded := range []struct {
		name      string
		operation string
		attempt   int
	}{
		{name: "equal attempt", operation: "op-ordered", attempt: 1},
		{name: "lower attempt", operation: "op-ordered", attempt: 0},
		{name: "competing original", operation: "op-competing", attempt: 0},
		{name: "competing retransmission", operation: "op-competing", attempt: 1},
	} {
		status, _, payload := environment.redeem(t, created.SessionID, token, superseded.operation, superseded.attempt)
		if status != http.StatusConflict {
			t.Errorf("%s status = %d, want 409: %s", superseded.name, status, payload)
			continue
		}
		if code := decodeProblem(t, payload).Code; code != "operation_superseded" {
			t.Errorf("%s code = %q, want operation_superseded", superseded.name, code)
		}
	}

	// The superseded attempt changed nothing on the wire either.
	if status, _, payload := environment.redeem(t, created.SessionID, token, "op-ordered", 1); status != http.StatusConflict {
		t.Fatalf("repeated retransmission = %d, want 409: %s", status, payload)
	}
	if stored := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_credentials
		WHERE receiver_id = $1 AND revoked_at IS NULL`, created.ReceiverID); stored != 1 {
		t.Errorf("current Receiver credentials = %d, want 1", stored)
	}
}

// An attempt outside the adopted 0/1 protocol and a request without an
// operation ID are rejected as validation failures.
func TestIssuanceRejectsUnsupportedAttempts(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-attempt-shape", "openid-attempt-shape")
	created, token := environment.readySession(t, sender)

	for _, body := range []string{
		`{"operationId":"op-shape","attempt":2}`,
		`{"operationId":"","attempt":0}`,
		`{"attempt":0}`,
	} {
		status, payload := environment.post(t, "/v1/pairing-sessions/"+created.SessionID+"/receiver-token", body, token)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("redemption %s = %d, want 422: %s", body, status, payload)
		}
	}
}

// pairingState reads one association's stored state.
func (e *environment) pairingState(t *testing.T, pairingID string) string {
	t.Helper()
	if pairingID == "" {
		t.Fatal("the session allocated no Pairing ID")
	}
	var state string
	if err := e.pool.QueryRow(
		`SELECT state FROM pin_glide.pairing_pairings WHERE id = $1`, pairingID).Scan(&state); err != nil {
		t.Fatalf("read pairing state: %v", err)
	}
	return state
}

// sessionState reads one session's stored state.
func (e *environment) sessionState(t *testing.T, sessionID string) string {
	t.Helper()
	var state string
	if err := e.pool.QueryRow(
		`SELECT state FROM pin_glide.pairing_sessions WHERE id = $1`, sessionID).Scan(&state); err != nil {
		t.Fatalf("read session state: %v", err)
	}
	return state
}

// confirmedAt reads one recorded confirmation timestamp.
func (e *environment) confirmedAt(t *testing.T, sessionID, column string) time.Time {
	t.Helper()
	var recorded time.Time
	if err := e.pool.QueryRow(
		`SELECT `+column+` FROM pin_glide.pairing_sessions WHERE id = $1`, sessionID).Scan(&recorded); err != nil {
		t.Fatalf("read %s: %v", column, err)
	}
	return recorded
}

// assertNoFrame requires that no application frame arrives inside the short
// observation window, so a control message sent to the wrong participant is
// reported instead of inferred.
func assertNoFrame(t *testing.T, socket *websocket.Conn, description string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, data, err := socket.Read(ctx); err == nil {
		t.Errorf("%s: %s", description, data)
	}
}
