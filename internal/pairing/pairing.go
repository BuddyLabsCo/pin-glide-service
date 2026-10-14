// Package pairing owns Sender-to-Receiver associations and the restricted
// Pairing Sessions that authorize them.
package pairing

import (
	"context"
	"errors"
	"time"
)

// ReceiverLimit is how many distinct Receivers one Sender may associate with.
// Active associations and confirmed pending reservations share the limit.
const ReceiverLimit = 5

// SessionLifetime is the absolute lifetime of a Pairing Session and of the
// restricted B credential it returns. Nothing extends it.
const SessionLifetime = 5 * time.Minute

// Mode is the fixed purpose one Pairing Session was created for. A later
// request cannot switch it, so the mode also fixes which credentials may
// complete the session.
type Mode string

const (
	// ModeNewReceiver pairs a Windows Receiver that has no registration yet.
	ModeNewReceiver Mode = "new_receiver"
	// ModeAddSender attaches an existing Receiver to one more Sender.
	ModeAddSender Mode = "add_sender"
	// ModeRecoverReceiver replaces a lost Receiver credential.
	ModeRecoverReceiver Mode = "recover_receiver"
)

// SessionState is the adopted Pairing Session lifecycle. Expiry is a fact of
// the stored expiry time rather than a state a request has to write.
type SessionState string

const (
	// StateAwaitingAuthorization marks a session no Sender has claimed.
	StateAwaitingAuthorization SessionState = "awaiting_authorization"
	// StateAwaitingConfirmation marks a claimed session before both reports.
	StateAwaitingConfirmation SessionState = "awaiting_confirmation"
	// StateCredentialReady marks a session whose Receiver may redeem R.
	StateCredentialReady SessionState = "credential_ready"
	// StateCredentialIssued marks a session whose R was consumed once.
	StateCredentialIssued SessionState = "credential_issued"
	// StateCompleted marks a session that produced its credential.
	StateCompleted SessionState = "completed"
	// StateCanceled marks a session its participant canceled.
	StateCanceled SessionState = "canceled"
	// StateExpired marks a session past its absolute five-minute lifetime.
	StateExpired SessionState = "expired"
	// StateSuperseded marks a session a newer recovery replaced.
	StateSuperseded SessionState = "superseded"
)

// AssociationState is the lifecycle of one Sender-to-Receiver association. A
// pending row holds binding capacity until both clients confirm it.
type AssociationState string

const (
	// AssociationPending marks a reserved association awaiting confirmation.
	AssociationPending AssociationState = "pending"
	// AssociationActive marks an activated association; it counts against the
	// Sender's Receiver capacity.
	AssociationActive AssociationState = "active"
	// AssociationRemoved marks an association its owning Sender removed.
	AssociationRemoved AssociationState = "removed"
)

// RegistrationState is the lifecycle of one Receiver registration.
type RegistrationState string

const (
	// RegistrationPending marks a Receiver context no Sender has claimed yet.
	RegistrationPending RegistrationState = "pending"
	// RegistrationActive marks a Receiver owned by an Account.
	RegistrationActive RegistrationState = "active"
	// RegistrationRevoked marks a Receiver revocation permanently closed it.
	RegistrationRevoked RegistrationState = "revoked"
)

// Session is the durable state of one Pairing Session. It never carries a raw
// credential, client key or handshake byte.
type Session struct {
	ID                  string
	Mode                Mode
	ReceiverID          string
	AuthorizedSenderID  string
	PendingPairingID    string
	State               SessionState
	TokenDigest         string
	SenderConfirmedAt   *time.Time
	ReceiverConfirmedAt *time.Time
	CreatedAt           time.Time
	ExpiresAt           time.Time
}

// View is the published PairingSessionView. SenderID and PairingID are empty
// until a claim or an allocation gives them a value; a client must tolerate
// their absence.
type View struct {
	SessionID         string
	Mode              Mode
	ReceiverID        string
	State             SessionState
	ExpiresAt         time.Time
	SenderConfirmed   bool
	ReceiverConfirmed bool
	SenderID          string
	PairingID         string
}

// Expired reports whether the session is past its absolute lifetime. The
// stored state of an unclaimed session is not rewritten when it lapses.
func (s Session) Expired(now time.Time) bool {
	return !s.ExpiresAt.After(now)
}

// CreateRequest is one request for a restricted session. A recovery session is
// created anonymously, like a new-receiver one: the fresh owner approval and
// its WeChat proof belong to the claim, which is the operation that acts on the
// target Receiver.
type CreateRequest struct {
	Mode       Mode
	ReceiverID string
}

// AuthorizeRequest is one Sender's claim of a session. Alias replaces the
// owning Sender's own label when it is non-empty; a recovery claim also
// carries a fresh owner-verification code.
type AuthorizeRequest struct {
	SessionID    string
	SenderID     string
	Alias        string
	RecoveryCode string
}

