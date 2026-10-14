//go:build integration

package pairing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// The delivery channel is one subprotocol, one credential and one open slot.
// Every refusal reaches a client through the shared problem model before the
// upgrade, and none of them disturbs the connection that holds the slot.
func TestReceiverDeliveryChannelRefusesBeforeTheUpgrade(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-receiver-framing", "openid-receiver-framing")
	ready, token := environment.readySession(t, sender)

	status, issued, payload := environment.redeem(t, ready.SessionID, token, "op-receiver-framing", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}

	held := receiverConnected(t, environment, issued.ReceiverToken)
	online := receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)

	for _, refused := range []struct {
		name       string
		credential string
		protocol   string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "no credential",
			protocol:   protocol.Subprotocol,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "credential_missing",
		},
		{
			name:       "no subprotocol",
			credential: issued.ReceiverToken,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "another subprotocol",
			credential: issued.ReceiverToken,
			protocol:   "pin-glide.v2",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "unknown credential",
			credential: "not-a-receiver-credential",
			protocol:   protocol.Subprotocol,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "credential_invalid",
		},
		{
			name:       "credential that already holds the slot",
			credential: issued.ReceiverToken,
			protocol:   protocol.Subprotocol,
			wantStatus: http.StatusConflict,
			wantCode:   "receiver_already_connected",
		},
	} {
		t.Run(refused.name, func(t *testing.T) {
			_, response := upgradeRawSocket(t, environment.service.BaseURL, "/v1/receiver/events",
				refused.credential, refused.protocol)
			refuseUpgrade(t, response, refused.wantStatus, refused.wantCode)
		})
	}

	// None of the refusals released the slot or changed the presence of the
	// connection that already holds it.
	if state := environment.service.Presence.State(issued.ReceiverID); !state.Online || !state.StateChangedAt.Equal(online.StateChangedAt) {
		t.Errorf("refused handshakes published %+v, want the first connection's %+v", state, online)
	}
	readTimeout, cancelRead := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelRead()
	if _, _, err := held.Read(readTimeout); err == nil {
		t.Error("a refused handshake sent the delivery connection traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("a refused handshake closed the delivery connection: %v", err)
	}
}

// An application frame the delivery channel does not carry is a protocol
// error, so the service closes with 1002 instead of ignoring it.
func TestReceiverDeliveryChannelClosesOnAnUnknownMessageType(t *testing.T) {
	environment := start(t)
	issued := environment.redeemReceiver(t, "code-receiver-protocol", "openid-receiver-protocol", "op-receiver-protocol")

	socket := dialRawSocket(t, environment.service.BaseURL, "/v1/receiver/events",
		issued.ReceiverToken, protocol.Subprotocol)
	socket.writeFrame(t, rawText, []byte(`{"version":1,"type":"delivery"}`))
	socket.requireCloseFrame(t, int(websocket.StatusProtocolError))
}

// Only text frames are part of this channel, so a binary frame is a
// message-type violation rather than an encoding outcome and closes with 1002.
func TestReceiverDeliveryChannelClosesOnABinaryFrame(t *testing.T) {
	environment := start(t)
	issued := environment.redeemReceiver(t, "code-receiver-binary", "openid-receiver-binary", "op-receiver-binary")

	socket := dialRawSocket(t, environment.service.BaseURL, "/v1/receiver/events",
		issued.ReceiverToken, protocol.Subprotocol)
	socket.writeFrame(t, rawBinary, []byte{0x01, 0x02})
	if reason := socket.requireCloseFrame(t, int(websocket.StatusProtocolError)); reason != "protocol_error" {
		t.Errorf("a binary frame closed with reason %q, want protocol_error", reason)
	}
}

// Text the service cannot read as UTF-8 closes with 1007. A conforming client
// library cannot send such a frame, so the test writes the bytes itself.
func TestReceiverDeliveryChannelClosesOnInvalidTextEncoding(t *testing.T) {
	environment := start(t)
	issued := environment.redeemReceiver(t, "code-receiver-encoding", "openid-receiver-encoding", "op-receiver-encoding")

	socket := dialRawSocket(t, environment.service.BaseURL, "/v1/receiver/events",
		issued.ReceiverToken, protocol.Subprotocol)
	socket.writeFrame(t, rawText, []byte{0xff})
	if reason := socket.requireCloseFrame(t, int(websocket.StatusInvalidFramePayloadData)); reason != "invalid_payload" {
		t.Errorf("invalid text encoding closed with reason %q, want invalid_payload", reason)
	}
}

// A service that stops ends its delivery connection with the shutdown meaning
// rather than leaving it to fail on the transport.
func TestShutdownClosesTheDeliveryChannel(t *testing.T) {
	environment := start(t)
	issued := environment.redeemReceiver(t, "code-receiver-shutdown", "openid-receiver-shutdown", "op-receiver-shutdown")

	socket := dialRawSocket(t, environment.service.BaseURL, "/v1/receiver/events",
		issued.ReceiverToken, protocol.Subprotocol)
	receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)

	// The frame read observes what the service sends while it goes away, so the
	// test does not stop the process before asking.
	type closed struct {
		code   int
		reason string
	}
	ended := make(chan closed, 1)
	go func() {
		code, reason, err := socket.awaitClose()
		if err != nil {
			ended <- closed{code: -1}
			return
		}
		ended <- closed{code: code, reason: reason}
	}()

	if err := environment.service.Stop(); err != nil {
		t.Fatalf("stop the service process: %v", err)
	}
	select {
	case final := <-ended:
		if final.code != int(websocket.StatusGoingAway) {
			t.Errorf("shutdown closed the delivery channel with %d (%q), want %d",
				final.code, final.reason, websocket.StatusGoingAway)
		}
		if final.reason != "service_shutdown" {
			t.Errorf("shutdown closed with reason %q, want service_shutdown", final.reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stopped service left its delivery connection serving")
	}
	if state := environment.service.Presence.State(issued.ReceiverID); state.Online {
		t.Errorf("the stopped service reports the Receiver %+v, want offline", state)
	}
}

// redeemReceiver opens one Receiver whose credential the returned token
// possesses, using the composed pairing flow.
func (e *environment) redeemReceiver(t *testing.T, code, openID, operationID string) issuedCredential {
	t.Helper()

	sender := e.sender(t, code, openID)
	ready, token := e.readySession(t, sender)
	status, issued, payload := e.redeem(t, ready.SessionID, token, operationID, 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}
	return issued
}
