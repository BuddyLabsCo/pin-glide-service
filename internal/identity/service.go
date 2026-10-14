package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Service implements Account mapping, Sender login and credential resolution
// over PostgreSQL.
type Service struct {
	pool     *sql.DB
	provider Provider
}

// NewService composes the identity service on pool. A nil provider is a
// process composed without provider credentials: login then reports a provider
// failure instead of calling anyone.
func NewService(pool *sql.DB, provider Provider) *Service {
	return &Service{pool: pool, provider: provider}
}

// Login verifies the supplied WeChat code and creates or restores this
// installation's Sender registration, replacing that Sender's previous
// credential family. The provider is called first, so a slow or failing
// provider never holds a database transaction.
func (s *Service) Login(ctx context.Context, request LoginRequest) (bundle Bundle, err error) {
	// A failure that is not an explicit outcome is storage unavailability:
	// login cannot complete without PostgreSQL.
	defer func() {
		if err != nil && !isOutcome(err) {
			err = fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
		}
	}()

	if s.provider == nil {
		return Bundle{}, ErrIdentityProvider
	}
	verified, err := s.provider.Verify(ctx, request.WeChatCode)
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
		if err := insertSender(ctx, tx, senderID, accountID); err != nil {
			return Bundle{}, err
		}
	} else if err := restoreSender(ctx, tx, senderID, accountID); err != nil {
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
// scope. An unknown, revoked, expired or wrongly scoped credential is
// indistinguishable to the caller.
func (s *Service) Authenticate(ctx context.Context, rawCredential string, scope Scope) (string, error) {
	if rawCredential == "" {
		return "", ErrCredentialInvalid
	}
	credential, err := findCredential(ctx, s.pool, digest(rawCredential))
	if err != nil {
		if errors.Is(err, ErrCredentialInvalid) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
	}
	if Scope(credential.Scope) != scope || credential.RevokedAt != nil || !credential.ExpiresAt.After(time.Now().UTC()) {
		return "", ErrCredentialInvalid
	}
	return credential.SenderID, nil
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
		ErrIdentityProvider,
		ErrSenderNotFound,
		ErrSenderRevoked,
		ErrCredentialInvalid,
	} {
		if errors.Is(err, outcome) {
			return true
		}
	}
	return false
}
