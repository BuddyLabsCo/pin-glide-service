package wss

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// PairingSessions is the pairing domain as the pairing channel needs it: the
// transport maps frames and connections, and the domain decides authority,
// state and whether a message may be relayed.
type PairingSessions interface {
	// SessionByID reads the current state of one session.
	SessionByID(ctx context.Context, sessionID string) (pairing.Session, error)
	// Resolve presents one restricted B credential and reports its session
	// and side, or an expired/invalid credential outcome.
	Resolve(ctx context.Context, token string, now time.Time) (pairing.Resolved, error)
	// SenderOf resolves one Sender management credential M.
	SenderOf(ctx context.Context, token string) (string, error)
}

// PairingSocket serves the restricted pairing channel of one session. It owns
// the upgrade and the frame loop; the domain owns every authority decision.
type PairingSocket struct {
	sessions PairingSessions
	hub      *Hub
	logger   *slog.Logger
}

// NewPairingSocket composes the pairing channel on its domain and registry.
func NewPairingSocket(sessions PairingSessions, hub *Hub, logger *slog.Logger) *PairingSocket {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardOutput{}, nil))
	}
	return &PairingSocket{sessions: sessions, hub: hub, logger: logger}
}

// Serve upgrades one authenticated subscription and serves it until the
// connection closes. The admitted work keeps its token for the connection's
// whole lifetime, so bounded shutdown can join it. It returns the failure of a
// subscription the service rejected before upgrading, and nil once the
// connection answered the client itself.
func (s *PairingSocket) Serve(writer http.ResponseWriter, request *http.Request, sessionID string, release func()) error {
	defer release()

	now := time.Now().UTC()
	participant, err := s.authenticate(request.Context(), sessionID, now)
	if err != nil {
		return err
	}

	socket, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols: []string{protocol.Subprotocol},
	})
	if err != nil {
		// Accept already answered the client with the failure it produced.
		s.logger.WarnContext(request.Context(), "pairing upgrade rejected",
			slog.String("request_id", observability.RequestIDFrom(request.Context())),
			slog.String("session_id", sessionID),
			slog.String("cause", err.Error()),
		)
		return nil
	}
	socket.SetReadLimit(maxFrameBytes)

	connection := NewConnection(socket, participant.Session.ID, participant.Side, s.logger)
	connection.Send(snapshot(participant.Session, now))
	s.hub.Subscribe(connection)
	defer s.hub.Unsubscribe(connection)

	connection.Serve(request.Context(), func(frame []byte) error {
		return s.relay(request.Context(), participant, frame)
	})
	return nil
}

// authenticate resolves the upgrade credential to one participant. A
// management credential of the session's claiming Sender is the Sender side;
// the restricted session credential is the Receiver side. The caller never
// selects its own side.
func (s *PairingSocket) authenticate(ctx context.Context, sessionID string, now time.Time) (pairing.Resolved, error) {
	token := observability.CredentialFrom(ctx)
	session, err := s.sessions.SessionByID(ctx, sessionID)
	if err != nil {
		return pairing.Resolved{}, err
	}
	if senderID, err := s.sessions.SenderOf(ctx, token); err == nil {
		// The credential's validity is decided before the protected session
		// state, so each side reports its own distinct outcome. The session's
		// lifetime ends its channel too, and nothing extends it.
		if session.Expired(now) {
			return pairing.Resolved{}, pairing.ErrSessionExpired
		}
		if session.AuthorizedSenderID != senderID {
			return pairing.Resolved{}, pairing.ErrSessionNotFound
		}
		// A session that completed or closed cannot open its channel either;
		// one whose Pairing activated but whose credential grant is still
		// outstanding keeps it for the current view.
		if session.ClosesChannel(now) {
			return pairing.Resolved{}, pairing.ErrSessionClosed
		}
		return pairing.Resolved{Session: session, Side: pairing.SideSender}, nil
	} else if !isCredentialOutcome(err) {
		return pairing.Resolved{}, err
	}

	// Resolve decides the restricted credential's own validity, including the
	// expiry it shares with its session, before the session state is protected.
	resolved, err := s.sessions.Resolve(ctx, token, now)
	if err != nil {
		return pairing.Resolved{}, err
	}
	if resolved.Session.ID != sessionID {
		// A restricted credential authorizes nothing outside its own session,
		// and the caller cannot confirm another session exists.
		return pairing.Resolved{}, pairing.ErrCredentialInvalid
	}
	// A session that completed or closed cannot open its channel either.
	if resolved.Session.ClosesChannel(now) {
		return pairing.Resolved{}, pairing.ErrSessionClosed
	}
	return resolved, nil
}

