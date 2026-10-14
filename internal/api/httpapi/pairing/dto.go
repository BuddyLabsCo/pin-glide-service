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
// the session's purpose; receiverId is present exactly for the mode that acts
// on an existing Receiver. Creation itself carries no credential, so the
// Receiver side holds only the restricted token it receives here.
type CreateRequest struct {
	Mode       string `json:"mode" required:"true" enum:"new_receiver,recover_receiver" doc:"Fixed session mode: new_receiver allocates a Receiver, recover_receiver replaces a lost Receiver credential"`
	ReceiverID string `json:"receiverId,omitempty" maxLength:"128" doc:"Existing Receiver for recover_receiver; omit it for new_receiver"`
}

// AuthorizeRequest is the accepted POST
// /v1/pairing-sessions/{id}/authorize body.
type AuthorizeRequest struct {
	Alias      string `json:"alias,omitempty" maxLength:"256" doc:"Replacement for the claiming Sender's own alias; omit it to preserve the current alias"`
	WeChatCode string `json:"wechatCode,omitempty" maxLength:"512" doc:"Fresh WeChat authorization; required for recover_receiver"`
}

// CreatedSession is the accepted creation payload. The restricted credential
// is returned only here and only to the Receiver-side peer.
type CreatedSession struct {
	Session               protocol.PairingSessionView `json:"session" doc:"The created Pairing Session"`
	PairingToken          string                      `json:"pairingToken" doc:"Restricted five-minute Pairing credential; opens the pairing channel for this session and redeems its first Receiver credential"`
	PairingTokenExpiresAt time.Time                   `json:"pairingTokenExpiresAt" doc:"Absolute expiry of the restricted credential; the session cannot outlive it"`
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

type cancelInput struct {
	ID string `path:"id" maxLength:"128" doc:"Pairing Session ID"`
}
