package protocol

import "time"

// WSSVersion is the only application message version the v1 surface accepts.
const WSSVersion = 1

// Subprotocol is the WebSocket subprotocol the service negotiates.
const Subprotocol = "pin-glide.v1"

// WSSCode is one stable machine-readable code the WSS surface reports. The
// published description carries the same set, so a code the service can send
// belongs in WSSCodes.
type WSSCode struct {
	// Code is the value clients match on.
	Code ErrorCode
	// Description is the stable English explanation the published description
	// carries for this code.
	Description string
}

// WSSCodes lists every business code the WSS surface reports in an error
// message. An unfamiliar code is still a failure for a client, so this set only
// grows with a compatible addition and never changes an existing meaning.
var WSSCodes = []WSSCode{
	{InvalidRequest, "The received frame is not a message this direction accepts."},
	{ResourceNotFound, "The Pairing Session does not exist or is not visible to this participant."},
	{CredentialInvalid, "The presented credential does not authorize this Pairing Session."},
	{CredentialExpired, "The presented credential has expired."},
	{PairingSessionExpired, "The Pairing Session passed its absolute lifetime."},
	{PairingSessionClosed, "The Pairing Session may no longer relay; it completed, was canceled or was superseded."},
	{PairingNotReady, "The session cannot relay this frame yet, for example because the other participant is not connected."},
	{ServiceUnavailable, "The service or its storage is temporarily unavailable."},
	{InternalError, "An unexpected server failure occurred."},
}

// CloseCode is one documented WebSocket Close meaning of the v1 surface.
type CloseCode struct {
	// Code is the standard WebSocket status code.
	Code int
	// Reason is the short machine-readable reason the service sends with it.
	// It is empty only for a close that sends no reason.
	Reason string
	// Description states when the service ends a connection this way.
	Description string
}

// CloseCodes lists the adopted close meanings in the order the published
// description documents them. A normal close carries no reason.
var CloseCodes = []CloseCode{
	{1000, "", "The connection ended normally."},
	{1001, "service_shutdown", "The service is shutting down."},
	{1002, "protocol_error", "The connection sent a frame that is not an accepted message of this protocol version, type or shape."},
	{1007, "invalid_payload", "The connection sent text that is not valid UTF-8."},
	{1008, "policy_violation", "The connection's credential, session or policy no longer authorizes it."},
	{1011, "internal_error", "An unexpected server failure ended the connection."},
}

// CloseReason returns the machine-readable reason the published description
// states for one Close code, so the connections that close and the description
// carry one authority. A code this surface does not document closes with no
// reason.
func CloseReason(code int) string {
	for _, close := range CloseCodes {
		if close.Code == code {
			return close.Reason
		}
	}
	return ""
}

// PairingRelayIn is one opaque handshake message a participant sends. The
// service neither inspects nor keeps the payload.
type PairingRelayIn struct {
	Version   int    `json:"version" doc:"Application message version; always 1"`
	Type      string `json:"type" doc:"Message type; pairing_relay on this direction"`
	SessionID string `json:"sessionId" doc:"Pairing Session this handshake belongs to"`
	Payload   string `json:"payload" doc:"Padded-Base64 opaque handshake bytes"`
}

// PairingRelayOut is one relayed handshake message as its peer receives it.
// The payload is the sender's bytes unchanged; the side is derived from
// authentication rather than chosen by the caller.
type PairingRelayOut struct {
	Version   int    `json:"version" doc:"Application message version; always 1"`
	Type      string `json:"type" doc:"Message type; pairing_relay"`
	SessionID string `json:"sessionId" doc:"Pairing Session this handshake belongs to"`
	Side      string `json:"side" doc:"Participant the bytes came from; sender or receiver"`
	Payload   string `json:"payload" doc:"The peer's padded-Base64 handshake bytes, unchanged"`
}

// PairingState is the initial and on-change session snapshot. It is also the
// only place either participant reads the session's current state.
type PairingState struct {
	Version int                `json:"version" doc:"Application message version; always 1"`
	Type    string             `json:"type" doc:"Message type; pairing_state"`
	Session PairingSessionView `json:"session" doc:"Current Pairing Session view"`
}

// CredentialReady tells the Receiver side of a new_receiver session that its
// first Receiver credential can be redeemed. It is sent only to the restricted
// B subscriber and carries no credential, QR PSK or client key material.
type CredentialReady struct {
	Version    int       `json:"version" doc:"Application message version; always 1"`
	Type       string    `json:"type" doc:"Message type; credential_ready"`
	SessionID  string    `json:"sessionId" doc:"Pairing Session whose credential is ready"`
	ReceiverID string    `json:"receiverId" doc:"Receiver the credential belongs to"`
	ExpiresAt  time.Time `json:"expiresAt" doc:"Absolute end of the restricted credential's redemption window"`
}

// PairingRemoved tells one Receiver's delivery connection that a Sender
// removed its own association with it, so the Receiver stops treating that
// Pairing as one that may deliver. The Receiver's credential, its connection
// and every other Sender's Pairing are unaffected, so the message changes
// forwarding authority without ending the channel.
type PairingRemoved struct {
	Version   int    `json:"version" doc:"Application message version; always 1"`
	Type      string `json:"type" doc:"Message type; pairing_removed"`
	SenderID  string `json:"senderId" doc:"Sender whose association was removed"`
	PairingID string `json:"pairingId" doc:"Pairing ID of the removed association"`
}

// WSSError is a recoverable application error. The connection stays open
// after it, and the shared BusinessError carries the stable code.
type WSSError struct {
	Version   int          `json:"version" doc:"Application message version; always 1"`
	Type      string       `json:"type" doc:"Message type; error"`
	RequestID string       `json:"requestId" doc:"Correlation identifier of the operation; every recoverable failure states one"`
	SessionID string       `json:"sessionId,omitempty" doc:"Pairing Session when it is readable"`
	Error     WSSErrorItem `json:"error" doc:"Stable business error"`
}

// WSSErrorItem is the BusinessError object WSS failures carry.
type WSSErrorItem struct {
	Code   string `json:"code" doc:"Stable machine-readable error code"`
	Detail string `json:"detail" doc:"Safe explanation; clients must not parse it"`
}
