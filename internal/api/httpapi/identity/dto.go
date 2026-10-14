// Package identity exposes the Sender authentication operations of the
// identity domain over the service's HTTP surface.
package identity

import (
	"time"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// IssuedCredential is one issued credential as its holder receives it. This
// payload is the only place a raw service credential appears.
type IssuedCredential struct {
	Token     string    `json:"token" doc:"Raw opaque credential; send it only in the Authorization header of an authenticated HTTPS or WSS request"`
	ExpiresAt time.Time `json:"expiresAt" doc:"Actual expiry of this credential in RFC 3339 UTC"`
}

// CredentialBundle is one issued Sender credential family as its holder
// receives it: the identity it belongs to plus its management, upload and
// renewal credentials.
type CredentialBundle struct {
	AccountID   string           `json:"accountId" doc:"Internal Account identifier that owns this Sender"`
	SenderID    string           `json:"senderId" doc:"Sender registration ID (deviceId); save it to restore this installation"`
	UserSession IssuedCredential `json:"userSession" doc:"Management credential, valid for three hours"`
	Uploader    IssuedCredential `json:"uploader" doc:"Upload and model-credential access, valid for three hours"`
	Refresh     IssuedCredential `json:"refresh" doc:"Renewal credential with a sliding seven-day lifetime"`
}

// WeChatLoginRequest is the accepted POST /v1/auth/wechat body.
type WeChatLoginRequest struct {
	WeChatCode string `json:"wechatCode" required:"true" minLength:"1" maxLength:"512" doc:"Fresh single-use WeChat authorization code obtained by the Android client"`
	SenderID   string `json:"senderId,omitempty" maxLength:"128" doc:"Saved Sender ID to restore; omit it to register this installation as a new Sender"`
}

// RefreshRequest is the accepted POST /v1/auth/refresh body.
type RefreshRequest struct {
	RefreshToken string `json:"refreshToken" required:"true" minLength:"1" maxLength:"512" doc:"Current renewal credential F; the client persists its operation state before sending it, and it never travels in a URL"`
	OperationID  string `json:"operationId" required:"true" minLength:"1" maxLength:"128" doc:"Client-generated identifier of one renewal operation, reused by that operation's retransmission"`
	Attempt      int    `json:"attempt" required:"true" minimum:"0" maximum:"1" doc:"0 for the original request, 1 for its one permitted retransmission"`
}

// RefreshBundle is the accepted renewal success payload: the replaced family
// plus the operation attempt this response committed.
type RefreshBundle struct {
	CredentialBundle
	OperationID string `json:"operationId" doc:"Operation identifier this renewal committed"`
	Attempt     int    `json:"attempt" doc:"Attempt number this renewal committed"`
}

type loginInput struct {
	Body WeChatLoginRequest
}

type loginOutput struct {
	Body CredentialBundle
}

type refreshInput struct {
	Body RefreshRequest
}

type refreshOutput struct {
	Body RefreshBundle
}

// credentialBundle maps one issued family to the payload its holder receives.
// The raw tokens appear only here.
func credentialBundle(issued domain.Bundle) CredentialBundle {
	return CredentialBundle{
		AccountID: issued.AccountID,
		SenderID:  issued.SenderID,
		UserSession: IssuedCredential{
			Token:     issued.UserSession.Value,
			ExpiresAt: issued.UserSession.ExpiresAt,
		},
		Uploader: IssuedCredential{
			Token:     issued.Uploader.Value,
			ExpiresAt: issued.Uploader.ExpiresAt,
		},
		Refresh: IssuedCredential{
			Token:     issued.Refresh.Value,
			ExpiresAt: issued.Refresh.ExpiresAt,
		},
	}
}
