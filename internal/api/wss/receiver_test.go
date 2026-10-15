package wss

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// receiverStub is the pairing domain as the delivery channel needs it. It
// admits the credential of each Receiver's current generation and revalidates
// that generation, so a test drives the same decisions through a real upgrade.
type receiverStub struct {
	mutex   sync.Mutex
	current map[string]int64
	refuse  bool
	// entered reports that an upgrade reached the final commit, and gate holds
	// it there until a test releases it.
	entered chan struct{}
	gate    chan struct{}
}

// newReceiverStub composes a stub whose Receivers each admit generation 1.
func newReceiverStub(receiverIDs ...string) *receiverStub {
	stub := &receiverStub{current: map[string]int64{}}
	for _, receiverID := range receiverIDs {
		stub.current[receiverID] = 1
	}
	return stub
}

// AdmitReceiver reads the <receiverID>:<generation> credential and refuses
// everything the Receiver's current generation does not cover.
func (s *receiverStub) AdmitReceiver(_ context.Context, credential string, _ time.Time) (pairing.ReceiverCredential, error) {
	presented, err := receiverGeneration(credential)
	if err != nil {
		return pairing.ReceiverCredential{}, err
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.current[presented.ReceiverID] != presented.Generation {
		return pairing.ReceiverCredential{}, pairing.ErrCredentialInvalid
	}
	return presented, nil
}

// ConfirmReceiverConnection revalidates one generation under the stub's own
// boundary. refuse stands in for a generation change that committed while the
// upgrade was in flight.
func (s *receiverStub) ConfirmReceiverConnection(_ context.Context, receiverID string, generation int64) error {
	s.mutex.Lock()
	entered, gate := s.entered, s.gate
	s.mutex.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}

	s.mutex.Lock()
	refuse, current := s.refuse, s.current[receiverID]
	s.mutex.Unlock()
	if refuse || current != generation {
		return pairing.ErrCredentialInvalid
	}
	return nil
}

// holdConfirm makes the next final commit report that it arrived and wait, so a
// test can end the channel while one upgrade is still committing.
func (s *receiverStub) holdConfirm() (entered <-chan struct{}, release func()) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.entered = make(chan struct{}, 1)
	s.gate = make(chan struct{})
	return s.entered, func() { close(s.gate) }
}

// supersede commits the next generation of one Receiver, the way an approved
// recovery does.
func (s *receiverStub) supersede(receiverID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.current[receiverID]++
}

// refuseConnection makes the stub refuse the final commit, standing in for a
// generation change that commits while an upgrade is in flight.
func (s *receiverStub) refuseConnection(refuse bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.refuse = refuse
}

// receiverGeneration reads the Receiver and generation a credential presents.
func receiverGeneration(credential string) (pairing.ReceiverCredential, error) {
	receiverID, generation, ok := strings.Cut(credential, ":")
	number, err := strconv.ParseInt(generation, 10, 64)
	if !ok || err != nil || number <= 0 {
		return pairing.ReceiverCredential{}, pairing.ErrCredentialInvalid
	}
	return pairing.ReceiverCredential{ReceiverID: receiverID, Generation: number}, nil
}

// receiverChannel is one delivery channel served on a real listener, with the
// domain decisions a test controls.
type receiverChannel struct {
	socket *ReceiverSocket
	server *httptest.Server
}

// newReceiverChannel composes the channel. A handshake the domain refuses
// before the upgrade answers with a failure status, the way the composed HTTP
// chain reports a rejected delivery subscription.
func newReceiverChannel(t *testing.T, receivers ReceiverAdmissions, presence *Presence) *receiverChannel {
	t.Helper()

	socket := NewReceiverSocket(receivers, presence, nil)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// The composed HTTP chain is what exposes the bearer credential, so the
		// test supplies that one step rather than restating it in the socket.
		ctx := observability.WithCredential(request.Context(), request.Header.Get(observability.CredentialHeader))
		if err := socket.Serve(writer, request.WithContext(ctx), func() {}); err != nil {
			writer.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(server.Close)
	return &receiverChannel{socket: socket, server: server}
}

// dial opens one delivery subscription of the channel.
func (c *receiverChannel) dial(credential string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.server.URL, "http"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + credential}},
		Subprotocols: []string{protocol.Subprotocol},
	})
}

