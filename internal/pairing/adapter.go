package pairing

import (
	"context"
	"errors"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// NewProvider adapts the WeChat verification boundary the identity service
// already uses to the fresh owner proof a recovery session requires. The
// pairing domain also needs the Account the verified identity belongs to,
// which is what makes a recovery claim an owner action rather than an
// anonymous one.
func NewProvider(provider identity.Provider, accounts *identity.Service) Provider {
	if provider == nil {
		// No provider credentials: recovery reports a provider failure instead
		// of calling anyone.
		return nil
	}
	return wechatProofProvider{provider: provider, accounts: accounts}
}

// NewAuthenticator adapts the identity service's credential resolution to the
// Sender management credential the pairing channel authenticates.
func NewAuthenticator(accounts *identity.Service) Authenticator {
	return senderAuthenticator{accounts: accounts}
}

// wechatProofProvider turns one verified WeChat code into the Account it
// belongs to.
type wechatProofProvider struct {
	provider identity.Provider
	accounts *identity.Service
}

// VerifyIdentity verifies the code and resolves its Account. An unknown
// identity reports the provider's own outcome.
func (p wechatProofProvider) VerifyIdentity(ctx context.Context, wechatCode string) (WeChatProof, error) {
	verified, err := p.provider.Verify(ctx, wechatCode)
	if err != nil {
		return WeChatProof{}, providerOutcome(err)
	}
	accountID, err := p.accounts.AccountFor(ctx, verified)
	if err != nil {
		return WeChatProof{}, providerOutcome(err)
	}
	return WeChatProof{AccountID: accountID}, nil
}

// senderAuthenticator resolves one Sender management credential.
type senderAuthenticator struct {
	accounts *identity.Service
}

// Authenticate returns the Sender the credential authorizes for management.
func (a senderAuthenticator) Authenticate(ctx context.Context, rawCredential, scope string) (string, error) {
	senderID, err := a.accounts.Authenticate(ctx, rawCredential, identity.Scope(scope))
	if err != nil {
		return "", credentialOutcome(err)
	}
	return senderID, nil
}

// credentialOutcome restates the identity service's credential results in the
// pairing domain's own vocabulary, so its transports map one set of causes
// rather than two.
func credentialOutcome(err error) error {
	switch {
	case errors.Is(err, identity.ErrCredentialMissing):
		return ErrCredentialMissing
	case errors.Is(err, identity.ErrCredentialInvalid):
		return ErrCredentialInvalid
	case errors.Is(err, identity.ErrCredentialExpired):
		return ErrCredentialExpired
	case errors.Is(err, identity.ErrStorageUnavailable):
		return ErrStorageUnavailable
	default:
		return err
	}
}

// providerOutcome restates one WeChat verification failure. An identity the
// provider verified but that has no Account cannot be the owner of an
// existing Receiver, which is the same answer as proving another Account.
func providerOutcome(err error) error {
	switch {
	case errors.Is(err, identity.ErrWeChatAuthorizationInvalid):
		return ErrWeChatAuthorizationInvalid
	case errors.Is(err, identity.ErrIdentityProvider):
		return ErrIdentityProvider
	case errors.Is(err, identity.ErrSenderNotFound):
		return ErrFreshIdentityRequired
	case errors.Is(err, identity.ErrStorageUnavailable):
		return ErrStorageUnavailable
	default:
		return err
	}
}
