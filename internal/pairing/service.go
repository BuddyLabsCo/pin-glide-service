package pairing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// Service implements Pairing Session creation, Sender authorization and
// cancellation over PostgreSQL. Expiry is evaluated from stored rows, so no
// background work is needed to make a session expire.
type Service struct {
	pool          *sql.DB
	provider      Provider
	authenticator Authenticator
	notifier      Notifier
}

// Notifier reports session state changes to the live pairing channel, so a
// connected participant follows a claim or a cancellation without polling.
type Notifier interface {
	SessionChanged(sessionID string, view View)
}

// SetNotifier installs the transport notifier. A process composed without one
// keeps working; only live state notifications stop.
func (s *Service) SetNotifier(notifier Notifier) {
	s.notifier = notifier
}

// NewService composes the pairing service. A nil provider is a process
// composed without provider credentials: recovery then reports a provider
// failure instead of verifying anything.
func NewService(pool *sql.DB, provider Provider, authenticator Authenticator) *Service {
	return &Service{pool: pool, provider: provider, authenticator: authenticator}
}

// Create opens one restricted session in the requested mode and returns it
// together with the raw B credential, which leaves the service only through
// this result and only to the Receiver side.
func (s *Service) Create(ctx context.Context, request CreateRequest, now time.Time) (Session, string, error) {
	session, token := Session{}, ""
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		created, rawToken, err := s.create(ctx, tx, request, now)
		if err != nil {
			return err
		}
		session, token = created, rawToken
		return nil
	})
	if err != nil {
		return Session{}, "", err
	}
	s.sessionChanged(session, now)
	return session, token, nil
}

// Authorize claims a session for one authenticated Sender. The first
// successful claim fixes ownership; the same Sender repeating the call gets
// the same session back, and another Sender receives ErrSessionClaimed.
func (s *Service) Authorize(ctx context.Context, request AuthorizeRequest, now time.Time) (Session, error) {
	session, err := sessionByID(ctx, s.pool, request.SessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return Session{}, err
	}
	if err != nil {
		return Session{}, storageError(err)
	}

	if session.AuthorizedSenderID != "" {
		if session.AuthorizedSenderID != request.SenderID {
			return Session{}, ErrSessionClaimed
		}
		// Repeating a claim never extends the lifetime, so a lapsed session
		// reports its own outcome instead of a fresh snapshot.
		if session.Expired(now) {
			return Session{}, ErrSessionExpired
		}
		if session.State == StateAwaitingConfirmation || session.State == StateCredentialReady {
			return session, nil
		}
		if err := session.TerminalError(now); err != nil {
			return Session{}, err
		}
		return Session{}, ErrSessionClosed
	}
	if err := session.TerminalError(now); err != nil {
		return Session{}, err
	}
	if session.State != StateAwaitingAuthorization {
		return Session{}, ErrSessionClosed
	}

	// The fresh owner proof of a recovery session is an external call, so it is
	// verified before the transaction that claims the session and reserves
	// capacity.
	var proof WeChatProof
	if session.Mode == ModeRecoverReceiver {
		proof, err = s.verifyOwner(ctx, request.RecoveryCode)
		if err != nil {
			return Session{}, err
		}
	}

	err = s.inTransaction(ctx, func(tx *sql.Tx) error {
		claimed, err := claimSession(ctx, tx, request.SessionID, request.SenderID)
		if err != nil {
			return err
		}
		if !claimed {
			// A concurrent authorization committed first. The caller keeps
			// the idempotent or conflicting outcome of that claim.
			owner, err := claimedSender(ctx, tx, request.SessionID)
			if err != nil && !errors.Is(err, ErrSessionNotFound) {
				return err
			}
			if owner == request.SenderID {
				return nil
			}
			return ErrSessionClaimed
		}

		switch session.Mode {
		case ModeNewReceiver, ModeAddSender:
			return s.authorizeAssociation(ctx, tx, session, request, now)
		case ModeRecoverReceiver:
			return s.authorizeRecovery(ctx, tx, session, request, proof)
		default:
			return ErrInvalidMode
		}
	})
	if err != nil {
		return Session{}, err
	}
	authorized, err := sessionByID(ctx, s.pool, request.SessionID)
	if err != nil {
		return Session{}, storageError(err)
	}
	s.sessionChanged(authorized, now)
	return authorized, nil
}

// Cancel releases an unfinished session and its reservation. After an
// activation committed, cancellation reports the session as closed instead of
// undoing the association.
func (s *Service) Cancel(ctx context.Context, sessionID, senderID, token string, now time.Time) error {
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		session, err := sessionByID(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if err := authorizeParticipant(session, senderID, token); err != nil {
			return err
		}
		if err := session.TerminalError(now); err != nil {
			return err
		}
		if !session.Live(now) {
			return ErrSessionClosed
		}
		return cancelSession(ctx, tx, session, now)
	})
	if err != nil {
		return err
	}
	if session, err := sessionByID(ctx, s.pool, sessionID); err == nil {
		s.sessionChanged(session, now)
	}
	return nil
}

