package wss

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

const (
	// maxFrameBytes bounds one application frame. The adopted profile carries
	// small handshake messages, so an oversized frame is a protocol violation
	// rather than something to buffer.
	maxFrameBytes = 64 * 1024
	// writeQueue holds frames a connection is waiting to write. A peer that
	// stops reading must not block the writer of the other participant.
	writeQueue = 16
)

// close intents name why one connection ended. They map to the standard
// meanings the published description documents.
type closeIntent int

const (
	closeNothing closeIntent = iota
	closeGoingAway
	closeProtocolError
	closeInvalidPayload
	closePolicy
	closeInternal
)

// ErrSubprotocolRequired reports an upgrade that did not offer the one
// subprotocol this surface speaks. The service publishes that requirement, so
// an upgrade answer it never negotiated is refused before the 101.
var ErrSubprotocolRequired = errors.New("pin-glide.v1 subprotocol required")

// closeMeans is the published meaning of one close intent: the standard status
// code, and the short machine-readable reason the service sends with it. A
// reason never carries a secret value, so it stays safe on a transport anyone
// on the path can read.
type closeMeans struct {
	intent closeIntent
	code   websocket.StatusCode
	reason string
}

// closeCodes lists the close meanings the v1 surface uses, in the order the
// published description documents them. It is the single authority for the code
// and reason of each intent.
var closeCodes = []closeMeans{
	{intent: closeNothing, code: websocket.StatusNormalClosure, reason: ""},
	{intent: closeGoingAway, code: websocket.StatusGoingAway, reason: "service_shutdown"},
	{intent: closeProtocolError, code: websocket.StatusProtocolError, reason: "protocol_error"},
	{intent: closeInvalidPayload, code: websocket.StatusInvalidFramePayloadData, reason: "invalid_payload"},
	{intent: closePolicy, code: websocket.StatusPolicyViolation, reason: "policy_violation"},
	{intent: closeInternal, code: websocket.StatusInternalError, reason: "internal_error"},
}

// subprotocolAccepted reports whether one upgrade offered the subprotocol the
// service speaks. The library negotiates the first accepted token or none at
// all, so an upgrade that offered nothing else would otherwise reach this
// surface without its published subprotocol.
func subprotocolAccepted(request *http.Request) bool {
	for _, offered := range request.Header.Values("Sec-WebSocket-Protocol") {
		for _, token := range strings.Split(offered, ",") {
			if strings.EqualFold(strings.TrimSpace(token), protocol.Subprotocol) {
				return true
			}
		}
	}
	return false
}

// closeFrame is a queue marker that ends a connection once every frame queued
// before it has been written, so a final state message is not lost to the
// close that follows it.
type closeFrame struct {
	intent closeIntent
}

// Connection owns one accepted pairing socket: its write queue, its sequence
// of frames and the cleanup of its own registration.
type Connection struct {
	socket    *websocket.Conn
	sessionID string
	side      pairing.Side
	logger    *slog.Logger

	frames chan any
	done   chan struct{}
	once   sync.Once
}

// NewConnection wraps one accepted socket. The caller serves it and closes it
// through this type so every write is serialized.
func NewConnection(socket *websocket.Conn, sessionID string, side pairing.Side, logger *slog.Logger) *Connection {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardOutput{}, nil))
	}
	return &Connection{
		socket:    socket,
		sessionID: sessionID,
		side:      side,
		logger:    logger,
		frames:    make(chan any, writeQueue),
		done:      make(chan struct{}),
	}
}

// Send queues one application message. A closed connection drops the message
// instead of blocking its caller.
func (c *Connection) Send(message any) {
	select {
	case <-c.done:
	case c.frames <- message:
	default:
		c.logger.Warn("pairing write queue full",
			slog.String("session_id", c.sessionID),
			slog.String("side", string(c.side)),
		)
	}
}

