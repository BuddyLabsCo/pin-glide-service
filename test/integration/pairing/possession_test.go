//go:build integration

package pairing_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialReceiver opens one R-authenticated delivery subscription. A rejected
// handshake returns the problem response of the refusal.
func dialReceiver(t *testing.T, environment *environment, credential string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(environment.service.BaseURL, "http") + "/v1/receiver/events"
	socket, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + credential}},
		Subprotocols: []string{"pin-glide.v1"},
	})
	if err != nil {
		return nil, response, err
	}
	socket.SetReadLimit(64 * 1024)
	return socket, response, nil
}

// receiverConnected requires one credential to open the delivery channel.
func receiverConnected(t *testing.T, environment *environment, credential string) *websocket.Conn {
	t.Helper()

	socket, response, err := dialReceiver(t, environment, credential)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("connect Receiver: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = socket.CloseNow() })
	return socket
}

// receiverRejected requires one credential's handshake to be refused before
// the upgrade with the given problem status and code.
func receiverRejected(t *testing.T, environment *environment, credential string, wantStatus int, wantCode string) {
	t.Helper()

	socket, response, err := dialReceiver(t, environment, credential)
	if err == nil {
		_ = socket.CloseNow()
		t.Fatalf("credential %q opened the delivery channel", credential)
	}
	if response == nil {
		t.Fatalf("rejected handshake has no response: %v", err)
	}
	var body []byte
	if response.Body != nil {
		body, _ = io.ReadAll(response.Body)
		_ = response.Body.Close()
	}
	if response.StatusCode != wantStatus {
		t.Errorf("rejected handshake status = %d, want %d: %s", response.StatusCode, wantStatus, body)
	}
	if code := decodeProblem(t, body).Code; code != wantCode {
		t.Errorf("rejected handshake code = %q, want %q", code, wantCode)
	}
}

// A valid current credential proves possession when it upgrades: the session's
// issuance grant completes, its restricted channel closes, and a late
// redemption can no longer rotate the credential.
func TestReceiverPossessionConsumesTheGrantAndClosesThePairingChannel(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-possession", "openid-possession")

	created, token := environment.createSession(t, "new_receiver", "", "")
	if status, _, payload := environment.authorize(t, created.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}
	pairingSocket := environment.dial(t, created.SessionID, token)
	var snapshot struct {
		Type string `json:"type"`
	}
	readFrame(t, pairingSocket, &snapshot)
	environment.activate(t, created.SessionID, sender.UserSession.Token, token)

	status, issued, payload := environment.redeem(t, created.SessionID, token, "op-possess", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}

	// Possession is proven by the current credential's upgrade, which consumes
	// the grant without a second request.
	receiverSocket := receiverConnected(t, environment, issued.ReceiverToken)
	if state := environment.sessionState(t, created.SessionID); state != "completed" {
		t.Fatalf("session state after possession = %q, want completed", state)
	}

	// The restricted channel reports the final state and then closes once its
	// grant is consumed.
	sawCompleted := false
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := pairingSocket.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("the pairing channel did not close after possession: %v", err)
			}
			break
		}
		var frame struct {
			Type    string      `json:"type"`
			Session sessionView `json:"session"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("decode pairing frame: %v: %s", err, data)
		}
		if frame.Session.State == "completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Error("the pairing channel closed without reporting the completed session")
	}

	// The credential can no longer be rotated once it proved possession.
	status, _, payload = environment.redeem(t, created.SessionID, token, "op-possess", 1)
	if status != http.StatusGone {
		t.Fatalf("late redemption = %d, want 410: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "pairing_session_closed" {
		t.Errorf("late redemption code = %q, want pairing_session_closed", code)
	}

	// A second handshake is refused without evicting the first connection.
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusConflict, "receiver_already_connected")
	ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := receiverSocket.Read(ctx); err == nil {
		t.Error("the first delivery connection received unexpected traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("the first delivery connection was closed: %v", err)
	}
}

// Concurrent first handshakes admit exactly one connection per Receiver; the
// losers receive receiver_already_connected.
func TestConcurrentFirstConnectionsAdmitOneReceiver(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-exclusive", "openid-exclusive")
	ready, token := environment.readySession(t, sender)

	status, issued, payload := environment.redeem(t, ready.SessionID, token, "op-exclusive", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}

	type admission struct {
		socket *websocket.Conn
		status int
	}
	results := make([]admission, 2)
	var wait sync.WaitGroup
	for index := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			socket, response, _ := dialReceiver(t, environment, issued.ReceiverToken)
			results[index] = admission{socket: socket}
			if response != nil {
				results[index].status = response.StatusCode
			}
		}(index)
	}
	wait.Wait()

	admitted := 0
	for index, result := range results {
		if result.socket != nil {
			admitted++
			t.Cleanup(func() { _ = result.socket.CloseNow() })
			continue
		}
		if result.status != http.StatusConflict {
			t.Errorf("concurrent admission %d status = %d, want 409", index, result.status)
		}
	}
	if admitted != 1 {
		t.Fatalf("concurrent admissions = %d, want exactly 1", admitted)
	}
}

// After the permitted retransmission replaces the unclaimed credential, the
// stale value authorizes nothing while the replacement works.
func TestSupersededReceiverCredentialCannotConnect(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-stale", "openid-stale")
	ready, token := environment.readySession(t, sender)

	status, stale, payload := environment.redeem(t, ready.SessionID, token, "op-stale", 0)
	if status != http.StatusOK {
		t.Fatalf("original redemption = %d: %s", status, payload)
	}
	status, current, payload := environment.redeem(t, ready.SessionID, token, "op-stale", 1)
	if status != http.StatusOK {
		t.Fatalf("retransmission = %d: %s", status, payload)
	}

	receiverRejected(t, environment, stale.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	receiverConnected(t, environment, current.ReceiverToken)
}

// Activation commits the Pairing, so a later cancellation reports the closed
// session without undoing the association, and an expired B stops redemption
// while the Pairing stays active.
func TestActivationSurvivesCancellationAndBExpiry(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-survive", "openid-survive")

	canceled, _ := environment.readySession(t, sender)
	if status, payload := environment.request(t, http.MethodDelete,
		"/v1/pairing-sessions/"+canceled.SessionID, "", sender.UserSession.Token); status != http.StatusGone {
		t.Fatalf("cancel after activation = %d, want 410: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "pairing_session_closed" {
		t.Errorf("cancel after activation code = %q, want pairing_session_closed", code)
	}
	if state := environment.pairingState(t, canceled.PairingID); state != "active" {
		t.Errorf("pairing after cancel = %q, want active", state)
	}

	expired, expiredToken := environment.readySession(t, sender)
	environment.exec(t, `UPDATE pin_glide.pairing_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`,
		expired.SessionID)
	status, _, payload := environment.redeem(t, expired.SessionID, expiredToken, "op-expired", 0)
	if status != http.StatusUnauthorized {
		t.Fatalf("redemption after B expiry = %d, want 401: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "credential_expired" {
		t.Errorf("redemption after B expiry code = %q, want credential_expired", code)
	}
	if state := environment.pairingState(t, expired.PairingID); state != "active" {
		t.Errorf("pairing after B expiry = %q, want active", state)
	}
	if status, payload := environment.request(t, http.MethodDelete,
		"/v1/pairing-sessions/"+expired.SessionID, "", sender.UserSession.Token); status != http.StatusGone {
		t.Fatalf("cancel of an expired session = %d, want 410: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "pairing_session_expired" {
		t.Errorf("cancel of an expired session code = %q, want pairing_session_expired", code)
	}
	if state := environment.pairingState(t, expired.PairingID); state != "active" {
		t.Errorf("pairing after expired cancel = %q, want active", state)
	}
}
