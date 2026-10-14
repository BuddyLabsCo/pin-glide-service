// Package identity exposes the Sender authentication operations of the
// identity domain over the service's HTTP surface.
package identity

import "time"

// IssuedCredential is one issued credential as its holder receives it. This
// payload is the only place a raw service credential appears.
type IssuedCredential struct {
	Token     string    `json:"token" doc:"Raw opaque credential; send it only in the Authorization header of an authenticated HTTPS or WSS request"`
	ExpiresAt time.Time `json:"expiresAt" doc:"Actual expiry of this credential in RFC 3339 UTC"`
}

// CredentialBundle is the accepted login success payload. It carries one
// Sender's issued management, upload and renewal credentials together with the
// identity they belong to.
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

type loginInput struct {
	Body WeChatLoginRequest
}

type loginOutput struct {
	Body CredentialBundle
}
