//go:build integration

// Package pairing_test exercises Pairing Session creation, Sender
// authorization and the restricted pairing channel the way a client
// experiences them: HTTP and real WSS requests against the composed service, a
// disposable real PostgreSQL holding the shipped schema, and a fake WeChat
// boundary.
package pairing_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// Independent sessions never leak into each other, a participant cannot relay
// without its own authenticated subscription, and a payload reaches only the
// peer with its bytes and derived side.
func TestPairingChannelRelaysOnlyBetweenItsParticipants(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-relay", "openid-relay")

	created, token := environment.createSession(t, "new_receiver", "", "")
	environment.authorize(t, created.SessionID, "", sender.UserSession.Token)
	other, _ := environment.createSession(t, "new_receiver", "", "")

	receiverSocket := environment.dial(t, created.SessionID, token)
	var snapshot struct {
		Type    string      `json:"type"`
		Session sessionView `json:"session"`
	}
	readFrame(t, receiverSocket, &snapshot)
	if snapshot.Type != "pairing_state" || snapshot.Session.State != "awaiting_confirmation" {
		t.Fatalf("initial snapshot = %+v, want the claimed session state", snapshot)
	}

	senderSocket := environment.dial(t, created.SessionID, sender.UserSession.Token)
	readFrame(t, senderSocket, &snapshot)
	if snapshot.Session.SenderID != sender.SenderID {
		t.Errorf("Sender-side snapshot = %+v, want its own claim", snapshot.Session)
	}

	payload := base64.StdEncoding.EncodeToString([]byte("opaque-handshake-bytes"))
	writeFrame(t, receiverSocket, relayFrame(created.SessionID, payload))
	var relayed struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		Side      string `json:"side"`
		Payload   string `json:"payload"`
	}
	readFrame(t, senderSocket, &relayed)
	if relayed.Type != "pairing_relay" || relayed.Side != "receiver" || relayed.Payload != payload {
		t.Fatalf("relayed frame = %+v, want the receiver bytes unchanged", relayed)
	}
	if relayed.SessionID != created.SessionID {
		t.Errorf("relayed session = %q, want %q", relayed.SessionID, created.SessionID)
	}

	// A different session's credential cannot subscribe to this session, so
	// sessions stay isolated.
	url := "ws" + strings.TrimPrefix(environment.service.BaseURL, "http") +
		"/v1/pairing-sessions/" + other.SessionID + "/events"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if socket, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols: []string{protocol.Subprotocol},
	}); err == nil {
		_ = socket.CloseNow()
		t.Fatal("a restricted credential subscribed to another session")
	} else if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-session subscription status = %v, want 401", response)
	}

	// Malformed framing is a protocol violation rather than relayed traffic.
	writeFrame(t, receiverSocket, `{"version":1,"type":"pairing_relay","sessionId":"`+created.SessionID+`"}`)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := receiverSocket.Read(ctx); err == nil {
		t.Error("a malformed frame kept the pairing channel open")
	} else if code := websocket.CloseStatus(err); code != websocket.StatusProtocolError {
		t.Errorf("malformed frame close = %v, want %v", code, websocket.StatusProtocolError)
	}

}

