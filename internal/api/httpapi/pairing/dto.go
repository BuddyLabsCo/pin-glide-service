// Package pairing exposes the Pairing Session operations of the pairing
// domain over the service's HTTP surface.
package pairing

import (
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// TagPairing groups the published pairing operations.
const TagPairing = "pairing"

// CreateRequest is the accepted POST /v1/pairing-sessions body. The mode fixes
// the session's purpose; receiverId is present exactly for the modes that act
// on an existing Receiver. new_receiver and recover_receiver creation carry no
// credential, so the Receiver side holds only the restricted token it receives
// here; add_sender creation presents the credential that Receiver already
// holds.
type CreateRequest struct {
	Mode       string `json:"mode" required:"true" enum:"new_receiver,add_sender,recover_receiver" doc:"Fixed session mode: new_receiver allocates a Receiver, add_sender attaches one more Sender to an existing Receiver, recover_receiver replaces a lost Receiver credential"`
	ReceiverID string `json:"receiverId,omitempty" maxLength:"128" doc:"Existing Receiver for add_sender and recover_receiver; omit it for new_receiver"`
}

// AuthorizeRequest is the accepted POST
// /v1/pairing-sessions/{id}/authorize body.
type AuthorizeRequest struct {
	Alias      string `json:"alias,omitempty" maxLength:"256" doc:"Replacement for the claiming Sender's own alias; omit it to preserve the current alias"`
	WeChatCode string `json:"wechatCode,omitempty" maxLength:"512" doc:"Fresh WeChat authorization; required for recover_receiver when no QQ proof is sent"`
	QQCode     string `json:"qqCode,omitempty" maxLength:"512" doc:"Fresh QQ authorization; required for recover_receiver when no WeChat proof is sent"`
}

// CompleteRequest is the accepted POST /v1/pairing-sessions/{id}/complete
// body. One report confirms that the authenticated side saved the derived
// Pairing key and checked its peer's key confirmation; the service never sees
// or verifies that client key material. A recovery report instead states which
// existing keys that side still holds, and only its Receiver side lists them.
type CompleteRequest struct {
	KeyConfirmation    string   `json:"keyConfirmation" required:"true" enum:"confirmed,not_available" doc:"Confirmed reports that this side saved the Pairing key and verified its peer's key confirmation; recover_receiver may instead report not_available for a key it no longer holds, and every other mode reports confirmed"`
	RetainedPairingIDs []string `json:"retainedPairingIds,omitempty" doc:"recover_receiver only, from its Receiver side: the Pairing IDs of this Receiver whose local keys survived. It must agree with keyConfirmation and name only Pairings of this Receiver"`
}

// RedeemRequest is the accepted POST
// /v1/pairing-sessions/{id}/receiver-token body. The operation ID and attempt
// order the one permitted retransmission of a redemption whose response was
// lost.
type RedeemRequest struct {
	OperationID string `json:"operationId" required:"true" minLength:"1" maxLength:"128" doc:"Credential-issuance operation identifier the client persisted before sending"`
	Attempt     int    `json:"attempt" required:"true" minimum:"0" maximum:"1" doc:"0 is the original request, 1 the one permitted retransmission of it"`
}

// CreatedSession is the accepted creation payload. The restricted credential
// is returned only here and only to the Receiver-side peer.
type CreatedSession struct {
	Session               protocol.PairingSessionView `json:"session" doc:"The created Pairing Session"`
	PairingToken          string                      `json:"pairingToken" doc:"Restricted five-minute Pairing credential; opens the pairing channel for this session and redeems its first Receiver credential"`
	PairingTokenExpiresAt time.Time                   `json:"pairingTokenExpiresAt" doc:"Absolute expiry of the restricted credential; the session cannot outlive it"`
}

// IssuedReceiverToken is the accepted redemption payload. The long-lived
// Receiver credential is returned only here and only to the side that
// authenticated with the restricted credential.
type IssuedReceiverToken struct {
	ReceiverID    string `json:"receiverId" doc:"Receiver the credential belongs to"`
	ReceiverToken string `json:"receiverToken" doc:"Long-lived Receiver credential; store it securely, it is never returned again"`
}

type createInput struct {
	Body CreateRequest
}

type createOutput struct {
	Body CreatedSession
}

type authorizeInput struct {
	ID   string `path:"id" maxLength:"128" doc:"Pairing Session ID"`
	Body AuthorizeRequest
}

type sessionOutput struct {
	Body protocol.PairingSessionView
}

type completeInput struct {
	ID   string `path:"id" maxLength:"128" doc:"Pairing Session ID"`
	Body CompleteRequest
}

type redeemInput struct {
	ID   string `path:"id" maxLength:"128" doc:"Pairing Session ID"`
	Body RedeemRequest
}

type redeemOutput struct {
	Body IssuedReceiverToken
}

type cancelInput struct {
	ID string `path:"id" maxLength:"128" doc:"Pairing Session ID"`
}
