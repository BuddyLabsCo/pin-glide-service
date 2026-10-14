package protocol

import "time"

// PairingSessionView is the published view of one Pairing Session, shared by
// the HTTP operations and the pairing channel so both report one shape.
type PairingSessionView struct {
	SessionID  string    `json:"sessionId" doc:"Opaque session identifier the QR references"`
	Mode       string    `json:"mode" doc:"Fixed session mode: new_receiver, add_sender or recover_receiver" enum:"new_receiver,add_sender,recover_receiver"`
	ReceiverID string    `json:"receiverId" doc:"Receiver-side target fixed at creation"`
	State      string    `json:"state" doc:"Session state; expiry is reported as expired" enum:"awaiting_authorization,awaiting_confirmation,credential_ready,credential_issued,completed,canceled,expired,superseded"`
	ExpiresAt  time.Time `json:"expiresAt" doc:"Absolute end of the session and of its restricted credential"`
	// The confirmation flags are populated by the completion slice; they stay
	// false until a participant reports its side.
	SenderConfirmed   bool   `json:"senderConfirmed" doc:"Whether the Sender side reported key confirmation"`
	ReceiverConfirmed bool   `json:"receiverConfirmed" doc:"Whether the Receiver side reported key confirmation"`
	SenderID          string `json:"senderId,omitempty" doc:"Claiming Sender; absent until a claim succeeds"`
	PairingID         string `json:"pairingId,omitempty" doc:"Reserved Pairing ID; absent for recovery sessions and unclaimed new sessions"`
}

// ReceiverAssociation is the published view of one of a Sender's own active
// Receiver associations: the Pairing that grants forwarding, the Sender's own
// alias and the Receiver's current presence. It carries no credential and no
// other Sender's association, and a pending reservation is not one yet.
type ReceiverAssociation struct {
	ReceiverID     string    `json:"receiverId" doc:"Receiver this association forwards to"`
	PairingID      string    `json:"pairingId" doc:"Pairing ID both clients stored the Pairing key under"`
	Alias          string    `json:"alias" doc:"The owning Sender's own label for this association; empty when unset"`
	Presence       string    `json:"presence" enum:"online,offline" doc:"Whether this Receiver currently holds its committed delivery connection"`
	StateChangedAt time.Time `json:"stateChangedAt" doc:"When the Receiver's published presence last changed, in RFC 3339 UTC"`
}

// Receiver presence values the published association view reports.
const (
	// PresenceOnline marks a Receiver that currently holds its one committed
	// delivery connection.
	PresenceOnline = "online"
	// PresenceOffline marks every other Receiver, including one this process
	// run has never observed.
	PresenceOffline = "offline"
)
