package wss

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

const (
	// heartbeatInterval is how often the service asks one committed Receiver
	// connection whether its peer is still there.
	heartbeatInterval = 15 * time.Second
	// pongTimeout is how long that connection has to answer before the service
	// treats it as gone and releases its slot.
	pongTimeout = 5 * time.Second
	// handshakeTimeout bounds the work one delivery handshake performs before it
	// answers the client, so a stalled dependency cannot hold a Receiver's slot
	// behind an open handshake. It matches the server's header timeout.
	handshakeTimeout = 5 * time.Second
	// controlWriteTimeout bounds one service-initiated control write, so a
	// Receiver that stopped reading cannot hold the operation reporting the
	// change for longer than that window.
	controlWriteTimeout = 2 * time.Second
)

// ErrReceiverAlreadyConnected reports a Receiver that already holds the one
// reserved or active delivery connection of its credential generation. A
// rejected second handshake never evicts the first.
var ErrReceiverAlreadyConnected = errors.New("receiver already connected")

// ErrReceiverUnavailable reports delivery admission during shutdown, so a
// handshake still arriving cannot extend the draining process.
var ErrReceiverUnavailable = errors.New("receiver channel unavailable")

// ReceiverAdmissions is the pairing domain as the delivery channel needs it:
// the domain decides whether a presented credential authorizes the Receiver
// and which generation it presents, consumes the issuance grant that
// presentation proves possession of, and revalidates that generation when the
// upgrade is about to commit.
type ReceiverAdmissions interface {
	AdmitReceiver(ctx context.Context, credential string, now time.Time) (pairing.ReceiverCredential, error)
	ConfirmReceiverConnection(ctx context.Context, receiverID string, generation int64) error
}

// ReceiverSocket serves the single R-authenticated delivery channel of each
// Receiver. It owns the upgrade, the reserved/active slot of each credential
// generation, and the presence that slot publishes; the domain owns credential
// validity, possession and every generation change. Delivery and Receipt traffic
// joins this connection with live delivery.
type ReceiverSocket struct {
	receivers ReceiverAdmissions
	presence  *Presence
	logger    *slog.Logger
	// recorder stores the safe outcome of every delivery connection this
	// channel commits and ends; a socket composed without one records nothing.
	recorder observability.Recorder
	// receipts is the sink of the Receipt frames this channel accepts. The
	// delivery domain owns what a Receipt means; a socket composed without a
	// sink keeps serving and stops acknowledging arrival.
	receipts ReceiptSink

	// heartbeatInterval and pongTimeout are the adopted liveness bounds of one
	// committed connection.
	heartbeatInterval time.Duration
	pongTimeout       time.Duration

	mutex         sync.Mutex
	connectionIDs uint64
	slots         map[receiverKey]*receiverSlot
	closed        bool
}

// receiverKey identifies the one reserved or active delivery slot of a
// Receiver's credential generation. A newer generation is a different slot, so
// an approved recovery admits its replacement while the superseded connection
// is still closing.
type receiverKey struct {
	receiverID string
	generation int64
}

// NewReceiverSocket composes the delivery channel on its domain and publishes
// presence into the registry of this process run, which whoever reports a
// Receiver's liveness reads.
func NewReceiverSocket(receivers ReceiverAdmissions, presence *Presence, logger *slog.Logger) *ReceiverSocket {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardOutput{}, nil))
	}
	return &ReceiverSocket{
		receivers:         receivers,
		presence:          presence,
		logger:            logger,
		heartbeatInterval: heartbeatInterval,
		pongTimeout:       pongTimeout,
		slots:             map[receiverKey]*receiverSlot{},
	}
}

// SetDiagnostics supplies the recorder this socket reports its connections to,
// which the composed service installs when the channel is registered.
func (s *ReceiverSocket) SetDiagnostics(recorder observability.Recorder) {
	s.recorder = recorder
}