// SessionByID reads one session's current state. The pairing channel uses it
// to send an initial snapshot and to re-check authority on every frame.
func (s *Service) SessionByID(ctx context.Context, sessionID string) (Session, error) {
	session, err := sessionByID(ctx, s.pool, sessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return Session{}, err
	}
	if err != nil {
		return Session{}, storageError(err)
	}
	return session, nil
}

// SenderOf resolves one Sender management credential to its Sender, so the
// pairing channel can authenticate the claiming side of a session.
func (s *Service) SenderOf(ctx context.Context, token string) (string, error) {
	return s.authenticator.Authenticate(ctx, token, string(identity.ScopeUserSession))
}

// sessionChanged notifies the live channel about one session's new state.
func (s *Service) sessionChanged(session Session, now time.Time) {
	if s.notifier == nil {
		return
	}
	s.notifier.SessionChanged(session.ID, session.View(now))
}

// Resolve presents one restricted credential and reports the session it
// belongs to and the side it authenticates. A credential past its lifetime is
// reported as an expired credential, not as an expired session: validity is
// decided before the protected session state.
func (s *Service) Resolve(ctx context.Context, token string, now time.Time) (Resolved, error) {
	if token == "" {
		return Resolved{}, ErrCredentialMissing
	}
	session, err := sessionByTokenDigest(ctx, s.pool, digest(token))
	if errors.Is(err, ErrSessionNotFound) {
		return Resolved{}, ErrCredentialInvalid
	}
	if err != nil {
		return Resolved{}, storageError(err)
	}
	if session.Expired(now) {
		return Resolved{}, ErrCredentialExpired
	}
	return Resolved{Session: session, Side: SideReceiver}, nil
}

// create builds one session and its Receiver context inside the caller's
// transaction. The mode is fixed here, and no later request can switch it.
func (s *Service) create(ctx context.Context, tx *sql.Tx, request CreateRequest, now time.Time) (Session, string, error) {
	switch request.Mode {
	case ModeNewReceiver:
		if request.ReceiverID != "" {
			return Session{}, "", ErrInvalidMode
		}
	case ModeRecoverReceiver:
		if request.ReceiverID == "" {
			return Session{}, "", ErrInvalidMode
		}
	default:
		return Session{}, "", ErrInvalidMode
	}

	receiverID := request.ReceiverID
	if request.Mode == ModeNewReceiver {
		receiverID = newValue()
		if err := insertReceiverRegistration(ctx, tx, receiverID); err != nil {
			return Session{}, "", err
		}
	} else if _, err := claimedReceiverAccount(ctx, tx, receiverID); err != nil {
		// Recovery acts on a Receiver that already has an owning Account; an
		// unknown, unclaimed or revoked one is not visible to anyone.
		return Session{}, "", err
	}

	info := sessionInfo{
		id:         newValue(),
		mode:       request.Mode,
		receiverID: receiverID,
		state:      StateAwaitingAuthorization,
		createdAt:  now,
		expiresAt:  now.Add(SessionLifetime),
	}
	token := newValue()
	info.tokenDigest = digest(token)
	if err := insertSession(ctx, tx, info); err != nil {
		return Session{}, "", err
	}
	return info.session(), token, nil
}

// authorizeAssociation reserves the Pairing ID and the binding capacity a
// new_receiver or add_sender session needs for the claiming Sender.
func (s *Service) authorizeAssociation(ctx context.Context, tx *sql.Tx, session Session, request AuthorizeRequest, now time.Time) error {
	// The lock serializes this Sender's capacity decisions, so two concurrent
	// claims cannot both see a free slot.
	if err := lockSender(ctx, tx, request.SenderID); err != nil {
		return err
	}

	// Re-pairing a Receiver the Sender already holds reuses its association, so
	// it needs no free slot of its own.
	pairingID, err := currentPairing(ctx, tx, request.SenderID, session.ReceiverID)
	if err != nil {
		return err
	}
	if pairingID == "" {
		held, err := countReceivers(ctx, tx, request.SenderID)
		if err != nil {
			return err
		}
		if held >= ReceiverLimit {
			return ErrReceiverLimitReached
		}
		pairingID = newValue()
		if err := insertPendingPairing(ctx, tx, pairingID, request.SenderID, session.ReceiverID, now); err != nil {
			return err
		}
	}
	if err := reservePairing(ctx, tx, session.ID, pairingID); err != nil {
		return err
	}
	if request.Alias != "" {
		if err := setAlias(ctx, tx, pairingID, request.Alias); err != nil {
			return err
		}
	}

	// A new_receiver session fixes the Receiver's owning Account at its first
	// successful claim, which is what makes the Receiver visible to its Sender.
	if session.Mode == ModeNewReceiver {
		account, err := senderAccount(ctx, tx, request.SenderID)
		if err != nil {
			return err
		}
		if err := claimReceiverRegistration(ctx, tx, session.ReceiverID, account); err != nil {
			return err
		}
	}
	return nil
}

