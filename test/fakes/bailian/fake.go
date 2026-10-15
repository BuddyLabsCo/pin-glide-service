// Package bailian fakes the Bailian temporary-key boundary for tests that
// compose the real service against a disposable database.
package bailian

import (
	"context"
	"fmt"
	"sync"

	"github.com/BuddyLabsCo/pin-glide-service/internal/model"
)

// Request is one issuance attempt this fake observed.
type Request struct {
	WorkspaceID string
}

// Fake answers TemporaryKey with the credential a test registered. A request
// no test answered is a provider failure, so every issued credential is stated
// explicitly.
type Fake struct {
	mutex      sync.Mutex
	credential model.Credential
	answered   bool
	failure    error
	gate       chan struct{}
	arrived    chan struct{}
	requests   []Request
}

// New returns a fake that answers no request yet.
func New() *Fake {
	return &Fake{}
}

// Answer makes every later request report credential, replacing an earlier
// failure or answer.
func (f *Fake) Answer(credential model.Credential) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.credential, f.answered, f.failure = credential, true, nil
}

// Fail makes every later request report err, for example a provider outage, a
// timeout or an answer the adapter refused.
func (f *Fake) Fail(err error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.failure, f.answered = err, false
}

// Hold makes later requests wait until Release and returns a channel that is
// closed once one request has arrived, so a test can change the configuration
// while an issuance is in flight.
func (f *Fake) Hold() <-chan struct{} {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.gate, f.arrived = make(chan struct{}), make(chan struct{})
	return f.arrived
}

// Release lets requests held by Hold continue.
func (f *Fake) Release() {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if f.gate == nil {
		return
	}
	close(f.gate)
	f.gate = nil
}

// Requests returns the issuance attempts this fake observed, in order.
func (f *Fake) Requests() []Request {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	return append([]Request(nil), f.requests...)
}

// TemporaryKey implements model.Provider.
func (f *Fake) TemporaryKey(ctx context.Context, workspaceID string) (model.Credential, error) {
	f.mutex.Lock()
	f.requests = append(f.requests, Request{WorkspaceID: workspaceID})
	if f.arrived != nil {
		close(f.arrived)
		f.arrived = nil
	}
	gate, credential, answered, failure := f.gate, f.credential, f.answered, f.failure
	f.mutex.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return model.Credential{}, fmt.Errorf("%w: %w", model.ErrProvider, ctx.Err())
		}
	}
	switch {
	case failure != nil:
		return model.Credential{}, failure
	case !answered:
		return model.Credential{}, fmt.Errorf("%w: no answer was registered", model.ErrProvider)
	}
	return credential, nil
}
