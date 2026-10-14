package wss

import (
	"context"
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

// ErrReceiverAlreadyConnected reports a Receiver that already holds the one
// reserved or active delivery connection. A rejected second handshake never
// evicts the first.
var ErrReceiverAlreadyConnected = errors.New("receiver already connected")

// ErrReceiverUnavailable reports delivery admission during shutdown, so a
// handshake still arriving cannot extend the draining process.
var ErrReceiverUnavailable = errors.New("receiver channel unavailable")

// ReceiverAdmissions is the pairing domain as the delivery channel needs it:
// the domain decides whether a presented credential authorizes the Receiver
// and consumes the issuance grant that presentation proves possession of.
type ReceiverAdmissions interface {
	AdmitReceiver(ctx context.Context, credential string, now time.Time) (pairing.ReceiverCredential, error)
}

// ReceiverSocket serves the single R-authenticated delivery channel of each
// Receiver. It owns the upgrade and the reserved/active slot; the domain owns
// credential validity and possession. Delivery and Receipt traffic joins this
// connection with live delivery.
type ReceiverSocket struct {
	receivers ReceiverAdmissions
	logger    *slog.Logger

	mutex  sync.Mutex
	slots  map[string]*receiverSlot
	closed bool
}

// receiverSlot is the reserved connection of one Receiver. It is reserved
// before the upgrade commits and released only by the connection that owns it,
// so a stale callback cannot remove a newer connection.
type receiverSlot struct {
	credential pairing.ReceiverCredential
	socket     *websocket.Conn
}

// NewReceiverSocket composes the delivery channel on its domain.
func NewReceiverSocket(receivers ReceiverAdmissions, logger *slog.Logger) *ReceiverSocket {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardOutput{}, nil))
	}
	return &ReceiverSocket{
		receivers: receivers,
		logger:    logger,
		slots:     map[string]*receiverSlot{},
	}
}

// Serve authenticates and reserves one delivery connection, upgrades it and
// serves it until it closes. A subscription the service rejects is reported
// before the upgrade; nil means this connection answered the client itself.
func (s *ReceiverSocket) Serve(writer http.ResponseWriter, request *http.Request, release func()) error {
	defer release()

	// The domain's admission is the authoritative decision: it resolves the
	// credential, consumes the grant its presentation proves possession of and
	// serializes with redemption, recovery and later revocation.
	admitted, err := s.receivers.AdmitReceiver(
		request.Context(), observability.CredentialFrom(request.Context()), time.Now().UTC())
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
	if !s.attach(slot, socket) {
		// Shutdown ended the reservation while the upgrade was committing.
		_ = socket.Close(websocket.StatusGoingAway, "service_shutdown")
		return nil
	}
	s.serve(request.Context(), socket)
	return nil
}

// reserve takes the Receiver's one slot. A Receiver that already holds a
// reserved or active connection is refused, and the first connection keeps
// working.
func (s *ReceiverSocket) reserve(credential pairing.ReceiverCredential) (*receiverSlot, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed {
		return nil, ErrReceiverUnavailable
	}
	if _, held := s.slots[credential.ReceiverID]; held {
		return nil, ErrReceiverAlreadyConnected
	}
	slot := &receiverSlot{credential: credential}
	s.slots[credential.ReceiverID] = slot
	return slot, nil
}

// attach records the accepted socket of one reservation, unless shutdown
// already ended that reservation.
func (s *ReceiverSocket) attach(slot *receiverSlot, socket *websocket.Conn) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.slots[slot.credential.ReceiverID] != slot {
		return false
	}
	slot.socket = socket
	return true
}

// release frees one reservation, and only if this connection still owns it.
func (s *ReceiverSocket) release(slot *receiverSlot) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.slots[slot.credential.ReceiverID] == slot {
		delete(s.slots, slot.credential.ReceiverID)
	}
}

// serve holds the connection open until the client closes it or sends a
// frame. The delivery and Receipt messages this channel carries arrive with
// live delivery; until they are published, an application frame is a protocol
// error rather than traffic the service silently ignores.
func (s *ReceiverSocket) serve(ctx context.Context, socket *websocket.Conn) {
	messageType, _, err := socket.Read(ctx)
	if err != nil {
		return
	}
	if messageType != websocket.MessageText {
		_ = socket.Close(websocket.StatusInvalidFramePayloadData, "invalid_payload")
		return
	}
	_ = socket.Close(websocket.StatusProtocolError, "protocol_error")
}

// Close ends every reserved or active delivery connection and refuses the
// handshakes still arriving, so bounded shutdown does not wait for a reader
// that would otherwise block until its transport failed.
func (s *ReceiverSocket) Close() {
	s.mutex.Lock()
	s.closed = true
	sockets := make([]*websocket.Conn, 0, len(s.slots))
	for _, slot := range s.slots {
		if slot.socket != nil {
			sockets = append(sockets, slot.socket)
		}
	}
	s.mutex.Unlock()

	for _, socket := range sockets {
		_ = socket.Close(websocket.StatusGoingAway, "service_shutdown")
	}
}
