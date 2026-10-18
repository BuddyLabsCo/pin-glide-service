// Package wechat fakes the WeChat identity boundary for tests that compose the
// real service against a disposable database.
package wechat

import (
	"context"
	"sync"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// Fake answers Verify for the authorization codes a test registered. An
// unregistered code is rejected the way the real provider rejects an unusable
// one, so a test states every accepted identity explicitly.
type Fake struct {
	mutex      sync.Mutex
	identities map[string]identity.Identity
	failures   map[string]error
}

// New returns a fake that accepts no code yet.
func New() *Fake {
	return &Fake{
		identities: map[string]identity.Identity{},
		failures:   map[string]error{},
	}
}

// Accept maps one authorization code to the identity the provider verified.
func (f *Fake) Accept(code string, verified identity.Identity) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	verified.Provider = identity.ProviderWeChat
	f.identities[code] = verified
}

// Fail makes one authorization code report err, for example a provider
// failure.
func (f *Fake) Fail(code string, err error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.failures[code] = err
}

// Kind reports the provider this fake verifies.
func (f *Fake) Kind() identity.ProviderKind {
	return identity.ProviderWeChat
}

// Verify implements identity.Provider.
func (f *Fake) Verify(_ context.Context, code string) (identity.Identity, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if err, failed := f.failures[code]; failed {
		return identity.Identity{}, err
	}
	if verified, accepted := f.identities[code]; accepted {
		return verified, nil
	}
	return identity.Identity{}, identity.ErrWeChatAuthorizationInvalid
}