// Serve writes queued frames and reads incoming ones until the connection
// closes. relay handles one received frame and reports the outcome of the
// business decision; this loop owns only framing and Close semantics.
func (c *Connection) Serve(ctx context.Context, relay func(frame []byte) error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.write(ctx)
	}()

	for {
		messageType, data, err := c.socket.Read(ctx)
		if err != nil {
			break
		}
		if messageType != websocket.MessageText {
			c.Close(closeInvalidPayload)
			break
		}
		// A text frame the service cannot read as UTF-8 is an invalid payload
		// rather than a protocol or business failure, so it has its own meaning.
		if !utf8.Valid(data) {
			c.Close(closeInvalidPayload)
			break
		}
		if err := relay(data); err != nil {
			if !c.reportFrameError(ctx, err) {
				break
			}
		}
	}

	c.Close(closeNothing)
	<-writerDone
}

// write drains the frame queue until the connection closes.
func (c *Connection) write(ctx context.Context) {
	for {
		select {
		case <-c.done:
			return
		case message := <-c.frames:
			if closing, ok := message.(closeFrame); ok {
				c.Close(closing.intent)
				return
			}
			encoded, err := json.Marshal(message)
			if err != nil {
				c.logger.ErrorContext(ctx, "encode pairing frame",
					slog.String("session_id", c.sessionID),
					slog.String("cause", err.Error()),
				)
				c.Close(closeInternal)
				return
			}
			if err := c.socket.Write(ctx, websocket.MessageText, encoded); err != nil {
				c.Close(closeNothing)
				return
			}
		}
	}
}

// Close ends the connection with the standard meaning of intent. It is safe
// to call more than once and from any goroutine. The library bounds its own
// close handshake, so a peer that stopped reading cannot hold it open here.
func (c *Connection) Close(intent closeIntent) {
	c.once.Do(func() {
		close(c.done)
		code, reason := closeMeaning(intent)
		_ = c.socket.Close(code, reason)
	})
}

// CloseAfterWrite ends the connection once every frame already queued has
// been written. A queue that cannot take the marker closes immediately instead,
// so a peer that stopped reading cannot hold the connection open.
func (c *Connection) CloseAfterWrite(intent closeIntent) {
	select {
	case <-c.done:
	case c.frames <- closeFrame{intent: intent}:
	default:
		c.Close(intent)
	}
}

// reportFrameError maps one business frame failure to its Close meaning and
// reports whether the connection keeps serving. A recoverable service error
// reports the shared BusinessError and leaves the session usable; an
// unrecoverable one ends the connection.
func (c *Connection) reportFrameError(ctx context.Context, err error) bool {
	var frameErr *frameError
	if !errors.As(err, &frameErr) {
		c.logger.ErrorContext(ctx, "pairing relay failed",
			slog.String("session_id", c.sessionID),
			slog.String("cause", err.Error()),
		)
		c.Close(closeInternal)
		return false
	}
	if frameErr.recoverable {
		// The session may continue, for example once its peer connects, so the
		// caller keeps its connection and only learns the outcome.
		c.Send(protocol.WSSError{
			Version:   protocol.WSSVersion,
			Type:      "error",
			RequestID: observability.RequestIDFrom(ctx),
			SessionID: c.sessionID,
			Error: protocol.WSSErrorItem{
				Code:   string(frameErr.code),
				Detail: frameErr.detail,
			},
		})
		return true
	}
	c.Close(frameErr.intent)
	return false
}

// closeMeaning maps one intent to the standard status code and a short
// machine-readable reason, never secret data.
func closeMeaning(intent closeIntent) (websocket.StatusCode, string) {
	for _, means := range closeCodes {
		if means.intent == intent {
			return means.code, means.reason
		}
	}
	return websocket.StatusNormalClosure, ""
}

// discardOutput drops the diagnostics of a connection composed without a
// logger.
type discardOutput struct{}

func (discardOutput) Write(data []byte) (int, error) { return len(data), nil }
