package protocol

// WSSVersion is the only application message version the v1 surface accepts.
const WSSVersion = 1

// Subprotocol is the WebSocket subprotocol the service negotiates.
const Subprotocol = "pin-glide.v1"

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

// WSSError is a recoverable application error. The connection stays open
// after it, and the shared BusinessError carries the stable code.
type WSSError struct {
	Version   int          `json:"version" doc:"Application message version; always 1"`
	Type      string       `json:"type" doc:"Message type; error"`
	RequestID string       `json:"requestId,omitempty" doc:"Correlation identifier of the operation"`
	SessionID string       `json:"sessionId,omitempty" doc:"Pairing Session when it is readable"`
	Error     WSSErrorItem `json:"error" doc:"Stable business error"`
}

// WSSErrorItem is the BusinessError object WSS failures carry.
type WSSErrorItem struct {
	Code   string `json:"code" doc:"Stable machine-readable error code"`
	Detail string `json:"detail" doc:"Safe explanation; clients must not parse it"`
}
