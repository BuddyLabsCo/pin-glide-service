package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Service implements the enabled model configuration read and the temporary
// provider credential issued from it over PostgreSQL.
type Service struct {
	pool          *sql.DB
	provider      Provider
	authenticator Authenticator
}

// NewService composes the model service. A nil provider is a process composed
// without provider credentials: issuance then reports a provider failure
// instead of calling anyone.
func NewService(pool *sql.DB, provider Provider, authenticator Authenticator) *Service {
	return &Service{pool: pool, provider: provider, authenticator: authenticator}
}

// Credentials issues one temporary provider credential with the enabled
// configuration snapshot it belongs to, for the Sender whose upload credential
// authorizes the request. Authority is decided before the configuration is
// read; the snapshot is read in one statement and the provider is called
// outside any database transaction, so the credential and the configuration
// returned are the ones read and used together.
func (s *Service) Credentials(ctx context.Context, uploadCredential string) (Issued, error) {
	if err := s.authenticator.Authenticate(ctx, uploadCredential); err != nil {
		return Issued{}, err
	}

	configuration, err := enabledConfiguration(ctx, s.pool)
	if err != nil {
		if errors.Is(err, ErrConfigurationUnavailable) {
			return Issued{}, err
		}
		return Issued{}, fmt.Errorf("%w: %w", ErrStorageUnavailable, err)
	}

	if s.provider == nil {
		return Issued{}, ErrProvider
	}
	credential, err := s.provider.TemporaryKey(ctx, configuration.WorkspaceID)
	if err != nil {
		return Issued{}, err
	}
	return Issued{Credential: credential, Configuration: configuration}, nil
}