// Serve authenticates and reserves one delivery connection, upgrades it and
// serves it until it closes. A subscription the service rejects is reported
// before the upgrade; nil means this connection answered the client itself.
func (s *ReceiverSocket) Serve(writer http.ResponseWriter, request *http.Request, release func()) error {
	defer release()

	if !subprotocolAccepted(request) {
		return ErrSubprotocolRequired
	}

	handshake, cancelHandshake := context.WithTimeout(request.Context(), handshakeTimeout)
	defer cancelHandshake()

	// The domain's admission is the authoritative decision: it resolves the
	// credential, consumes the grant its presentation proves possession of and
	// serializes with redemption, recovery and later revocation.
	admitted, err := s.receivers.AdmitReceiver(
		handshake, observability.CredentialFrom(request.Context()), time.Now().UTC())
	if err != nil {
		return err
	}

	// The slot is what makes one connection exclusive, so it is reserved
	// atomically before the upgrade commits.
	slot, err := s.reserve(admitted)
	if err != nil {
		return err
	}
	defer s.release(slot)

	// The final commit revalidates the generation in the same boundary that
	// serializes redemption, recovery and revocation, so a generation change
	// that won the race cancels this upgrade before the 101 answers the client.
	if err := s.receivers.ConfirmReceiverConnection(handshake, admitted.ReceiverID, admitted.Generation); err != nil {
		return err
	}

	socket, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols: []string{protocol.Subprotocol},
	})
	if err != nil {
		// Accept already answered the client with the failure it produced.
		s.logger.WarnContext(request.Context(), "receiver upgrade rejected",
			slog.String("request_id", observability.RequestIDFrom(request.Context())),
			slog.String("cause", err.Error()),
		)
		return nil
	}
	socket.SetReadLimit(maxFrameBytes)
	requestID := observability.RequestIDFrom(request.Context())
	if !s.attach(slot, socket, requestID) {
		// Shutdown, or a generation change the domain committed while this
		// upgrade was committing, ended the reservation; the connection never
		// becomes Receiver presence.
		s.closeUncommitted(socket)
		return nil
	}

	s.serve(request.Context(), slot, requestID)
	return nil
}

// reserve takes the slot of the credential's generation. A generation that
// already holds a reserved or active connection is refused, and the first
// connection keeps working. Only the Receiver's current generation reaches
// here, because admission refuses every superseded one.
func (s *ReceiverSocket) reserve(credential pairing.ReceiverCredential) (*receiverSlot, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed {
		return nil, ErrReceiverUnavailable
	}
	key := receiverKey{receiverID: credential.ReceiverID, generation: credential.Generation}
	if _, held := s.slots[key]; held {
		return nil, ErrReceiverAlreadyConnected
	}
	s.connectionIDs++
	slot := &receiverSlot{connectionID: s.connectionIDs, key: key, ending: make(chan closeIntent, 1)}
	s.slots[key] = slot
	return slot, nil
}

// attach records the accepted socket of one reservation and publishes the
// Receiver's presence, unless shutdown or a generation change already ended
// that reservation. Presence is published under the same lock that owns the
// slot, so ending a Receiver's access cannot interleave here and leave a closed
// connection reported online.
func (s *ReceiverSocket) attach(slot *receiverSlot, socket *websocket.Conn, requestID string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed || s.slots[slot.key] != slot {
		return false
	}
	slot.socket = socket
	// Online begins with the committed 101, which answered the client before
	// this reservation became a connection. An acknowledgement is not presence.
	s.presence.online(slot.key.receiverID, slot.connectionID, time.Now().UTC())
	s.report(slot, requestID, http.StatusSwitchingProtocols)
	return true
}

// report records one safe outcome of a delivery connection: the status that
// committed it, or the Close status it ended with. A channel composed without a
// recorder reports nothing.
func (s *ReceiverSocket) report(slot *receiverSlot, requestID string, code int) {
	if s.recorder == nil {
		return
	}
	s.recorder.RecordWSS(observability.DiagnosticEvent{
		RequestID: requestID,
		Subject:   slot.key.receiverID,
		Code:      &code,
	})
}

