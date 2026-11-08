package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
	"github.com/go-jet/jet/v2/qrm"
)

// Service implements Account mapping, Sender login and credential resolution
// over PostgreSQL.
type Service struct {
	pool      *sql.DB
	providers map[ProviderKind]Provider
}

// NewService composes the identity service on pool. A provider that is absent
// reports a provider failure when its operation is invoked instead of preventing
// process startup.
func NewService(pool *sql.DB, providers ...Provider) *Service {
	byKind := make(map[ProviderKind]Provider, len(providers))
	for _, provider := range providers {
		if provider != nil {
			byKind[provider.Kind()] = provider
		}
	}
	return &Service{pool: pool, providers: byKind}
}

// Login verifies the supplied provider proof and creates or restores this
// installation's Sender registration, replacing that Sender's previous
// credential family. The provider is called first, so a slow or failing
// provider never holds a database transaction.
func (s *Service) Login(ctx context.Context, request LoginRequest) (bundle Bundle, err error) {
	request.DeviceName = strings.TrimSpace(request.DeviceName)
	if request.DeviceName == "" || !utf8.ValidString(request.DeviceName) || utf8.RuneCountInString(request.DeviceName) > 64 {
		return Bundle{}, ErrDeviceNameInvalid
	}

	// A failure that is not an explicit outcome is storage unavailability:
	// login cannot complete without PostgreSQL.
	defer func() {
		if err != nil && !isOutcome(err) {
			err = fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
		}
	}()

	verified, err := s.verify(ctx, request.Proof)
	if err != nil {
		return Bundle{}, err
	}

	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return Bundle{}, err
	}
	defer func() { _ = tx.Rollback() }()

	issuedAt := time.Now().UTC()
	accountID, err := upsertAccount(ctx, tx, verified)
	if err != nil {
		return Bundle{}, err
	}

	senderID := request.SenderID
	if senderID == "" {
		senderID = newValue()
		if err := insertSender(ctx, tx, senderID, accountID, request.DeviceName); err != nil {
			return Bundle{}, err
		}
	} else if err := restoreSender(ctx, tx, senderID, accountID, request.DeviceName); err != nil {
		return Bundle{}, err
	}

	// The Sender is locked for the rest of this transaction, and every family
	// mutation path takes the same lock first, so a concurrent login, refresh
	// or revocation cannot interleave with this replacement.
	if err := revokeSenderCredentials(ctx, tx, senderID, issuedAt); err != nil {
		return Bundle{}, err
	}

	bundle = newBundle(accountID, senderID, issuedAt)
	if err := insertCredentials(ctx, tx, newValue(), bundle, issuedAt); err != nil {
		return Bundle{}, err
	}
	if err := tx.Commit(); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

// Authenticate resolves one raw credential to the Sender it authorizes for
// scope. A retained credential identifies a permanently closed Sender before
// its own expiry, scope or revocation is considered.
func (s *Service) Authenticate(ctx context.Context, rawCredential string, scope Scope) (string, error) {
	if rawCredential == "" {
		return "", ErrCredentialMissing
	}
	credential, err := findCredential(ctx, s.pool, digest(rawCredential))
	if err != nil {
		if errors.Is(err, ErrCredentialInvalid) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
	}
	// An owner's permanent closure of the registration is a lasting fact about
	// the identity itself, so it is decided before the credential's own
	// lifecycle: the closure also revoked the family's rows, and a client that
	// never learns the registration is gone would keep asking for it.
	switch state, err := registrationState(ctx, s.pool, credential.SenderID); {
	case errors.Is(err, ErrCredentialInvalid):
		return "", err
	case err != nil:
		return "", fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
	case state == model.IdentitySenderRegistrationState_Revoked:
		return "", ErrSenderRevoked
	}
	if credential.RevokedAt != nil || Scope(credential.Scope) != scope {
		return "", ErrCredentialInvalid
	}
	if !credential.ExpiresAt.After(time.Now().UTC()) {
		return "", ErrCredentialExpired
	}
	return credential.SenderID, nil
}

