package delivery

import (
	"context"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// NewAuthenticator adapts the identity service's credential resolution to the
// upload authority the delivery operations require, so the credential rules of
// the family they belong to are stated once and their outcomes are the ones
// the transports already map.
func NewAuthenticator(accounts *identity.Service) Authenticator {
	return uploaderAuthenticator{accounts: accounts}
}

// uploaderAuthenticator resolves one upload credential to the Sender it
// authorizes, which is both the authority and the identity this domain needs:
// the Sender never comes from the request payload.
type uploaderAuthenticator struct {
	accounts *identity.Service
}

// Authenticate reports the Sender the presented credential authorizes for
// upload.
func (a uploaderAuthenticator) Authenticate(ctx context.Context, rawCredential string) (string, error) {
	return a.accounts.Authenticate(ctx, rawCredential, identity.ScopeUploader)
}
