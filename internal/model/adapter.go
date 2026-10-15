package model

import (
	"context"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// NewAuthenticator adapts the identity service's credential resolution to the
// upload authority the model credential operation requires, so the credential
// rules of the family it belongs to are stated once and its outcomes are the
// ones the transports already map.
func NewAuthenticator(accounts *identity.Service) Authenticator {
	return uploaderAuthenticator{accounts: accounts}
}

// uploaderAuthenticator resolves one upload credential. The Sender's identity
// is not needed here: upload authority alone authorizes the operation.
type uploaderAuthenticator struct {
	accounts *identity.Service
}

// Authenticate reports whether the presented credential authorizes upload and
// model access, which is the authority this operation requires.
func (a uploaderAuthenticator) Authenticate(ctx context.Context, rawCredential string) error {
	_, err := a.accounts.Authenticate(ctx, rawCredential, identity.ScopeUploader)
	return err
}