// Refresh rotates one Sender's credential family against its refresh token.
// The operation ID and attempt the client persisted with that token order the
// rotation: the family's current token rotates, the token an operation already
// consumed recovers one lost response for that same operation inside
// RefreshOverlap, a recorded attempt that is equal or higher is superseded, any
// other use of a spent token revokes that family alone, and a higher attempt
// that presents a credential no operation consumed is rejected untouched.
func (s *Service) Refresh(ctx context.Context, request RefreshRequest) (bundle Bundle, err error) {
	// A failure that is not an explicit outcome is storage unavailability:
	// renewal cannot complete without PostgreSQL.
	defer func() {
		if err != nil && !isOutcome(err) {
			err = fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
		}
	}()

	if request.Attempt != 0 && request.Attempt != 1 {
		return Bundle{}, ErrAttemptUnsupported
	}

	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return Bundle{}, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	presented, err := findCredential(ctx, tx, digest(request.RefreshToken))
	if err != nil {
		return Bundle{}, err
	}
	// The Sender lock is what serializes renewal with login, logout and
	// security revocation, so the credential is read again under it.
	registration, err := lockSender(ctx, tx, presented.SenderID)
	if errors.Is(err, qrm.ErrNoRows) {
		return Bundle{}, ErrCredentialInvalid
	}
	if err != nil {
		return Bundle{}, err
	}
	if registration.State != model.IdentitySenderRegistrationState_Active {
		return Bundle{}, ErrSenderRevoked
	}
	credential, err := lockCredential(ctx, tx, presented.TokenDigest)
	if err != nil {
		return Bundle{}, err
	}
	if credential.RevokedAt != nil {
		return Bundle{}, ErrCredentialRevoked
	}
	if !credential.ExpiresAt.After(now) {
		return Bundle{}, ErrCredentialExpired
	}
	if Scope(credential.Scope) != ScopeRefresh {
		return Bundle{}, ErrCredentialInvalid
	}

	operation, recorded, err := findRefreshOperation(ctx, tx, credential.SenderID, request.OperationID)
	if err != nil {
		return Bundle{}, err
	}
	// The attempt this client already committed: report the ordering conflict
	// before replay detection, so a stale retransmission never mints
	// credentials and never revokes the family.
	if recorded && request.Attempt <= int(operation.Attempt) {
		return Bundle{}, ErrOperationSuperseded
	}
	spent, err := tokenConsumed(ctx, tx, credential.TokenDigest)
	if err != nil {
		return Bundle{}, err
	}
	// A higher attempt is a retransmission of the recorded operation only when
	// it presents the credential that operation consumed. Any other credential
	// has been consumed by nothing, so the attempt is rejected without
	// consuming it and without crediting it to the operation's record.
	if recorded && !spent {
		return Bundle{}, ErrOperationMismatch
	}
	// A consumed token is replayable only as the recovery of the one operation
	// that may recover it; every other use revokes the family it belongs to.
	recovering := recorded && recoverable(operation, credential, now)
	if spent && !recovering {
		return Bundle{}, revokeForReuse(ctx, tx, credential, now)
	}

	// The rotation continues the family: it issues fresh M/U/F and leaves the
	// family's existing access tokens to their original expiry. A replacement
	// for a lost response is a fresh bundle of the same kind, never the raw
	// successor the client did not receive.
	rotated := newBundle(registration.AccountID, credential.SenderID, now)
	if err := insertCredentials(ctx, tx, credential.FamilyID, rotated, now); err != nil {
		return Bundle{}, err
	}
	if err := recordRefreshAttempt(ctx, tx, credential.SenderID, request.OperationID, request.Attempt, credential, now); err != nil {
		return Bundle{}, err
	}
	if err := tx.Commit(); err != nil {
		return Bundle{}, err
	}
	return rotated, nil
}

// recoverable reports whether a higher attempt of the recorded operation may
// mint a replacement bundle: the presented token must be that operation's
// consumed immediate predecessor, inside the overlap that started when the
// operation committed and inside the parent's original expiry, which is also
// how long the record itself is kept.
func recoverable(operation model.IdentityRefreshOperations, presented model.IdentityCredentials, now time.Time) bool {
	if presented.TokenDigest != operation.ParentDigest {
		return false
	}
	if now.After(operation.CommittedAt.Add(RefreshOverlap)) {
		return false
	}
	return !now.After(operation.ExpiresAt)
}

