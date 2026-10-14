package wss

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// problem is one handshake rejection. It carries the same fields the HTTP
// surface's problem details carry, because a rejected upgrade is an ordinary
// HTTP failure that happens to belong to a WebSocket endpoint.
type problem struct {
	code   protocol.ErrorCode
	status int
	title  string
	detail string
}

// problemFor maps a pairing subscription failure to the published outcome.
// The credential's validity is decided by the domain before the protected
// session state, so an expired restricted credential reports itself rather
// than the session's own expiry.
func problemFor(err error, _ string) problem {
	switch {
	case errors.Is(err, pairing.ErrCredentialMissing):
		return problem{code: protocol.CredentialMissing, status: http.StatusUnauthorized,
			title: "Credential missing", detail: "This operation requires exactly one service credential."}
	case errors.Is(err, pairing.ErrCredentialInvalid):
		return problem{code: protocol.CredentialInvalid, status: http.StatusUnauthorized,
			title: "Credential invalid", detail: "The presented credential is not valid for this operation."}
	case errors.Is(err, pairing.ErrCredentialExpired):
		return problem{code: protocol.CredentialExpired, status: http.StatusUnauthorized,
			title: "Credential expired", detail: "The presented credential has expired; obtain a fresh one before retrying."}
	case errors.Is(err, pairing.ErrSessionNotFound):
		return problem{code: protocol.ResourceNotFound, status: http.StatusNotFound,
			title: "Resource not found", detail: "The requested session does not exist or is not visible to this caller."}
	case errors.Is(err, pairing.ErrSessionExpired):
		return problem{code: protocol.PairingSessionExpired, status: http.StatusGone,
			title: "Pairing session expired", detail: "This Pairing Session passed its absolute five-minute lifetime."}
	case errors.Is(err, pairing.ErrSessionClosed):
		return problem{code: protocol.PairingSessionClosed, status: http.StatusGone,
			title: "Pairing session closed", detail: "This Pairing Session was canceled, replaced or already produced its credential."}
	case errors.Is(err, pairing.ErrStorageUnavailable):
		return problem{code: protocol.ServiceUnavailable, status: http.StatusServiceUnavailable,
			title: "Service unavailable", detail: "The service is temporarily unavailable."}
	default:
		return problem{code: protocol.InternalError, status: http.StatusInternalServerError,
			title: "Internal error", detail: "The service encountered an unexpected error."}
	}
}

// writeProblem answers one rejected upgrade with the problem media type and
// the actual failure status. Only stable machine-readable facts are reported.
func writeProblem(writer http.ResponseWriter, request *http.Request, failure problem) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(failure.status)
	_ = json.NewEncoder(writer).Encode(struct {
		Type      string             `json:"type"`
		Title     string             `json:"title"`
		Status    int                `json:"status"`
		Detail    string             `json:"detail"`
		Code      protocol.ErrorCode `json:"code"`
		RequestID string             `json:"requestId"`
	}{
		Type:      "/problems/" + string(failure.code),
		Title:     failure.title,
		Status:    failure.status,
		Detail:    failure.detail,
		Code:      failure.code,
		RequestID: observability.RequestIDFrom(request.Context()),
	})
}

// bearerToken extracts one bearer credential from an Authorization header
// value. An absent or non-bearer header yields an empty credential, which the
// domain then reports as missing rather than valid.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}
