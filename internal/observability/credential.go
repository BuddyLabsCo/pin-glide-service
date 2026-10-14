package observability

import (
	"context"
	"strings"
)

// CredentialHeader names the header one service credential travels in. The
// value itself is never logged, persisted or echoed, and never appears in a
// URL.
const CredentialHeader = "Authorization"

type credentialContextKey struct{}

// WithCredential attaches the bearer credential of the current operation so a
// handler whose required credential depends on the requested mode can resolve
// it without re-reading the header.
func WithCredential(ctx context.Context, credential string) context.Context {
	return context.WithValue(ctx, credentialContextKey{}, credential)
}

// CredentialFrom returns the bearer credential of the current operation, or an
// empty string when the request carries no bearer credential. A handler reports
// the missing credential itself, so each operation states its own requirement.
func CredentialFrom(ctx context.Context) string {
	header, _ := ctx.Value(credentialContextKey{}).(string)
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}
