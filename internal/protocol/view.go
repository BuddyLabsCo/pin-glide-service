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