// Reconnect returns the current session state without extending the lifetime
// and without replaying handshake bytes. A canceled session refuses both of
// its former participants, and a lapsed one reports its own distinct outcome
// for each participant credential.
func TestPairingReconnectSnapshotsStateWithoutReplay(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-reconnect", "openid-reconnect")

	created, token := environment.createSession(t, "new_receiver", "", "")
	environment.authorize(t, created.SessionID, "", sender.UserSession.Token)

	socket := environment.dial(t, created.SessionID, token)
	var snapshot struct {
		Type    string      `json:"type"`
		Session sessionView `json:"session"`
	}
	readFrame(t, socket, &snapshot)
	originalExpiry := snapshot.Session.ExpiresAt

	senderSocket := environment.dial(t, created.SessionID, sender.UserSession.Token)
	readFrame(t, senderSocket, &snapshot)

	if err := socket.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close first subscription: %v", err)
	}

	// Handshake bytes are live relay traffic: a frame sent while the Receiver
	// side is away is reported to its sender instead of being kept for the
	// next subscription.
	writeFrame(t, senderSocket, relayFrame(created.SessionID, base64.StdEncoding.EncodeToString([]byte("while-away"))))
	var failure struct {
		Type  string `json:"type"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	readFrame(t, senderSocket, &failure)
	if failure.Type != "error" || failure.Error.Code != "pairing_not_ready" {
		t.Fatalf("relay without a receiver side = %+v, want pairing_not_ready", failure)
	}

	reconnected := environment.dial(t, created.SessionID, token)
	readFrame(t, reconnected, &snapshot)
	if snapshot.Type != "pairing_state" || !snapshot.Session.ExpiresAt.Equal(originalExpiry) {
		t.Errorf("reconnect snapshot = %+v, want the current state and the unchanged expiry %v", snapshot, originalExpiry)
	}

	// The reconnected participant continues on live traffic rather than any
	// replayed bytes.
	live := base64.StdEncoding.EncodeToString([]byte("live-after-reconnect"))
	writeFrame(t, reconnected, relayFrame(created.SessionID, live))
	var relayed struct {
		Type    string `json:"type"`
		Payload string `json:"payload"`
	}
	readFrame(t, senderSocket, &relayed)
	if relayed.Type != "pairing_relay" || relayed.Payload != live {
		t.Errorf("relayed frame after reconnect = %+v, want the live bytes", relayed)
	}

	// A canceled session is closed for both former participants: neither can
	// subscribe to it again.
	if status, payload := environment.request(t, http.MethodDelete,
		"/v1/pairing-sessions/"+created.SessionID, "", sender.UserSession.Token); status != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204: %s", status, payload)
	}
	for _, rejected := range []rejectedSubscription{
		{name: "canceled restricted credential", credential: token, wantStatus: http.StatusGone, wantCode: "pairing_session_closed"},
		{name: "canceled owning management credential", credential: sender.UserSession.Token, wantStatus: http.StatusGone, wantCode: "pairing_session_closed"},
	} {
		assertSubscriptionRejected(t, environment, created.SessionID, rejected)
	}

	environment.exec(t, `UPDATE pin_glide.pairing_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`,
		created.SessionID)

	// A lapsed restricted credential reports the expired credential, while
	// the owning Sender's valid management credential reports the expired
	// session.
	for _, rejected := range []rejectedSubscription{
		{name: "expired restricted credential", credential: token, wantStatus: http.StatusUnauthorized, wantCode: "credential_expired"},
		{name: "owning management credential", credential: sender.UserSession.Token, wantStatus: http.StatusGone, wantCode: "pairing_session_expired"},
	} {
		assertSubscriptionRejected(t, environment, created.SessionID, rejected)
	}
}

// rejectedSubscription is one expected pre-upgrade rejection of the restricted
// pairing channel.
type rejectedSubscription struct {
	name       string
	credential string
	wantStatus int
	wantCode   string
}

// assertSubscriptionRejected requires one credential to be refused before the
// pairing channel upgrades, with the given problem status and code.
func assertSubscriptionRejected(t *testing.T, environment *environment, sessionID string, rejected rejectedSubscription) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(environment.service.BaseURL, "http") +
		"/v1/pairing-sessions/" + sessionID + "/events"
	socket, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + rejected.credential}},
		Subprotocols: []string{protocol.Subprotocol},
	})
	if err == nil {
		_ = socket.CloseNow()
		t.Errorf("%s subscribed to a rejected session", rejected.name)
		return
	}
	if response == nil {
		t.Errorf("%s subscription has no response: %v", rejected.name, err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != rejected.wantStatus {
		t.Errorf("%s status = %d, want %d", rejected.name, response.StatusCode, rejected.wantStatus)
	}
	if decoded := decodeProblem(t, body); decoded.Code != rejected.wantCode {
		t.Errorf("%s code = %q, want %q", rejected.name, decoded.Code, rejected.wantCode)
	}
}