// relay forwards one opaque handshake frame to the other participant after
// checking its shape and the session's current state. The payload is never
// interpreted, persisted or replayed.
func (s *PairingSocket) relay(ctx context.Context, sender pairing.Resolved, frame []byte) error {
	message, err := decodeRelay(frame)
	if err != nil {
		return protocolClose(closeProtocolError, err, frameDetail(err))
	}
	if message.SessionID != sender.Session.ID {
		return protocolClose(closeProtocolError, errMalformedFrame{}, frameDetail(errMalformedFrame{}))
	}

	session, err := s.sessions.SessionByID(ctx, sender.Session.ID)
	if err != nil {
		return protocolClose(closePolicy, err, frameDetail(err))
	}
	if err := session.TerminalError(time.Now().UTC()); err != nil {
		return protocolClose(closePolicy, err, frameDetail(err))
	}
	if !session.Live(time.Now().UTC()) {
		return protocolClose(closePolicy, pairing.ErrSessionClosed, frameDetail(pairing.ErrSessionClosed))
	}

	relayed := protocol.PairingRelayOut{
		Version:   protocol.WSSVersion,
		Type:      "pairing_relay",
		SessionID: session.ID,
		Side:      string(sender.Side),
		Payload:   message.Payload,
	}
	if !s.hub.Relay(session.ID, sender.Side, relayed) {
		return errUnreachablePeer()
	}
	return nil
}

// SessionChanged tells every subscribed participant the session's current
// state and applies the two control effects the state carries: the Receiver
// side alone learns that its credential is ready to redeem, and a session that
// can no longer pair ends its live channels. The domain calls it after every
// committed change, so a connected peer follows the state without polling or
// replay.
func (s *PairingSocket) SessionChanged(sessionID string, view pairing.View) {
	s.hub.SendState(sessionID, protocol.PairingState{
		Version: protocol.WSSVersion,
		Type:    "pairing_state",
		Session: wireView(view),
	})

	switch view.State {
	case pairing.StateCredentialReady:
		// Only the restricted B side may redeem the credential, so only it is
		// told that the grant became redeemable.
		s.hub.SendTo(sessionID, pairing.SideReceiver, protocol.CredentialReady{
			Version:    protocol.WSSVersion,
			Type:       "credential_ready",
			SessionID:  view.SessionID,
			ReceiverID: view.ReceiverID,
			ExpiresAt:  view.ExpiresAt,
		})
	case pairing.StateCompleted, pairing.StateCanceled, pairing.StateSuperseded, pairing.StateExpired:
		s.hub.CloseSession(sessionID)
	}
}

// snapshot builds one session state message.
func snapshot(session pairing.Session, now time.Time) protocol.PairingState {
	return protocol.PairingState{
		Version: protocol.WSSVersion,
		Type:    "pairing_state",
		Session: wireView(session.View(now)),
	}
}

// isCredentialOutcome reports whether an authentication failure is one of the
// credential outcomes, which the receive-side resolution tries next. Any other
// failure is a real fault and must not be downgraded to an invalid credential.
func isCredentialOutcome(err error) bool {
	return errors.Is(err, pairing.ErrCredentialMissing) ||
		errors.Is(err, pairing.ErrCredentialInvalid) ||
		errors.Is(err, pairing.ErrCredentialExpired)
}

// wireView maps the domain view to the shared wire shape.
func wireView(view pairing.View) protocol.PairingSessionView {
	return protocol.PairingSessionView{
		SessionID:         view.SessionID,
		Mode:              string(view.Mode),
		ReceiverID:        view.ReceiverID,
		State:             string(view.State),
		ExpiresAt:         view.ExpiresAt,
		SenderConfirmed:   view.SenderConfirmed,
		ReceiverConfirmed: view.ReceiverConfirmed,
		SenderID:          view.SenderID,
		PairingID:         view.PairingID,
	}
}