// verifyOwner verifies one fresh WeChat proof and reports the Account it names.
// The provider call is an external boundary, so it stays outside every database
// transaction.
func (s *Service) verifyOwner(ctx context.Context, recoveryCode string) (WeChatProof, error) {
	if recoveryCode == "" {
		return WeChatProof{}, ErrFreshIdentityRequired
	}
	if s.provider == nil {
		return WeChatProof{}, ErrIdentityProvider
	}
	return s.provider.VerifyIdentity(ctx, recoveryCode)
}

// authorizeRecovery applies the fresh owner approval a recovery session
// requires. Pure recovery creates no association and reserves no capacity.
func (s *Service) authorizeRecovery(ctx context.Context, tx *sql.Tx, session Session, request AuthorizeRequest, proof WeChatProof) error {
	owner, err := senderAccount(ctx, tx, request.SenderID)
	if err != nil {
		return err
	}
	if proof.AccountID != owner {
		return ErrFreshIdentityRequired
	}
	// A recovery session can only replace the credential of a Receiver that
	// already belongs to this Account.
	return requireOwnedReceiver(ctx, tx, session.ReceiverID, request.SenderID)
}

// requireOwnedReceiver checks that a Receiver registration exists, is not
// revoked and belongs to the Sender's Account. Everything else reports the
// same outcome, so a caller cannot confirm another Account's Receivers.
func requireOwnedReceiver(ctx context.Context, tx *sql.Tx, receiverID, senderID string) error {
	account, err := claimedReceiverAccount(ctx, tx, receiverID)
	if err != nil {
		return err
	}
	owner, err := senderAccount(ctx, tx, senderID)
	if err != nil {
		return err
	}
	if owner != account {
		return ErrReceiverNotOwned
	}
	return nil
}

// claimedReceiverAccount returns the Account a Receiver registration belongs
// to. An unknown, unclaimed or revoked registration reports the same outcome,
// so a caller cannot confirm another Account's Receivers.
func claimedReceiverAccount(ctx context.Context, tx *sql.Tx, receiverID string) (string, error) {
	account, state, err := lockRegistration(ctx, tx, receiverID)
	if err != nil {
		return "", err
	}
	if state == string(RegistrationRevoked) || account == "" {
		return "", ErrReceiverNotOwned
	}
	return account, nil
}

// authorizeParticipant reports whether the presented credential belongs to
// this session and may cancel it. The owning Sender uses M; the Receiver side
// uses the restricted B.
func authorizeParticipant(session Session, senderID, token string) error {
	if senderID != "" {
		if session.AuthorizedSenderID == "" || session.AuthorizedSenderID != senderID {
			return ErrSessionNotFound
		}
		return nil
	}
	if token == "" || digest(token) != session.TokenDigest {
		return ErrSessionNotFound
	}
	return nil
}

// inTransaction runs work in one transaction and reports a failure that is not
// an explicit outcome as storage unavailability.
func (s *Service) inTransaction(ctx context.Context, work func(tx *sql.Tx) error) (err error) {
	defer func() {
		if err != nil && !isOutcome(err) {
			err = storageError(err)
		}
	}()

	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := work(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// sessionInfo is the durable session one operation writes.
type sessionInfo struct {
	id          string
	mode        Mode
	receiverID  string
	state       SessionState
	tokenDigest string
	createdAt   time.Time
	expiresAt   time.Time
}

// session maps the written session to its domain type.
func (i sessionInfo) session() Session {
	return Session{
		ID:          i.id,
		Mode:        i.mode,
		ReceiverID:  i.receiverID,
		State:       i.state,
		TokenDigest: i.tokenDigest,
		CreatedAt:   i.createdAt,
		ExpiresAt:   i.expiresAt,
	}
}

// newValue returns an unpredictable opaque value. Identifiers and credentials
// never encode authority and are never reused.
func newValue() string {
	return rand.Text()
}

// storageError marks a failure that is not an explicit business outcome.
func storageError(err error) error {
	if err == nil || isOutcome(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
}

// isOutcome reports whether err already states a specific public outcome.
func isOutcome(err error) bool {
	for _, outcome := range []error{
		ErrSessionNotFound,
		ErrSessionExpired,
		ErrSessionClosed,
		ErrSessionClaimed,
		ErrReceiverLimitReached,
		ErrInvalidMode,
		ErrReceiverNotOwned,
		ErrFreshIdentityRequired,
		ErrWeChatAuthorizationInvalid,
		ErrIdentityProvider,
		ErrCredentialMissing,
		ErrCredentialInvalid,
		ErrCredentialExpired,
	} {
		if errors.Is(err, outcome) {
			return true
		}
	}
	return false
}
