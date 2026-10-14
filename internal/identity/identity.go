// Package identity owns Accounts, Sender installation registrations, the
// WeChat-backed login and the lifecycle of the Sender credential family.
package identity

import (
	"context"
	"errors"
	"time"
)

// Scope is the authority one Sender credential carries. A credential is valid
// only for the operation scopes it was issued for.
type Scope string

const (
	// ScopeUserSession authorizes the Sender's own management operations.
	ScopeUserSession Scope = "user_session"
	// ScopeUploader authorizes uploads, own results and model credentials.
	ScopeUploader Scope = "uploader"
	// ScopeRefresh renews that Sender's user-session and uploader bundle.
	ScopeRefresh Scope = "refresh"
)

const (
	// AccessLifetime is how long an issued M or U credential stays valid.
	AccessLifetime = 3 * time.Hour
	// RefreshLifetime is how long an issued F credential stays valid; each
	// successful renewal slides the window.
	RefreshLifetime = 7 * 24 * time.Hour
)

// WeChatIdentity is one identity the WeChat provider verified. AppID is the
// configured application the verification belongs to, so the same OpenID under
// another AppID is another Account.
type WeChatIdentity struct {
	AppID   string
	OpenID  string
	UnionID string
}

// Provider verifies a fresh WeChat authorization code. It is the external
// boundary the identity service depends on and is implemented by the WeChat
// adapter.
type Provider interface {
	Verify(ctx context.Context, wechatCode string) (WeChatIdentity, error)
}

// LoginRequest is one fresh login. An empty SenderID asks for a new
// installation registration; a supplied one may only restore an owned, active
// registration, never recreate one.
type LoginRequest struct {
	WeChatCode string
	SenderID   string
}

// Token is one issued credential and its actual expiry.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Bundle is one Sender's issued credential family. The raw tokens leave the
// service only through the response that hands them to their holder.
type Bundle struct {
	AccountID   string
	SenderID    string
	UserSession Token
	Uploader    Token
	Refresh     Token
}

// Errors the identity operations report. Callers distinguish outcomes by these
// typed causes; the HTTP surface maps them to stable business codes, and
// nothing else about them is public.
var (
	// ErrWeChatAuthorizationInvalid marks an authorization code the provider
	// rejected; the caller must obtain a fresh one.
	ErrWeChatAuthorizationInvalid = errors.New("wechat authorization invalid")
	// ErrIdentityProvider marks a provider that could not answer or answered
	// unusably.
	ErrIdentityProvider = errors.New("wechat provider failed")
	// ErrSenderNotFound marks an unknown registration or one this Account does
	// not own; the two are deliberately indistinguishable.
	ErrSenderNotFound = errors.New("sender registration not found")
	// ErrSenderRevoked marks an owned registration that security revocation
	// permanently closed and that must never be recreated.
	ErrSenderRevoked = errors.New("sender registration revoked")
	// ErrCredentialInvalid marks a credential that does not authorize the
	// requested operation for any reason.
	ErrCredentialInvalid = errors.New("credential invalid")
	// ErrStorageUnavailable marks a required database operation that failed.
	ErrStorageUnavailable = errors.New("storage unavailable")
)
