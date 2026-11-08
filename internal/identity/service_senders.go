package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
)

// Senders lists every unrevoked Sender of the management credential's Account.
// Logout leaves the registration active, independently of its credentials.
func (s *Service) Senders(ctx context.Context, managementCredential string) (senders []Sender, err error) {
	defer func() {
		if err != nil && !isOutcome(err) {
			err = fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
		}
	}()
	senderID, err := s.Authenticate(ctx, managementCredential, ScopeUserSession)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Recheck under the family mutation lock so concurrent logout or revocation
	// cannot turn an obsolete credential into Account inventory access.
	actor, err := lockSender(ctx, tx, senderID)
	if err != nil {
		return nil, err
	}
	if actor.State == model.IdentitySenderRegistrationState_Revoked {
		return nil, ErrSenderRevoked
	}
	credential, err := lockCredential(ctx, tx, digest(managementCredential))
	if err != nil {
		return nil, err
	}
	if credential.RevokedAt != nil || Scope(credential.Scope) != ScopeUserSession {
		return nil, ErrCredentialInvalid
	}
	if !credential.ExpiresAt.After(time.Now().UTC()) {
		return nil, ErrCredentialExpired
	}
	senders, err = activeSenders(ctx, tx, actor.AccountID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return senders, nil
}
