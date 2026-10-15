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

// LogoutRequest is the accepted POST /v1/auth/logout body. The body itself is
// optional: a Sender that still holds access authority sends only the
// Authorization header, and one whose access has expired sends its renewal
// credential here instead. Switching Accounts removes the Sender's own
// associations, so only its management credential may request that mode and it
// must arrive in the Authorization header.
type LogoutRequest struct {
	Mode         string `json:"mode,omitempty" enum:"logout,switch_account" default:"logout" doc:"logout revokes this Sender's credentials and preserves its Pairings; switch_account also removes its own Pairings and requires its management credential in the Authorization header"`
	RefreshToken string `json:"refreshToken,omitempty" minLength:"1" maxLength:"512" doc:"Renewal credential F; present exactly when the request carries no Authorization credential. switch_account always requires a management credential in the header instead"`
}

// WeChatProofRequest is the accepted POST /v1/devices/{id}/revoke body: fresh
// authorization of the Account that owns the registration, which is what makes
// this an owner action rather than an ordinary Sender one.
type WeChatProofRequest struct {
	WeChatCode string `json:"wechatCode" required:"true" minLength:"1" maxLength:"512" doc:"Fresh single-use WeChat authorization code obtained by the Account owner"`
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

type logoutInput struct {
	// A pointer makes the whole body optional, so a Sender that holds a valid
	// access credential sends none.
	Body *LogoutRequest
}

type revokeDeviceInput struct {
	ID   string `path:"id" maxLength:"128" doc:"Sender or Receiver registration ID (deviceId) to revoke permanently"`
	Body WeChatProofRequest
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
