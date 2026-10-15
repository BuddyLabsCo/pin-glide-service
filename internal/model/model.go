// Package model owns the enabled non-secret model configuration and the
// temporary provider credentials the service issues from it.
package model

import (
	"context"
	"errors"
	"time"
)

const (
	// ProviderBailian is the only model provider the adopted profile permits.
	// Inference itself stays on the Sender, which calls that provider directly.
	ProviderBailian = "bailian"
	// RegionBeijing is the adopted provider region. The service neither invents
	// a workspace nor switches the region implicitly.
	RegionBeijing = "cn-beijing"
	// TemporaryKeyLifetime is the lifetime the service requests for every
	// temporary provider credential. The provider accepts one to 1800 seconds,
	// and this lifetime belongs to the credential, not to a Sender's session.
	TemporaryKeyLifetime = 600 * time.Second
)

// Configuration is one enabled, non-secret model configuration snapshot: the
// provider workspace the Sender calls and the model it asks for. Credentials,
// prompts and inference settings are not part of it.
type Configuration struct {
	WorkspaceID string
	Model       string
}

// Credential is one temporary provider credential: the raw value only its
// holder receives and the provider's actual expiry.
type Credential struct {
	APIKey    string
	ExpiresAt time.Time
}

// Issued is one temporary credential together with the configuration snapshot
// it was issued against, so a caller returns both as they were read together.
type Issued struct {
	Credential    Credential
	Configuration Configuration
}

// Endpoint returns the workspace-specific Beijing endpoint of path. The
// adopted regional host serves both the inference base URL and the provider's
// own API paths for one workspace.
func Endpoint(workspaceID, path string) string {
	return "https://" + workspaceID + "." + RegionBeijing + ".maas.aliyuncs.com" + path
}

// Provider mints one temporary credential for a workspace's parent key. It is
// the external boundary the model service depends on, implemented by the
// Bailian adapter, and every failure it reports is unusable issuance rather
// than a partial success.
type Provider interface {
	TemporaryKey(ctx context.Context, workspaceID string) (Credential, error)
}

// Authenticator resolves the authority one upload credential carries. The
// identity service owns those rules, so this domain reuses them instead of
// restating them, and reports the credential outcomes its transports map.
type Authenticator interface {
	Authenticate(ctx context.Context, rawCredential string) error
}

// Errors the model operations report. Callers distinguish outcomes by these
// typed causes; the HTTP surface maps them to stable business codes, and
// nothing else about them is public.
var (
	// ErrConfigurationUnavailable marks an absent or disabled model
	// configuration: the service has no enabled workspace to issue for.
	ErrConfigurationUnavailable = errors.New("model configuration unavailable")
	// ErrProvider marks a provider that could not mint a temporary credential
	// or answered unusably.
	ErrProvider = errors.New("model provider failed")
	// ErrStorageUnavailable marks a required database read that failed.
	ErrStorageUnavailable = errors.New("storage unavailable")
)
