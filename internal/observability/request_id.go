package observability

import (
	"context"
	"crypto/rand"
)

// RequestIDHeader exposes the correlation identifier of one operation.
const RequestIDHeader = "X-Request-Id"

type requestIDContextKey struct{}

// NewRequestID returns a fresh opaque correlation identifier. It is random
// rather than sequential so callers cannot infer traffic volume.
func NewRequestID() string {
	return "req_" + rand.Text()
}

// WithRequestID attaches the correlation identifier of the current operation.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

// RequestIDFrom returns the correlation identifier of ctx, or "" when absent.
func RequestIDFrom(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDContextKey{}).(string)
	return requestID
}
