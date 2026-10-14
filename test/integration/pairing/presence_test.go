//go:build integration

package pairing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api/wss"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
	"github.com/coder/websocket"
)

// receiverPresence waits for one Receiver's published liveness on one composed
// service to reach want, so a test observes a transition instead of racing the
// cleanup that publishes it.
func receiverPresence(t *testing.T, service *harness.Service, receiverID string, want bool, within time.Duration) wss.PresenceState {
	t.Helper()

	deadline := time.Now().Add(within)
	for {
		state := service.Presence.State(receiverID)
		if state.Online == want {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("Receiver %s presence = %+v, want online=%v within %v", receiverID, state, want, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A Receiver is online exactly while its committed delivery connection lives:
// refused handshakes leave that state alone, the connection's own end reports it
// offline, and reconnecting publishes a new live connection without replaying
// anything.
func TestDeliveryPresenceFollowsTheCommittedConnection(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-presence", "openid-presence")
	ready, token := environment.readySession(t, sender)

	// The retransmission replaces the credential no one had proved possession
	// of, so the first value is the superseded generation's.
	status, stale, payload := environment.redeem(t, ready.SessionID, token, "op-presence", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}
	status, issued, payload := environment.redeem(t, ready.SessionID, token, "op-presence", 1)
	if status != http.StatusOK {
		t.Fatalf("retransmission = %d: %s", status, payload)
	}

	// A handshake the domain refuses before the upgrade neither publishes
	// presence nor holds the slot the current credential then takes.
	receiverRejected(t, environment, stale.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	if state := environment.service.Presence.State(issued.ReceiverID); state.Online {
		t.Errorf("a refused handshake published %+v, want offline", state)
	}

	before := time.Now().UTC()
	receiver := receiverConnected(t, environment, issued.ReceiverToken)
	online := receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)
	if online.StateChangedAt.Before(before) || online.StateChangedAt.After(time.Now().UTC()) {
		t.Errorf("online state changed at %v, want the committed upgrade between %v and now", online.StateChangedAt, before)
	}

	// A second handshake of the credential that holds the slot is refused
	// without evicting the connection or changing its presence.
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusConflict, "receiver_already_connected")
	if state := environment.service.Presence.State(issued.ReceiverID); !state.Online || !state.StateChangedAt.Equal(online.StateChangedAt) {
		t.Errorf("a refused second handshake published %+v, want %+v", state, online)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := receiver.Read(ctx); err == nil {
		t.Error("the first delivery connection received unexpected traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("a refused second handshake closed the first connection: %v", err)
	}

	// The connection's own end reports the Receiver offline.
	if err := receiver.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close delivery connection: %v", err)
	}
	offline := receiverPresence(t, environment.service, issued.ReceiverID, false, 5*time.Second)
	if offline.StateChangedAt.Before(online.StateChangedAt) {
		t.Errorf("offline state changed at %v, before the online transition %v", offline.StateChangedAt, online.StateChangedAt)
	}

	// Reconnecting establishes a new live connection, and nothing kept for the
	// closed one arrives on it.
	reconnected := receiverConnected(t, environment, issued.ReceiverToken)
	reobserved := receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)
	if !reobserved.StateChangedAt.After(offline.StateChangedAt) {
		t.Errorf("reconnect state changed at %v, want it after the offline transition %v", reobserved.StateChangedAt, offline.StateChangedAt)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := reconnected.Read(ctx); err == nil {
		t.Error("the reconnected Receiver received traffic kept from the closed connection")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("the reconnected Receiver stopped serving: %v", err)
	}
}

// A Receiver that stops answering the service loses its connection, its slot
// and its online state once the adopted Pong deadline passes.
func TestHalfOpenDeliveryConnectionGoesOfflineAndReleasesItsSlot(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-half-open", "openid-half-open")
	ready, token := environment.readySession(t, sender)

	status, issued, payload := environment.redeem(t, ready.SessionID, token, "op-half-open", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}

	// This connection never reads, so it never answers the service's Ping. The
	// service pings every 15 seconds and requires the Pong within 5 seconds.
	halfOpen, _, err := dialDelivery(t, environment.service, issued.ReceiverToken)
	if err != nil {
		t.Fatalf("connect Receiver: %v", err)
	}
	t.Cleanup(func() { _ = halfOpen.CloseNow() })

	receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)
	offline := receiverPresence(t, environment.service, issued.ReceiverID, false, 30*time.Second)

	// The failed connection released its slot, so the credential subscribes
	// again and publishes a new live connection.
	connectedDelivery(t, environment.service, issued.ReceiverToken)
	online := receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)
	if !online.StateChangedAt.After(offline.StateChangedAt) {
		t.Errorf("reconnect state changed at %v, want it after the offline transition %v", online.StateChangedAt, offline.StateChangedAt)
	}
}

// A restarted service reports its Receivers offline, having ended the
// connections of the process it replaced, and the credential of that connection
// establishes a new one there.
func TestRestartReportsReceiverOfflineAndAdmitsAgain(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-restart", "openid-restart")
	ready, token := environment.readySession(t, sender)

	status, issued, payload := environment.redeem(t, ready.SessionID, token, "op-restart", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}

	receiver := receiverConnected(t, environment, issued.ReceiverToken)
	receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)

	// The Receiver keeps reading until the service goes away, the way a live
	// client does, so the restart ends this connection instead of leaving it
	// unread.
	closed := receiver.CloseRead(context.Background())
	if err := environment.service.Stop(); err != nil {
		t.Fatalf("stop the service process: %v", err)
	}
	select {
	case <-closed.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the stopped service left its delivery connection open")
	}

	// The next process has observed no connection, so the Receiver is offline
	// for as long as that process has been reporting.
	restartedAt := time.Now().UTC()
	restarted := harness.StartService(t, environment.databaseURL, environment.provider)
	offline := receiverPresence(t, restarted, issued.ReceiverID, false, 5*time.Second)
	if offline.StateChangedAt.Before(restartedAt) || offline.StateChangedAt.After(time.Now().UTC()) {
		t.Errorf("restarted process reports offline since %v, want between %v and now", offline.StateChangedAt, restartedAt)
	}

	// The credential the old process admitted is still current, and the new
	// process publishes the connection it establishes.
	connectedDelivery(t, restarted, issued.ReceiverToken)
	receiverPresence(t, restarted, issued.ReceiverID, true, 5*time.Second)
}
