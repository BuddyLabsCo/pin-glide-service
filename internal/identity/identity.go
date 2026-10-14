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
	// RefreshOverlap is how long the consumed immediate predecessor of one
	// renewal operation stays eligible for the retransmission of that same
	// operation. The predecessor's own expiry bounds it as well.
	RefreshOverlap = 30 * time.Second
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

// RefreshRequest is one ordered renewal: the token that authorizes it, the
// operation identifier the client persisted with that token before sending it,
// and the attempt number of this transmission. Attempt 0 is the original
// request and attempt 1 is the one permitted retransmission of an uncertain
// one.
type RefreshRequest struct {
	RefreshToken string
	OperationID  string
	Attempt      int
}

// LogoutRequest is one ordinary logout. Exactly one source carries the Sender's
// own authority: AccessToken is the management or upload credential the client
// sent in the Authorization header, RefreshToken is the renewal credential it
// sent in the body instead. A request that offers both reports
// ErrCredentialSourceConflicting.
type LogoutRequest struct {
	AccessToken  string
	RefreshToken string
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
	// ErrCredentialMissing marks an operation without a usable credential
	// source; a protected operation requires exactly one.
	ErrCredentialMissing = errors.New("credential missing")
	// ErrCredentialExpired marks a credential whose recorded expiry has
	// passed. An expired credential never revokes anything.
	ErrCredentialExpired = errors.New("credential expired")
	// ErrCredentialRevoked marks a credential or credential family that was
	// revoked, including by an earlier replay detection or logout.
	ErrCredentialRevoked = errors.New("credential revoked")
	// ErrCredentialSourceConflicting marks a logout that offered more than one
	// of its own authority sources at once, so no single source authorizes it.
	ErrCredentialSourceConflicting = errors.New("credential sources conflict")
	// ErrRefreshReuse marks confirmed reuse of an already consumed renewal
	// token; the family that token belongs to has been revoked.
	ErrRefreshReuse = errors.New("refresh credential reused")
	// ErrOperationSuperseded marks an attempt equal to or lower than the one
	// another request already committed for the same operation. It neither
	// issues credentials nor revokes anything.
	ErrOperationSuperseded = errors.New("operation superseded")
	// ErrOperationMismatch marks a higher attempt that claims a recorded
	// operation without presenting the credential that operation consumed. It
	// neither issues credentials nor revokes anything.
	ErrOperationMismatch = errors.New("operation mismatch")
	// ErrAttemptUnsupported marks an attempt outside the adopted 0/1 protocol.
	ErrAttemptUnsupported = errors.New("refresh attempt unsupported")
	// ErrStorageUnavailable marks a required database operation that failed.
	ErrStorageUnavailable = errors.New("storage unavailable")
)
