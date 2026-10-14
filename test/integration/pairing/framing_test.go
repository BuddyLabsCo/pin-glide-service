//go:build integration

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

// requireClose requires one served connection to end with the meaning want
// once the service has decided the outcome. Frames the service sends before the
// close, such as the state change that ended the session, are not the outcome.
func requireClose(t *testing.T, socket *websocket.Conn, want websocket.StatusCode) {
	t.Helper()

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _, err := socket.Read(ctx)
		cancel()
		if err == nil {
			continue
		}
		if code := websocket.CloseStatus(err); code != want {
			t.Errorf("the service closed with %v, want %v", err, want)
		}
		return
	}
}

// The pairing channel speaks one subprotocol. An upgrade that offered none is
// refused before the 101 with the shared problem model, and the session's
// credential is not consumed by the refusal.
func TestPairingChannelRequiresThePublishedSubprotocol(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-pairing-subprotocol", "openid-pairing-subprotocol")
	created, token := environment.createSession(t, "new_receiver", "", "")
	environment.authorize(t, created.SessionID, "", sender.UserSession.Token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(environment.service.BaseURL, "http") +
		"/v1/pairing-sessions/" + created.SessionID + "/events"
	socket, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err == nil {
		_ = socket.CloseNow()
		t.Fatal("an upgrade without the published subprotocol opened the pairing channel")
	}
	if response == nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("upgrade without the subprotocol = %v, want 400", response)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read refusal body: %v", err)
	}
	if decoded := decodeProblem(t, body); decoded.Code != "invalid_request" {
		t.Errorf("upgrade without the subprotocol reported %q, want invalid_request", decoded.Code)
	}

	// The refusal decided nothing about the session, so the credential still
	// opens the channel with the subprotocol the surface publishes.
	environment.dial(t, created.SessionID, token)
}

// The pairing channel accepts only its own message type at its own version and
// for its own session. Every other frame is a protocol error, and the service
// ends the connection with 1002 rather than relaying it.
func TestPairingChannelClosesOnProtocolViolations(t *testing.T) {
	tests := []struct {
		name  string
		frame func(sessionID string) string
	}{
		{
			name: "another message version",
			frame: func(sessionID string) string {
				return `{"version":2,"type":"pairing_relay","sessionId":"` + sessionID + `","payload":"AAAA"}`
			},
		},
		{
			name: "an unknown message type",
			frame: func(sessionID string) string {
				return `{"version":1,"type":"delivery","sessionId":"` + sessionID + `","payload":"AAAA"}`
			},
		},
		{
			name: "another session",
			frame: func(string) string {
				return `{"version":1,"type":"pairing_relay","sessionId":"session-somewhere-else","payload":"AAAA"}`
			},
		},
		{
			name: "a payload that is not padded Base64",
			frame: func(sessionID string) string {
				return `{"version":1,"type":"pairing_relay","sessionId":"` + sessionID + `","payload":"not base64!"}`
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := start(t)
			sender := environment.sender(t, "code-pairing-protocol", "openid-pairing-protocol")
			created, token := environment.createSession(t, "new_receiver", "", "")
			environment.authorize(t, created.SessionID, "", sender.UserSession.Token)

			receiverSocket := environment.dial(t, created.SessionID, token)
			readInitialSnapshot(t, receiverSocket)
			writeFrame(t, receiverSocket, test.frame(created.SessionID))
			requireClose(t, receiverSocket, websocket.StatusProtocolError)
		})
	}
}

// Text the service cannot read as UTF-8 is an invalid payload rather than a
// protocol violation, so it closes with 1007.
func TestPairingChannelClosesOnInvalidTextEncoding(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-pairing-encoding", "openid-pairing-encoding")
	created, token := environment.createSession(t, "new_receiver", "", "")
	environment.authorize(t, created.SessionID, "", sender.UserSession.Token)

	socket := dialRawSocket(t, environment.service.BaseURL,
		"/v1/pairing-sessions/"+created.SessionID+"/events", token, protocol.Subprotocol)
	var snapshot struct {
		Type string `json:"type"`
	}
	socket.receiveMessage(t, rawText, &snapshot)
	if snapshot.Type != "pairing_state" {
		t.Fatalf("first frame type = %q, want pairing_state", snapshot.Type)
	}
	socket.writeFrame(t, rawText, []byte{0xff})
	if reason := socket.requireCloseFrame(t, int(websocket.StatusInvalidFramePayloadData)); reason != "invalid_payload" {
		t.Errorf("invalid text encoding closed with reason %q, want invalid_payload", reason)
	}
}

// A session that may no longer relay ends its live channel with the policy
// meaning, so a client learns the channel is closed instead of waiting for
// traffic that will not arrive.
func TestPairingChannelClosesOnSessionClosure(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-pairing-policy", "openid-pairing-policy")
	created, token := environment.createSession(t, "new_receiver", "", "")
	environment.authorize(t, created.SessionID, "", sender.UserSession.Token)

	receiverSocket := environment.dial(t, created.SessionID, token)
	readInitialSnapshot(t, receiverSocket)

	// Cancel is the owner's decision: the session closes for both participants
	// without activating a Pairing.
	if status, payload := environment.request(t, http.MethodDelete,
		"/v1/pairing-sessions/"+created.SessionID, "", sender.UserSession.Token); status != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204: %s", status, payload)
	}

	requireClose(t, receiverSocket, websocket.StatusPolicyViolation)
}

