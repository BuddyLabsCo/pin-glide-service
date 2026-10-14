package wss

import (
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// frameError is one business outcome of a received frame. A recoverable one is
// reported to its sender and keeps the connection, because the session may
// still continue; an unrecoverable one also ends the connection with the
// standard Close meaning.
type frameError struct {
	intent      closeIntent
	code        protocol.ErrorCode
	detail      string
	recoverable bool
}

// Error satisfies the error interface so a frame failure can be returned from
// the relay callback.
func (e *frameError) Error() string {
	return e.detail
}

// protocolClose builds an unrecoverable frame failure with a Close meaning.
func protocolClose(intent closeIntent, cause error, detail string) error {
	code := frameErrorCodeOf(cause)
	return &frameError{intent: intent, code: code, detail: detail}
}

// recoverable builds a frame failure that keeps the connection open.
func recoverable(code protocol.ErrorCode, detail string) error {
	return &frameError{code: code, detail: detail, recoverable: true}
}

// decodeRelay validates one pairing_relay frame and returns it. Only shape and
// encoding are checked here; the session's authority and state decide whether
// the frame may be relayed at all.
func decodeRelay(frame []byte) (protocol.PairingRelayIn, error) {
	var message protocol.PairingRelayIn
	if err := json.Unmarshal(frame, &message); err != nil {
		return protocol.PairingRelayIn{}, errMalformedFrame{}
	}
	if message.Version != protocol.WSSVersion || message.Type != "pairing_relay" || message.SessionID == "" {
		return protocol.PairingRelayIn{}, errMalformedFrame{}
	}
	// The published message definition requires payload, and the adopted
	// profile has no empty handshake message, so an absent or empty payload is
	// not a frame this channel carries.
	if message.Payload == "" {
		return protocol.PairingRelayIn{}, errMalformedFrame{}
	}
	if _, err := base64.StdEncoding.DecodeString(message.Payload); err != nil {
		return protocol.PairingRelayIn{}, errMalformedFrame{}
	}
	return message, nil
}

// errMalformedFrame marks a frame whose shape, version, type or encoding does
// not match the published pairing_relay message.
type errMalformedFrame struct{}

func (errMalformedFrame) Error() string { return "malformed pairing frame" }

// frameErrorCodeOf names the stable code a frame failure reports. Only the
// service's own outcomes are translated; anything else is unexpected.
func frameErrorCodeOf(cause error) protocol.ErrorCode {
	var malformedFrame errMalformedFrame
	switch {
	case errors.Is(cause, pairing.ErrSessionExpired):
		return protocol.PairingSessionExpired
	case errors.Is(cause, pairing.ErrSessionClosed):
		return protocol.PairingSessionClosed
	case errors.Is(cause, pairing.ErrCredentialExpired):
		return protocol.CredentialExpired
	case errors.Is(cause, pairing.ErrCredentialInvalid):
		return protocol.CredentialInvalid
	case errors.Is(cause, pairing.ErrSessionNotFound):
		return protocol.ResourceNotFound
	case errors.Is(cause, pairing.ErrStorageUnavailable):
		return protocol.ServiceUnavailable
	case errors.As(cause, &malformedFrame):
		return protocol.InvalidRequest
	default:
		return protocol.InternalError
	}
}

// frameDetail is the safe explanation a frame failure carries. It never
// includes the rejected frame or any credential.
func frameDetail(err error) string {
	var malformed errMalformedFrame
	if errors.As(err, &malformed) {
		return "The frame is not a valid pairing_relay message."
	}
	return "The Pairing Session cannot relay this frame."
}

// errUnreachablePeer marks a relay attempt without a peer connection. It is
// recoverable: the session may continue once the peer connects.
func errUnreachablePeer() error {
	return recoverable(protocol.PairingNotReady, "The other participant is not connected.")
}