// Logout ends one Sender's service access: it revokes that Sender's credential
// family — both access scopes and the renewal credential — and nothing else. The
// registration stays active and its Pairings survive, so a later login restores
// the same installation. Logout takes the lock login, renewal and security
// revocation take, and validates the presented credential under it, so a
// delayed logout carrying authority an earlier operation already replaced
// revokes nothing.
func (s *Service) Logout(ctx context.Context, request LogoutRequest) (err error) {
	// A failure that is not an explicit outcome is storage unavailability: the
	// caller must never read a request the service could not commit as remote
	// sign-out.
	defer func() {
		if err != nil && !isOutcome(err) {
			err = fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
		}
	}()

	// A request that offers both of its own authority sources states no single
	// authority, so it authorizes nothing.
	if request.AccessToken != "" && request.RefreshToken != "" {
		return ErrCredentialSourceConflicting
	}

	// The header carries the Sender's own access authority; its renewal
	// credential stands in when access has expired.
	presented, allowed := request.AccessToken, []Scope{ScopeUserSession, ScopeUploader}
	if presented == "" {
		presented, allowed = request.RefreshToken, []Scope{ScopeRefresh}
	}
	if presented == "" {
		return ErrCredentialMissing
	}

	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	// The credential is read twice for the reason renewal reads it twice: the
	// first read names the Sender to lock, and only the locked read states
	// whether this credential still authorizes anything.
	claimed, err := findCredential(ctx, tx, digest(presented))
	if err != nil {
		return err
	}
	registration, err := lockSender(ctx, tx, claimed.SenderID)
	if errors.Is(err, qrm.ErrNoRows) {
		return ErrCredentialInvalid
	}
	if err != nil {
		return err
	}
	if registration.State != model.IdentitySenderRegistrationState_Active {
		return ErrSenderRevoked
	}
	credential, err := lockCredential(ctx, tx, claimed.TokenDigest)
	if err != nil {
		return err
	}
	if credential.RevokedAt != nil {
		return ErrCredentialRevoked
	}
	if !credential.ExpiresAt.After(now) {
		return ErrCredentialExpired
	}
	if !slices.Contains(allowed, Scope(credential.Scope)) {
		return ErrCredentialInvalid
	}

	if err := revokeSenderCredentials(ctx, tx, credential.SenderID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// revokeForReuse revokes every credential of the family a reused renewal token
// belongs to, commits that revocation and reports the reuse outcome. The
// revocation is the point of the operation, so it is committed even though the
// request itself fails; a storage failure leaves the family intact and reports
// unavailability instead.
func revokeForReuse(ctx context.Context, tx *sql.Tx, credential model.IdentityCredentials, now time.Time) error {
	if err := revokeFamilyCredentials(ctx, tx, credential.FamilyID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return ErrRefreshReuse
}

// AccountFor verifies one fresh provider proof and resolves the Account it
// belongs to. Unknown identities report the same outcome as invisible ones, so
// a caller cannot confirm whether another Account holds one.
func (s *Service) AccountFor(ctx context.Context, proof Proof) (string, error) {
	verified, err := s.verify(ctx, proof)
	if err != nil {
		return "", err
	}
	account, err := findAccount(ctx, s.pool, verified)
	if errors.Is(err, ErrSenderNotFound) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
	}
	return account, nil
}

// verify selects the configured provider and rejects an adapter answer that
// does not belong to the selected provider.
func (s *Service) verify(ctx context.Context, proof Proof) (Identity, error) {
	provider := s.providers[proof.Provider]
	if provider == nil || proof.Code == "" {
		return Identity{}, ErrIdentityProvider
	}
	verified, err := provider.Verify(ctx, proof.Code)
	if err != nil {
		return Identity{}, err
	}
	if verified.Provider != proof.Provider {
		return Identity{}, fmt.Errorf("%w: provider answer belongs to %q",
			ErrIdentityProvider, verified.Provider)
	}
	return verified, nil
}

// newBundle issues three independent credentials with the adopted lifetimes.
func newBundle(accountID, senderID string, issuedAt time.Time) Bundle {
	return Bundle{
		AccountID:   accountID,
		SenderID:    senderID,
		UserSession: Token{Value: newValue(), ExpiresAt: issuedAt.Add(AccessLifetime)},
		Uploader:    Token{Value: newValue(), ExpiresAt: issuedAt.Add(AccessLifetime)},
		Refresh:     Token{Value: newValue(), ExpiresAt: issuedAt.Add(RefreshLifetime)},
	}
}

// newValue returns an unpredictable opaque value. Credentials and identifiers
// never encode authority and are never reused.
func newValue() string {
	return rand.Text()
}

// isOutcome reports whether err already states a specific public outcome.
func isOutcome(err error) bool {
	for _, outcome := range []error{
		ErrWeChatAuthorizationInvalid,
		ErrQQAuthorizationInvalid,
		ErrIdentityProvider,
		ErrIdentityProofRequired,
		ErrIdentityProofConflicting,
		ErrSenderNotFound,
		ErrSenderRevoked,
		ErrCredentialMissing,
		ErrCredentialInvalid,
		ErrCredentialExpired,
		ErrCredentialRevoked,
		ErrCredentialSourceConflicting,
		ErrRefreshReuse,
		ErrOperationSuperseded,
		ErrOperationMismatch,
		ErrAttemptUnsupported,
	} {
		if errors.Is(err, outcome) {
			return true
		}
	}
	return false
}