// release frees one reservation, and only if this connection still owns it. The
// connection that made the Receiver online is also the one that reports it
// offline, so an older connection's cleanup cannot clear a newer connection's
// presence.
func (s *ReceiverSocket) release(slot *receiverSlot) {
	s.mutex.Lock()
	owned := s.slots[slot.key] == slot
	if owned {
		delete(s.slots, slot.key)
	}
	s.mutex.Unlock()

	if owned {
		s.presence.offline(slot.key.receiverID, slot.connectionID, time.Now().UTC())
	}
}

// closeUncommitted ends an upgrade whose reservation no longer exists: either
// the process is shutting down or the Receiver's credential generation moved on
// while this handshake was committing. Neither case may publish presence.
func (s *ReceiverSocket) closeUncommitted(socket *websocket.Conn) {
	s.mutex.Lock()
	intent := closePolicy
	if s.closed {
		intent = closeGoingAway
	}
	s.mutex.Unlock()

	code, reason := closeMeaning(intent)
	_ = socket.Close(code, reason)
}

// PairingRemoved reports one Sender's removed association to the Receiver's
// live delivery connection, so an online Receiver stops treating that Pairing
// as one that may deliver without polling. The Receiver's credential, this
// connection and every other Sender's association are unaffected, and a
// Receiver with no live connection simply receives nothing: this channel keeps
// no backlog.
func (s *ReceiverSocket) PairingRemoved(senderID, receiverID, pairingID string) {
	s.writeControl(receiverID, protocol.PairingRemoved{
		Version:   protocol.WSSVersion,
		Type:      "pairing_removed",
		SenderID:  senderID,
		PairingID: pairingID,
	}, "pairing removal", slog.String("pairing_id", pairingID))
}

// DeviceRevoked reports one Receiver's owner security revocation to its live
// delivery connections: each committed connection receives the adopted
// device_revoked control and then closes with the policy meaning, and a
// reservation still committing is closed when it tries to attach. The domain
// committed the closure before calling here, so the control is best effort and
// never delays or prevents it, and the write is bounded like every other
// service-initiated control.
func (s *ReceiverSocket) DeviceRevoked(receiverID string) {
	s.writeControl(receiverID, protocol.DeviceRevoked{
		Version:    protocol.WSSVersion,
		Type:       "device_revoked",
		ReceiverID: receiverID,
	}, "device revocation")

	// The closure follows the control even when it could not be written: what
	// the domain committed never depends on the peer reading it.
	s.EndReceiverAccess(receiverID)
}

// writeControl reports one service-initiated control message to every committed
// connection of one Receiver. The write is bounded like every other control, so
// a connection that can no longer carry the message neither delays nor prevents
// the change the domain already committed.
func (s *ReceiverSocket) writeControl(receiverID string, message any, event string, extra ...any) {
	encoded, err := json.Marshal(message)
	if err != nil {
		// Every shape is fixed by its wire type, so this cannot happen for a
		// value the service builds itself.
		s.logger.Error(event+" could not be encoded",
			slog.String("receiver_id", receiverID),
			slog.String("cause", err.Error()),
		)
		return
	}

	for _, socket := range s.committedSockets(receiverID) {
		ctx, cancel := context.WithTimeout(context.Background(), controlWriteTimeout)
		err := socket.Write(ctx, websocket.MessageText, encoded)
		cancel()
		if err != nil {
			s.logger.Warn(event+" not delivered",
				append(append([]any{slog.String("receiver_id", receiverID)}, extra...),
					slog.String("cause", err.Error()))...,
			)
		}
	}
}

// committedSockets reports the accepted sockets of one Receiver's slots, so a
// control write reaches every connection the Receiver currently holds.
func (s *ReceiverSocket) committedSockets(receiverID string) []*websocket.Conn {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	sockets := make([]*websocket.Conn, 0, 1)
	for key, slot := range s.slots {
		if key.receiverID == receiverID && slot.socket != nil {
			sockets = append(sockets, slot.socket)
		}
	}
	return sockets
}

