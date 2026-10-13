// Package observability owns structured logging, request correlation and
// redaction for runtime processes.
package observability

import (
	"io"
	"log/slog"
)

// NewLogger returns the structured logger used by runtime processes. Log
// records never carry request bodies, credentials or provider payloads.
func NewLogger(writer io.Writer, environment string) *slog.Logger {
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler).With(slog.String("environment", environment))
}
