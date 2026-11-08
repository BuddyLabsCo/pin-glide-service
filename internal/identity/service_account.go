package identity

import (
	"context"
	"errors"
	"fmt"
)

// OwnAccount accepts only U or current R authority. Receiver reads do not admit
// a connection or consume the credential's outstanding issuance grant.
func (s *Service) OwnAccount(ctx context.Context, credential string) (profile AccountProfile, err error) {
	senderID, err := s.Authenticate(ctx, credential, ScopeUploader)
	if err != nil && !errors.Is(err, ErrCredentialInvalid) {
		return AccountProfile{}, err
	}
	if err == nil {
		profile, err = accountForSender(ctx, s.pool, senderID)
	} else {
		profile, err = accountForReceiverCredential(ctx, s.pool, digest(credential))
	}
	if err != nil && !isOutcome(err) {
		return AccountProfile{}, fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
	}
	return profile, err
}