// connected opens one delivery subscription a test then holds open.
func (c *receiverChannel) connected(t *testing.T, credential string) *websocket.Conn {
	t.Helper()

	socket, response, err := c.dial(credential)
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

// awaitPresence waits for one Receiver's published liveness to reach want and
// reports the state that reached it, so a test asserts a transition instead of
// racing the cleanup that publishes it.
func awaitPresence(t *testing.T, presence *Presence, receiverID string, want bool) PresenceState {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		state := presence.State(receiverID)
		if state.Online == want {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("Receiver %s presence = %+v, want online=%v", receiverID, state, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// holdPresence requires one Receiver's published liveness to stay at want for a
// bounded window, so a test observes a state the service must not change.
func holdPresence(t *testing.T, presence *Presence, receiverID string, want bool) {
	t.Helper()

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if state := presence.State(receiverID); state.Online != want {
			t.Fatalf("Receiver %s presence = %+v, want online=%v until %v", receiverID, state, want, deadline)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A committed upgrade is what makes a Receiver online, and the connection that
// made it online is what reports it offline.
func TestCommittedUpgradePublishesOnlineThenOffline(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	channel := newReceiverChannel(t, newReceiverStub("receiver-1"), presence)

	unobserved := presence.State("receiver-1")
	if unobserved.Online || unobserved.StateChangedAt.IsZero() {
		t.Fatalf("an unobserved Receiver is %+v, want offline since the process started", unobserved)
	}

	before := time.Now().UTC()
	socket := channel.connected(t, "receiver-1:1")
	online := awaitPresence(t, presence, "receiver-1", true)
	if online.StateChangedAt.Before(before) || online.StateChangedAt.After(time.Now().UTC()) {
		t.Errorf("online state changed at %v, want the committed upgrade between %v and now", online.StateChangedAt, before)
	}

	if err := socket.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close delivery connection: %v", err)
	}
	offline := awaitPresence(t, presence, "receiver-1", false)
	if offline.StateChangedAt.Before(online.StateChangedAt) {
		t.Errorf("offline state changed at %v, before the online transition %v", offline.StateChangedAt, online.StateChangedAt)
	}
}

// A handshake the domain refuses before the upgrade neither publishes presence
// nor holds the slot, so the connection that owns the Receiver keeps serving.
func TestRefusedUpgradeLeavesPresenceAndSlotUntouched(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	receivers := newReceiverStub("receiver-1")
	channel := newReceiverChannel(t, receivers, presence)

	held := channel.connected(t, "receiver-1:1")
	online := awaitPresence(t, presence, "receiver-1", true)

	// A superseded generation and an unknown credential are both refused before
	// the upgrade, so neither disturbs the connection that holds the slot.
	receivers.supersede("receiver-1")
	for _, credential := range []string{"receiver-1:1", "receiver-2:1", "unknown"} {
		if socket, _, err := channel.dial(credential); err == nil {
			_ = socket.CloseNow()
			t.Fatalf("credential %q opened the delivery channel", credential)
		}
	}
	if state := presence.State("receiver-1"); !state.Online || !state.StateChangedAt.Equal(online.StateChangedAt) {
		t.Errorf("a refused handshake published %+v, want the first connection's %+v", state, online)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := held.Read(ctx); err == nil {
		t.Error("the refused handshakes sent the first connection unexpected traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("a refused handshake closed the first connection: %v", err)
	}
}

// A generation change that commits while the upgrade is in flight cancels that
// upgrade before the 101, so the connection never becomes Receiver presence.
func TestGenerationChangeDuringUpgradePreventsTheConnection(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	receivers := newReceiverStub("receiver-1")
	channel := newReceiverChannel(t, receivers, presence)

	receivers.refuseConnection(true)
	socket, response, err := channel.dial("receiver-1:1")
	if err == nil {
		_ = socket.CloseNow()
		t.Fatal("an upgrade whose generation moved on committed anyway")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Errorf("cancelled upgrade = %v, want 401", response)
	}
	if state := presence.State("receiver-1"); state.Online {
		t.Errorf("a cancelled upgrade published %+v, want offline", state)
	}

	// The cancellation released the reservation, so the generation that still
	// owns the Receiver connects.
	receivers.refuseConnection(false)
	channel.connected(t, "receiver-1:1")
	awaitPresence(t, presence, "receiver-1", true)
}

// Shutdown ends an upgrade that is still committing: the connection closes with
// the shutdown meaning and never becomes Receiver presence.
func TestShutdownEndsAnUpgradeStillCommitting(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	receivers := newReceiverStub("receiver-1")
	channel := newReceiverChannel(t, receivers, presence)
	entered, release := receivers.holdConfirm()

	ended := make(chan error, 1)
	go func() {
		socket, _, err := channel.dial("receiver-1:1")
		if err == nil {
			_, _, err = socket.Read(context.Background())
		}
		ended <- err
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the upgrade never reached its final commit")
	}
	channel.socket.Close()
	release()

	select {
	case err := <-ended:
		if status := websocket.CloseStatus(err); status != websocket.StatusGoingAway {
			t.Errorf("shutdown ended the upgrade with %v, want %v", err, websocket.StatusGoingAway)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown left an upgrade that was still committing serving")
	}
	if state := presence.State("receiver-1"); state.Online {
		t.Errorf("an upgrade shutdown ended published %+v, want offline", state)
	}
}

// One Receiver keeps one slot per credential generation, and its presence
// follows the newer connection: an older connection's close cannot report the
// Receiver offline, while ending the Receiver's access closes what holds it and
// reports it offline.
func TestNewerGenerationOwnsPresenceAcrossOldClose(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	receivers := newReceiverStub("receiver-1")
	channel := newReceiverChannel(t, receivers, presence)

	older := channel.connected(t, "receiver-1:1")
	online := awaitPresence(t, presence, "receiver-1", true)

	receivers.supersede("receiver-1")
	newer := channel.connected(t, "receiver-1:2")

	// The generation that already holds a slot is refused, and neither the
	// refusal nor the newer connection changes the online time.
	if socket, _, err := channel.dial("receiver-1:2"); err == nil {
		_ = socket.CloseNow()
		t.Error("a second handshake of the generation holding the slot opened the delivery channel")
	}
	if state := presence.State("receiver-1"); !state.StateChangedAt.Equal(online.StateChangedAt) {
		t.Errorf("the newer connection published %+v, want the online time %v kept", state, online.StateChangedAt)
	}

	if err := older.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close superseded connection: %v", err)
	}
	holdPresence(t, presence, "receiver-1", true)

	channel.socket.EndReceiverAccess("receiver-1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := newer.Read(ctx); err == nil {
		t.Error("ending Receiver access left the committed connection serving")
	} else if code := websocket.CloseStatus(err); code != websocket.StatusPolicyViolation {
		t.Errorf("ending Receiver access closed with %v, want %v", code, websocket.StatusPolicyViolation)
	}
	awaitPresence(t, presence, "receiver-1", false)
}

// Owner security revocation tells the Receiver's committed connection why it
// is ending before the policy Close, so a revoked installation reads the
// adopted control instead of observing a transport that simply stopped.
func TestDeviceRevokedReportsTheControlThenCloses(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	channel := newReceiverChannel(t, newReceiverStub("receiver-1"), presence)
	socket := channel.connected(t, "receiver-1:1")
	awaitPresence(t, presence, "receiver-1", true)

	channel.socket.DeviceRevoked("receiver-1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messageType, data, err := socket.Read(ctx)
	if err != nil {
		t.Fatalf("read revocation control: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("revocation control type = %v, want text", messageType)
	}
	var control protocol.DeviceRevoked
	if err := json.Unmarshal(data, &control); err != nil {
		t.Fatalf("decode revocation control: %v: %s", err, data)
	}
	if control.Version != protocol.WSSVersion || control.Type != "device_revoked" || control.ReceiverID != "receiver-1" {
		t.Errorf("revocation control = %+v, want version 1 type device_revoked of receiver-1", control)
	}

	if _, _, err := socket.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("revocation closed the connection with %v, want %v",
			websocket.CloseStatus(err), websocket.StatusPolicyViolation)
	}
	awaitPresence(t, presence, "receiver-1", false)
}

// A peer that stops answering the heartbeat is gone: the connection is closed,
// its slot is released and the Receiver it made online becomes offline.
func TestHeartbeatEndsAConnectionThatStopsAnswering(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	channel := newReceiverChannel(t, newReceiverStub("receiver-1"), presence)
	channel.socket.heartbeatInterval = 50 * time.Millisecond
	channel.socket.pongTimeout = 50 * time.Millisecond

	// This client never reads, so it never answers the service's Ping.
	channel.connected(t, "receiver-1:1")
	awaitPresence(t, presence, "receiver-1", true)
	offline := awaitPresence(t, presence, "receiver-1", false)

	// The failed connection released its slot, so the credential can subscribe
	// again.
	time.Sleep(50 * time.Millisecond)
	channel.connected(t, "receiver-1:1")
	online := awaitPresence(t, presence, "receiver-1", true)
	if !online.StateChangedAt.After(offline.StateChangedAt) {
		t.Errorf("reconnect at %v, want it after the heartbeat offline transition %v", online.StateChangedAt, offline.StateChangedAt)
	}
}