// EndReceiverAccess ends the reserved and active delivery connections of one
// Receiver whose credential generation moved on or whose registration the
// domain closed. The domain commits that change before calling here, and only
// afterwards can the replacement generation be possessed, so this ends the
// superseded connection without ever reaching a newer valid one.
func (s *ReceiverSocket) EndReceiverAccess(receiverID string) {
	s.mutex.Lock()
	ended := make([]*receiverSlot, 0, 1)
	for key, slot := range s.slots {
		if key.receiverID == receiverID {
			delete(s.slots, key)
			ended = append(ended, slot)
		}
	}
	s.mutex.Unlock()

	for _, slot := range ended {
		// A committed connection stops making the Receiver online here; a
		// reservation that has not committed is closed when it attaches.
		s.presence.offline(receiverID, slot.connectionID, time.Now().UTC())
		// The connection's own goroutine answers the peer, so ending access
		// never waits for that peer's Close handshake.
		select {
		case slot.ending <- closePolicy:
		default:
		}
	}
}

// serve holds one committed connection open until its client closes it, the
// heartbeat finds it gone, or the service ends it. The reader accepts the
// Receipt frames of this channel and attributes each one to the Receiver this
// connection authenticated; anything else this direction does not accept ends
// the connection with its own meaning.
func (s *ReceiverSocket) serve(ctx context.Context, slot *receiverSlot, requestID string) {
	ctx, stopKeepAlive := context.WithCancel(ctx)
	kept := make(chan struct{})
	go func() {
		defer close(kept)
		s.keepAlive(ctx, slot)
	}()

	for {
		messageType, data, err := slot.socket.Read(ctx)
		if err != nil {
			break
		}
		if intent, accepted := s.accept(slot.key.receiverID, messageType, data); !accepted {
			slot.end(intent)
			break
		}
	}

	// The reader owns the end of the connection, so it also ends the connection's
	// liveness goroutine and joins it before the slot is released.
	stopKeepAlive()
	<-kept
	// The service's own close owns its status; a peer that left first ends this
	// connection normally.
	code := int(slot.closeCode.Load())
	if code == 0 {
		code = int(websocket.StatusNormalClosure)
	}
	s.report(slot, requestID, code)
}

// keepAlive owns the liveness of one committed connection. It asks the peer
// whether it is still there, ends a connection that stopped answering, and
// answers a connection the service ended, so only this connection's own
// goroutine performs its close.
func (s *ReceiverSocket) keepAlive(ctx context.Context, slot *receiverSlot) {
	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case intent := <-slot.ending:
			slot.end(intent)
			return
		case <-ticker.C:
			pong, cancel := context.WithTimeout(ctx, s.pongTimeout)
			err := slot.socket.Ping(pong)
			cancel()
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				// The connection ended first; its own close owns the cleanup.
				return
			}
			s.logger.WarnContext(ctx, "receiver heartbeat failed",
				slog.String("receiver_id", slot.key.receiverID),
				slog.Int64("generation", slot.key.generation),
				slog.String("cause", err.Error()),
			)
			if !slot.claim(int(websocket.StatusAbnormalClosure)) {
				// A closer that claimed first owns this connection's end.
				return
			}
			_ = slot.socket.CloseNow()
			return
		}
	}
}

// Close ends every reserved or active delivery connection and refuses the
// handshakes still arriving, so bounded shutdown does not wait for a reader
// that would otherwise block until its transport failed.
func (s *ReceiverSocket) Close() {
	code, reason := closeMeaning(closeGoingAway)

	s.mutex.Lock()
	s.closed = true
	sockets := make([]*websocket.Conn, 0, len(s.slots))
	for _, slot := range s.slots {
		// Shutdown ends the connections no other closer claimed, so each
		// record carries the meaning its peer received.
		if !slot.claim(int(code)) {
			continue
		}
		if slot.socket != nil {
			sockets = append(sockets, slot.socket)
		}
	}
	s.mutex.Unlock()

	for _, socket := range sockets {
		_ = socket.Close(code, reason)
	}
}
