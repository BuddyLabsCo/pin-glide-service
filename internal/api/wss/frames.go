package wss

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"unicode/utf8"

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

// relayCloseIntent names the close meaning of one frame failure: bytes the
// service cannot read as UTF-8 are an invalid payload, and every other shape,
// version or type mismatch is a protocol error.
func relayCloseIntent(err error) closeIntent {
	var encoding errInvalidFrameEncoding
	if errors.As(err, &encoding) {
		return closeInvalidPayload
	}
	return closeProtocolError
}

// decodeRelay validates one pairing_relay frame and returns it. Only shape and
// encoding are checked here; the session's authority and state decide whether
// the frame may be relayed at all.
func decodeRelay(frame []byte) (protocol.PairingRelayIn, error) {
	if !utf8.Valid(frame) {
		return protocol.PairingRelayIn{}, errInvalidFrameEncoding{}
	}
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

// errInvalidFrameEncoding marks a text frame the service cannot read as UTF-8.
// It maps to the invalid-payload meaning rather than a protocol failure.
type errInvalidFrameEncoding struct{}

func (errInvalidFrameEncoding) Error() string { return "frame is not valid UTF-8 text" }

// frameCode maps one typed frame failure to the stable code the service
// reports for it. The published description carries exactly this set of codes,
// so a business outcome added here belongs in the description as well.
type frameCode struct {
	cause error
	code  protocol.ErrorCode
}

// frameCodes lists every business outcome a frame can report. Only the
// service's own outcomes are translated; anything else is unexpected.
var frameCodes = []frameCode{
	{pairing.ErrSessionExpired, protocol.PairingSessionExpired},
	{pairing.ErrSessionClosed, protocol.PairingSessionClosed},
	{pairing.ErrCredentialExpired, protocol.CredentialExpired},
	{pairing.ErrCredentialInvalid, protocol.CredentialInvalid},
	{pairing.ErrSessionNotFound, protocol.ResourceNotFound},
	{pairing.ErrStorageUnavailable, protocol.ServiceUnavailable},
	{errMalformedFrame{}, protocol.InvalidRequest},
	{errInvalidFrameEncoding{}, protocol.InvalidRequest},
}

// frameErrorCodeOf names the stable code a frame failure reports.
func frameErrorCodeOf(cause error) protocol.ErrorCode {
	var outcome *frameError
	if errors.As(cause, &outcome) {
		return outcome.code
	}
	for _, outcome := range frameCodes {
		if errors.Is(cause, outcome.cause) {
			return outcome.code
		}
	}
	return protocol.InternalError
}

// frameDetail is the safe explanation a frame failure carries. It never
// includes the rejected frame or any credential.
func frameDetail(err error) string {
	var malformed errMalformedFrame
	var encoding errInvalidFrameEncoding
	if errors.As(err, &malformed) || errors.As(err, &encoding) {
		return "The frame is not a valid pairing_relay message."
	}
	return "The Pairing Session cannot relay this frame."
}

// errUnreachablePeer marks a relay attempt without a peer connection. It is
// recoverable: the session may continue once the peer connects.
func errUnreachablePeer() error {
	return recoverable(protocol.PairingNotReady, "The other participant is not connected.")
}
