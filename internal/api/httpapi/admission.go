package httpapi

import (
	"context"
	"sync"
)

type sessionAdmissionContextKey struct{}

// WithSessionAdmission attaches the admission release of the current request,
// so a handler whose work outlives its transport holds that admission until
// the work is really finished.
func WithSessionAdmission(ctx context.Context, release func()) context.Context {
	return context.WithValue(ctx, sessionAdmissionContextKey{}, release)
}

// SessionAdmissionFrom returns the admission release of the current request.
// A caller that takes it owns calling it exactly once.
func SessionAdmissionFrom(ctx context.Context) func() {
	release, _ := ctx.Value(sessionAdmissionContextKey{}).(func())
	return release
}

// Admission is the bounded admission gate for runtime work. Requests use it
// now; WSS connections and admitted Deliveries extend it by holding a token for
// work that outlives its transport. Shutdown closes the gate to reject new work
// and then waits for in-flight work within the shutdown deadline.
type Admission struct {
	mutex  sync.Mutex
	closed bool
	active int
	idle   chan struct{}
}

// NewAdmission returns an open admission gate.
func NewAdmission() *Admission {
	return &Admission{}
}

// Begin admits one unit of work. Once the gate is closed it reports ok=false,
// and otherwise returns a release function that must be called exactly once
// when the work ends.
func (a *Admission) Begin() (release func(), ok bool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if a.closed {
		return nil, false
	}
	if a.active == 0 {
		a.idle = make(chan struct{})
	}
	a.active++

	var once sync.Once
	return func() {
		once.Do(func() {
			a.mutex.Lock()
			defer a.mutex.Unlock()
			a.active--
			if a.active == 0 {
				close(a.idle)
				a.idle = nil
			}
		})
	}, true
}

// Close stops admitting new work. Already admitted work keeps its admission.
func (a *Admission) Close() {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.closed = true
}

// Wait blocks until no admitted work remains or ctx is done. Call Close first
// so no new work can arrive while waiting.
func (a *Admission) Wait(ctx context.Context) error {
	a.mutex.Lock()
	idle := a.idle
	a.mutex.Unlock()

	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
