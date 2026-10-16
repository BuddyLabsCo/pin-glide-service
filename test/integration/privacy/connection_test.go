//go:build integration

// Real pairing connections of the composed service: what the service retains
// for each connection, and what it must never retain from the handshake bytes
// clients exchange through it.
package privacy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dial opens one authenticated pairing subscription and returns its socket once
// the snapshot the service sends on commit has arrived, so the caller decides
// how the connection ends.
func (e *environment) dial(t *testing.T, sessionID, credential string) *websocket.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.service.BaseURL, "http") +
		"/v1/pairing-sessions/" + sessionID + "/events"
	socket, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + credential}},
		Subprotocols: []string{"pin-glide.v1"},
	})
	if err != nil {
		t.Fatalf("dial pairing channel %s: %v: %+v", url, err, response)
	}
	t.Cleanup(func() { _ = socket.CloseNow() })
	if _, _, err := socket.Read(ctx); err != nil {
		t.Fatalf("read the committed snapshot: %v", err)
	}
	return socket
}

// pairingFrame is one decoded frame of a pairing connection.
type pairingFrame struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// writeFrame sends one application frame.
func writeFrame(t *testing.T, socket *websocket.Conn, frame string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := socket.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// readFrame reads one application frame.
func readFrame(t *testing.T, socket *websocket.Conn) pairingFrame {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messageType, data, err := socket.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("frame arrived as message type %v, want text", messageType)
	}
	var frame pairingFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return frame
}

// closeStatus reads one connection to its end and reports the Close status the
// service closed it with.
func closeStatus(t *testing.T, socket *websocket.Conn) websocket.StatusCode {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := socket.Read(ctx)
	if err == nil {
		t.Fatal("the connection answered instead of closing")
	}
	status := websocket.CloseStatus(err)
	if status == -1 {
		t.Fatalf("the connection did not close: %v", err)
	}
	return status
}

// TestWSSDiagnosticsCorrelateOneConnection shows the retained outcome of real
// pairing connections: the session each one belonged to, the handshake bytes
// that travelled through it and the Close status it ended with.
func TestWSSDiagnosticsCorrelateOneConnection(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-wss", "openid-wss")

	status, payload := environment.request(t, http.MethodPost, "/v1/pairing-sessions",
		`{"mode":"new_receiver"}`, "")
	if status != http.StatusCreated {
		t.Fatalf("create Pairing Session status = %d: %s", status, payload)
	}
	var created struct {
		Session struct {
			SessionID string `json:"sessionId"`
		} `json:"session"`
		PairingToken string `json:"pairingToken"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode created session: %v: %s", err, payload)
	}

	// The Sender's own subscription belongs to an authenticated session, so it
	// claims the session first, exactly as its client does.
	status, payload = environment.request(t, http.MethodPost,
		"/v1/pairing-sessions/"+created.Session.SessionID+"/authorize",
		`{"alias":"Diagnosed"}`, sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}

	// Both sides of the session subscribe, so the handshake bytes this test
	// hands the service really travel through it.
	senderSocket := environment.dial(t, created.Session.SessionID, sender.UserSession.Token)
	receiverSocket := environment.dial(t, created.Session.SessionID, created.PairingToken)
	writeFrame(t, senderSocket, `{"version":1,"type":"pairing_relay","sessionId":"`+
		created.Session.SessionID+`","payload":"`+relayedCanary+`"}`)
	if relayed := readFrame(t, receiverSocket); relayed.Payload != relayedCanary {
		t.Errorf("the peer received a %s frame without the handshake bytes this test sent",
			relayed.Type)
	}

	// A frame the channel cannot read is an error its own connection ends with
	// its own meaning, while the peer's connection still ends normally.
	writeFrame(t, senderSocket, `{"version":1,"type":"pairing_relay","sessionId":"`+
		created.Session.SessionID)
	if closed := closeStatus(t, senderSocket); closed != websocket.StatusProtocolError {
		t.Errorf("the failing connection closed with %d, want the protocol-error meaning", closed)
	}
	if err := receiverSocket.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close the peer connection: %v", err)
	}

	environment.stopped(t)

	// One operation correlates every record it produced, so the session's own
	// identifier carries one record per connection with the status it ended
	// under.
	retained := environment.diagnosticsFor(t, "wss", created.Session.SessionID)
	if len(retained) != 2 {
		t.Fatalf("the session kept %d records, want one per connection: %+v",
			len(retained), retained)
	}
	statuses := map[int64]int{}
	for _, record := range retained {
		if record.method.Valid || record.durationMS.Valid {
			t.Errorf("pairing connection record = %+v, want no request fields", record)
		}
		if !record.code.Valid {
			t.Errorf("pairing connection record = %+v, want a Close status", record)
			continue
		}
		statuses[record.code.Int64]++
	}
	if statuses[int64(websocket.StatusProtocolError)] != 1 ||
		statuses[int64(websocket.StatusNormalClosure)] != 1 {
		t.Errorf("the session retained the close statuses %v, want one protocol error and one normal close",
			statuses)
	}
	refuseSecrets(t, "service rows", environment.storedText(t))
	refuseSecrets(t, "service logs", environment.logs.String())
}

// diagnosticsFor reads every retained record of one surface and subject.
func (e *environment) diagnosticsFor(t *testing.T, kind, subject string) []diagnostic {
	t.Helper()

	return e.retained(t, `
SELECT request_id, kind, subject, method, code, duration_ms
FROM pin_glide.service_diagnostics
WHERE kind = $1 AND subject = $2
ORDER BY id`, kind, subject)
}