// Resolved is the result of presenting one restricted Pairing Session
// credential: the session it belongs to and the side that credential
// authorizes.
type Resolved struct {
	Session Session
	Side    Side
}

// Side is the participant a credential authenticates on a Pairing Session.
type Side string

const (
	// SideSender is the Sender that claimed the session, authenticated by M.
	SideSender Side = "sender"
	// SideReceiver is the Receiver side, authenticated by the restricted B.
	SideReceiver Side = "receiver"
)

// Other returns the participant a relayed frame is addressed to.
func (s Side) Other() Side {
	if s == SideSender {
		return SideReceiver
	}
	return SideSender
}

// Errors the pairing operations report. Callers distinguish outcomes by these
// typed causes; the HTTP surface maps them to stable business codes.
var (
	// ErrSessionNotFound marks an unknown session or one this caller is not a
	// participant of; the two are deliberately indistinguishable.
	ErrSessionNotFound = errors.New("pairing session not found")
	// ErrSessionExpired marks an authorized caller's session past its absolute
	// five-minute lifetime.
	ErrSessionExpired = errors.New("pairing session expired")
	// ErrSessionClosed marks a session that was canceled or superseded.
	ErrSessionClosed = errors.New("pairing session closed")
	// ErrSessionClaimed marks a session another Sender already claimed.
	ErrSessionClaimed = errors.New("pairing session claimed")
	// ErrReceiverLimitReached marks a Sender that is already associated with
	// the adopted number of distinct Receivers.
	ErrReceiverLimitReached = errors.New("receiver limit reached")
	// ErrInvalidMode marks a mode or target combination the request may not use.
	ErrInvalidMode = errors.New("pairing session mode is not usable for this request")
	// ErrReceiverNotOwned marks a Receiver that is not visible to this Sender.
	ErrReceiverNotOwned = errors.New("receiver not owned by this sender")
	// ErrFreshIdentityRequired marks a recovery request without fresh owner
	// verification.
	ErrFreshIdentityRequired = errors.New("fresh wechat verification required")
	// ErrWeChatAuthorizationInvalid marks a recovery code the provider rejected.
	ErrWeChatAuthorizationInvalid = errors.New("wechat authorization invalid")
	// ErrIdentityProvider marks a provider that could not answer.
	ErrIdentityProvider = errors.New("wechat provider failed")
	// ErrCredentialMissing marks a subscription without a credential.
	ErrCredentialMissing = errors.New("pairing credential missing")
	// ErrCredentialInvalid marks a restricted credential that authorizes
	// nothing here.
	ErrCredentialInvalid = errors.New("pairing credential invalid")
	// ErrCredentialExpired marks a restricted credential past its lifetime,
	// which ends with its session.
	ErrCredentialExpired = errors.New("pairing credential expired")
	// ErrStorageUnavailable marks a required database operation that failed.
	ErrStorageUnavailable = errors.New("storage unavailable")
)

// Provider verifies the fresh WeChat proof a recovery session requires. It is
// the same external boundary the identity service depends on.
type Provider interface {
	VerifyIdentity(ctx context.Context, wechatCode string) (WeChatProof, error)
}

// Authenticator resolves one Sender credential. The identity service
// implements it, so the pairing domain reuses the credential rules of the
// family it authorizes instead of restating them.
type Authenticator interface {
	Authenticate(ctx context.Context, rawCredential string, scope string) (string, error)
}

// WeChatProof is one verified WeChat identity and the Account it belongs to.
type WeChatProof struct {
	AccountID string
}

// Live reports whether a session may still accept relay traffic or
// cancellation; both end when an activation commits the association.
func (s Session) Live(now time.Time) bool {
	if s.Expired(now) {
		return false
	}
	switch s.State {
	case StateAwaitingAuthorization, StateAwaitingConfirmation:
		return true
	default:
		return false
	}
}

// TerminalError maps a session that cannot continue to the business outcome
// its caller must report, or nil while the session may still proceed.
func (s Session) TerminalError(now time.Time) error {
	if s.Expired(now) {
		return ErrSessionExpired
	}
	switch s.State {
	case StateCanceled, StateSuperseded, StateCompleted, StateCredentialIssued, StateCredentialReady:
		return ErrSessionClosed
	default:
		return nil
	}
}

// View reduces a session to the published PairingSessionView. The reported
// state is expired once the absolute lifetime has passed, whatever progress
// the stored state still records.
func (s Session) View(now time.Time) View {
	state := s.State
	if s.Expired(now) {
		state = StateExpired
	}
	return View{
		SessionID:         s.ID,
		Mode:              s.Mode,
		ReceiverID:        s.ReceiverID,
		State:             state,
		ExpiresAt:         s.ExpiresAt,
		SenderConfirmed:   s.SenderConfirmedAt != nil,
		ReceiverConfirmed: s.ReceiverConfirmedAt != nil,
		SenderID:          s.AuthorizedSenderID,
		PairingID:         s.PendingPairingID,
	}
}
