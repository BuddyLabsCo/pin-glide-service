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
	// revocations is the credential half of a Sender revocation and an Account
	// switch, which commit inside the transaction that also removes that
	// Sender's Pairings.
	revocations SenderMutations
	// deliveryNotifier reports a removed association to the Receiver's live
	// delivery connection, which is a different live channel than the pairing
	// one the session notifier serves.
	deliveryNotifier DeliveryNotifier
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
// failure instead of verifying anything. A nil mutations value composes a
// process that cannot change a Sender's credentials, so permanent revocation
// and Account switching report unavailability instead of half-applying.
func NewService(pool *sql.DB, provider Provider, authenticator Authenticator, mutations SenderMutations) *Service {
	return &Service{pool: pool, provider: provider, authenticator: authenticator, revocations: mutations}
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
		// The session row is locked before its state is read, so a cancellation
		// cannot interleave with the completion that activates the session's
		// association: whichever commits first decides the outcome.
		session, err := lockedSessionByID(ctx, tx, sessionID)
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
		return closeSession(ctx, tx, session, StateCanceled, now)
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

// sessionsChanged reports the state every session a committed change closed
// now has, so the live participants of that work learn its outcome without
// polling.
func (s *Service) sessionsChanged(ctx context.Context, sessionIDs []string, now time.Time) {
	for _, sessionID := range sessionIDs {
		if session, err := sessionByID(ctx, s.pool, sessionID); err == nil {
			s.sessionChanged(session, now)
		}
	}
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

// authorizeAssociation reserves the Pairing ID and the binding capacity a
// new_receiver or add_sender session needs for the claiming Sender. A
// replacement reserves its own Pairing ID next to the association it intends
// to retire, so the working association keeps forwarding until both clients
// confirm the replacement.
func (s *Service) authorizeAssociation(ctx context.Context, tx *sql.Tx, session Session, request AuthorizeRequest, now time.Time) error {
	// The lock serializes this Sender's capacity decisions, so two concurrent
	// claims cannot both see a free slot, and no claim interleaves with a
	// replacement committing its activation.
	if err := lockSender(ctx, tx, request.SenderID); err != nil {
		return err
	}
	if session.Mode == ModeAddSender {
		// The credential that created this session authenticated one Receiver
		// of one Account, so only that Account's Senders may claim it.
		if err := requireOwnedReceiver(ctx, tx, session.ReceiverID, request.SenderID); err != nil {
			return err
		}
	}

	reserved := reservation{
		pairingID:  newValue(),
		senderID:   request.SenderID,
		receiverID: session.ReceiverID,
		alias:      request.Alias,
		createdAt:  now,
	}
	// Re-pairing a Receiver the Sender already holds replaces that association
	// without needing a free slot of its own, and inherits the Sender's own
	// alias unless this claim supplies a replacement. A Receiver the Sender
	// already holds as a live reservation is counted already, so only its other
	// Receivers decide whether this claim needs a free slot.
	current, err := currentPairing(ctx, tx, request.SenderID, session.ReceiverID)
	if err != nil {
		return err
	}
	if current.ID == "" {
		held, err := countOtherReceivers(ctx, tx, request.SenderID, session.ReceiverID)
		if err != nil {
			return err
		}
		if held >= ReceiverLimit {
			return ErrReceiverLimitReached
		}
	} else {
		reserved.replacesPairingID = current.ID
		if reserved.alias == "" {
			reserved.alias = current.Alias
		}
	}
	if err := insertPendingPairing(ctx, tx, reserved); err != nil {
		return err
	}
	if err := reservePairing(ctx, tx, session.ID, reserved.pairingID); err != nil {
		return err
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
		ErrSenderNotOwned,
		ErrSenderRevoked,
		ErrReceiverRevoked,
		ErrAssociationNotFound,
		ErrPairingNotReady,
		ErrModeForbidsCredentialIssue,
		ErrOperationSuperseded,
		ErrAttemptUnsupported,
		ErrIssuanceInvalid,
		ErrRetainedPairingsInvalid,
		ErrFreshIdentityRequired,
		ErrWeChatAuthorizationInvalid,
		ErrIdentityProvider,
		ErrCredentialMissing,
		ErrCredentialInvalid,
		ErrCredentialExpired,
		ErrCredentialRevoked,
	} {
		if errors.Is(err, outcome) {
			return true
		}
	}
	return false
}