// A participant that closes normally ends only its own connection: the session
// keeps serving the other side and a reconnect.
func TestPairingChannelServesANormalClose(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-pairing-normal", "openid-pairing-normal")
	created, token := environment.createSession(t, "new_receiver", "", "")
	environment.authorize(t, created.SessionID, "", sender.UserSession.Token)

	receiverSocket := environment.dial(t, created.SessionID, token)
	readInitialSnapshot(t, receiverSocket)
	if err := receiverSocket.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close the pairing channel: %v", err)
	}

	// The session is unchanged, so the same credential opens a live connection
	// again.
	reconnected := environment.dial(t, created.SessionID, token)
	readInitialSnapshot(t, reconnected)
}

// A recoverable relay failure is reported to its sender and leaves the channel
// usable, with the service request ID and the session a client correlates the
// failure against.
func TestPairingChannelReportsRecoverableFailuresWithCorrelation(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-pairing-correlation", "openid-pairing-correlation")
	created, token := environment.createSession(t, "new_receiver", "", "")
	environment.authorize(t, created.SessionID, "", sender.UserSession.Token)

	receiverSocket := environment.dial(t, created.SessionID, token)
	readInitialSnapshot(t, receiverSocket)

	// The Sender side is not connected, so the frame has no peer to reach.
	writeFrame(t, receiverSocket, relayFrame(created.SessionID,
		base64.StdEncoding.EncodeToString([]byte("while-the-peer-is-away"))))

	var reported struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
		SessionID string `json:"sessionId"`
		Error     struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	readFrame(t, receiverSocket, &reported)
	if reported.Type != "error" || reported.Error.Code != "pairing_not_ready" {
		t.Fatalf("recoverable failure = %+v, want pairing_not_ready", reported)
	}
	if reported.RequestID == "" {
		t.Error("the recoverable failure carries no service request ID")
	}
	if reported.SessionID != created.SessionID {
		t.Errorf("the recoverable failure correlates to session %q, want %q", reported.SessionID, created.SessionID)
	}
	if reported.Error.Detail == "" {
		t.Error("the recoverable failure carries no readable explanation")
	}

	// The channel stays open, so the same participant continues relaying once
	// its peer is present.
	senderSocket := environment.dial(t, created.SessionID, sender.UserSession.Token)
	readInitialSnapshot(t, senderSocket)
	payload := base64.StdEncoding.EncodeToString([]byte("live-after-the-failure"))
	writeFrame(t, receiverSocket, relayFrame(created.SessionID, payload))

	var relayed struct {
		Type    string `json:"type"`
		Payload string `json:"payload"`
	}
	readFrame(t, senderSocket, &relayed)
	if relayed.Type != "pairing_relay" || relayed.Payload != payload {
		t.Errorf("relayed frame after the recoverable failure = %+v, want the live bytes", relayed)
	}
}

// Unknown optional fields are a compatible addition, so a frame that carries
// one still relays: the service tolerates what it does not know and rejects
// only what the definition does not allow.
func TestPairingChannelToleratesUnknownOptionalFields(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-pairing-extension", "openid-pairing-extension")
	created, token := environment.createSession(t, "new_receiver", "", "")
	environment.authorize(t, created.SessionID, "", sender.UserSession.Token)

	receiverSocket := environment.dial(t, created.SessionID, token)
	readInitialSnapshot(t, receiverSocket)
	senderSocket := environment.dial(t, created.SessionID, sender.UserSession.Token)
	readInitialSnapshot(t, senderSocket)

	payload := base64.StdEncoding.EncodeToString([]byte("extended-frame"))
	writeFrame(t, receiverSocket, `{"version":1,"type":"pairing_relay","sessionId":"`+created.SessionID+
		`","payload":"`+payload+`","futureField":{"nested":true}}`)

	var relayed struct {
		Type    string `json:"type"`
		Side    string `json:"side"`
		Payload string `json:"payload"`
	}
	readFrame(t, senderSocket, &relayed)
	if relayed.Type != "pairing_relay" || relayed.Side != "receiver" || relayed.Payload != payload {
		t.Errorf("relayed extended frame = %+v, want the receiver's bytes unchanged", relayed)
	}
}

// readInitialSnapshot reads the state frame an authorized subscription
// receives first, so a test continues from a served connection.
func readInitialSnapshot(t *testing.T, socket *websocket.Conn) sessionView {
	t.Helper()

	var snapshot struct {
		Type    string      `json:"type"`
		Session sessionView `json:"session"`
	}
	readFrame(t, socket, &snapshot)
	if snapshot.Type != "pairing_state" {
		t.Fatalf("first frame type = %q, want pairing_state", snapshot.Type)
	}
	return snapshot.Session
}
