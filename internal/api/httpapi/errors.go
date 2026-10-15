package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api/wss"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
)

// Problem is the RFC 9457 problem detail returned for every whole-request HTTP
// failure: handler failures, authentication middleware and routing fallbacks.
// Type is a URI reference so it stays relative to the served origin, and the
// error detail list carries validation locations and messages only.
type Problem struct {
	Type      string               `json:"type" format:"uri-reference" example:"/problems/invalid_request" doc:"Problem type identifier; always /problems/{code}"`
	Title     string               `json:"title" example:"Invalid request" doc:"Stable English summary of the problem type"`
	Status    int                  `json:"status" example:"400" doc:"HTTP status code; matches the response status"`
	Detail    string               `json:"detail" doc:"Safe explanation of this occurrence; clients must not parse it for control flow"`
	Code      protocol.ErrorCode   `json:"code" example:"invalid_request" doc:"Stable machine-readable error code"`
	RequestID string               `json:"requestId" doc:"Service-generated operation identifier for log correlation"`
	Errors    []protocol.ErrorItem `json:"errors,omitempty" doc:"Validation failures as location and message only"`
}

// Error satisfies the error interface so problems can be returned from handlers.
func (p *Problem) Error() string {
	return p.Detail
}

// GetStatus returns the HTTP status of this problem.
func (p *Problem) GetStatus() int {
	return p.Status
}

// ContentType makes problem responses use the RFC 9457 media type.
func (p *Problem) ContentType(contentType string) string {
	if contentType == "application/json" {
		return "application/problem+json"
	}
	return contentType
}

// errorCodeDefinition maps one code to its stable HTTP status, title and
// generic detail. Code and type map one to one through Type = /problems/{code}.
type errorCodeDefinition struct {
	status int
	title  string
	detail string
}

var errorCodeDefinitions = map[protocol.ErrorCode]errorCodeDefinition{
	protocol.InvalidRequest: {
		status: http.StatusBadRequest,
		title:  "Invalid request",
		detail: "The request is malformed or missing a required body.",
	},
	protocol.ResourceNotFound: {
		status: http.StatusNotFound,
		title:  "Resource not found",
		detail: "The requested resource does not exist.",
	},
	protocol.MethodNotAllowed: {
		status: http.StatusMethodNotAllowed,
		title:  "Method not allowed",
		detail: "The method is not supported for this resource.",
	},
	protocol.ValidationFailed: {
		status: http.StatusUnprocessableEntity,
		title:  "Validation failed",
		detail: "The request did not pass validation.",
	},
	protocol.WeChatAuthorizationInvalid: {
		status: http.StatusUnauthorized,
		title:  "WeChat authorization invalid",
		detail: "The WeChat authorization code is invalid or has already been used.",
	},
	protocol.IdentityProviderError: {
		status: http.StatusBadGateway,
		title:  "Identity provider error",
		detail: "The WeChat identity provider could not verify the authorization.",
	},
	protocol.DeviceRegistrationRevoked: {
		status: http.StatusConflict,
		title:  "Device registration revoked",
		detail: "This Sender registration was permanently security-revoked and cannot be restored.",
	},
	protocol.CredentialMissing: {
		status: http.StatusUnauthorized,
		title:  "Credential missing",
		detail: "This operation requires exactly one service credential.",
	},
	protocol.CredentialInvalid: {
		status: http.StatusUnauthorized,
		title:  "Credential invalid",
		detail: "The supplied credential does not authorize this operation.",
	},
	protocol.CredentialExpired: {
		status: http.StatusUnauthorized,
		title:  "Credential expired",
		detail: "The supplied credential has expired.",
	},
	protocol.CredentialRevoked: {
		status: http.StatusUnauthorized,
		title:  "Credential revoked",
		detail: "The supplied credential has been revoked.",
	},
	protocol.RefreshReuseDetected: {
		status: http.StatusUnauthorized,
		title:  "Refresh reuse detected",
		detail: "The renewal credential was already consumed, so its credential family was revoked.",
	},
	protocol.OperationSuperseded: {
		status: http.StatusConflict,
		title:  "Operation superseded",
		detail: "A recorded attempt of this operation is equal to or higher than the submitted attempt.",
	},
	protocol.OperationMismatch: {
		status: http.StatusConflict,
		title:  "Operation mismatch",
		detail: "The recorded operation consumed a different renewal credential, so this attempt is not that operation's retransmission.",
	},
	protocol.FreshIdentityRequired: {
		status: http.StatusForbidden,
		title:  "Fresh identity required",
		detail: "This operation requires fresh verification of the owning WeChat Account.",
	},
	protocol.ReceiverLimitReached: {
		status: http.StatusConflict,
		title:  "Receiver limit reached",
		detail: "A Sender can have at most 5 associated Receivers.",
	},
	protocol.ReceiverAlreadyConnected: {
		status: http.StatusConflict,
		title:  "Receiver already connected",
		detail: "This Receiver already holds the one reserved or active delivery connection.",
	},
	protocol.PairingSessionClaimed: {
		status: http.StatusConflict,
		title:  "Pairing session claimed",
		detail: "Another Sender already claimed this Pairing Session.",
	},
	protocol.PairingSessionExpired: {
		status: http.StatusGone,
		title:  "Pairing session expired",
		detail: "This Pairing Session passed its absolute five-minute lifetime.",
	},
	protocol.PairingSessionClosed: {
		status: http.StatusGone,
		title:  "Pairing session closed",
		detail: "This Pairing Session was canceled, replaced or already produced its credential.",
	},
	protocol.PairingNotReady: {
		status: http.StatusConflict,
		title:  "Pairing not ready",
		detail: "The Pairing Session cannot perform this step yet.",
	},
	protocol.ModeForbidsCredentialIssue: {
		status: http.StatusConflict,
		title:  "Mode forbids credential issue",
		detail: "An add_sender Pairing Session cannot issue or replace a Receiver credential.",
	},
	protocol.ServiceUnavailable: {
		status: http.StatusServiceUnavailable,
		title:  "Service unavailable",
		detail: "The service is temporarily unavailable.",
	},
	protocol.InternalError: {
		status: http.StatusInternalServerError,
		title:  "Internal error",
		detail: "The service encountered an unexpected error.",
	},
}

// NewProblem builds the problem for code, stamped with the request ID of the
// current operation. Handlers, middleware and routing fallbacks all return the
// same shape, and the code alone selects the HTTP status and stable title.
func NewProblem(ctx context.Context, code protocol.ErrorCode, detail string, items ...protocol.ErrorItem) *Problem {
	definition, ok := errorCodeDefinitions[code]
	if !ok {
		// An unregistered code is a programming error; never put an unstable
		// type or status in front of a client.
		definition = errorCodeDefinitions[protocol.InternalError]
		code = protocol.InternalError
		detail = definition.detail
		items = nil
	}
	if detail == "" {
		detail = definition.detail
	}
	return &Problem{
		Type:      "/problems/" + string(code),
		Title:     definition.title,
		Status:    definition.status,
		Detail:    detail,
		Code:      code,
		RequestID: observability.RequestIDFrom(ctx),
		Errors:    items,
	}
}

// WriteUpgradeProblem answers one WSS subscription the transport refused before
// upgrading with the shared problem document, and reports the code it published
// so the surface that refused the connection logs what the client receives.
func WriteUpgradeProblem(writer http.ResponseWriter, request *http.Request, reason error) protocol.ErrorCode {
	problem := problemForStatus(http.StatusInternalServerError, observability.RequestIDFrom(request.Context()), []error{reason})
	writeProblem(writer, request, problem)
	return problem.Code
}

// configureErrorModel replaces Huma's default error model so generated
// documents and runtime responses share one shape. Huma's default model omits
// code and requestId and echoes rejected input values.
func configureErrorModel() {
	huma.NewError = func(status int, _ string, errs ...error) huma.StatusError {
		return problemForStatus(status, "", errs)
	}
	huma.NewErrorWithContext = func(ctx huma.Context, status int, _ string, errs ...error) huma.StatusError {
		return problemForStatus(status, observability.RequestIDFrom(ctx.Context()), errs)
	}
}

func problemForStatus(status int, requestID string, errs []error) *Problem {
	code, outcome := outcomeCode(errs)
	if outcome {
		// The definition, not Huma's generic failure status, states the status
		// of a business outcome.
		status = errorCodeDefinitions[code].status
	} else {
		code = codeForStatus(status)
	}
	definition := errorCodeDefinitions[code]

	problem := &Problem{
		Type:      "/problems/" + string(code),
		Title:     definition.title,
		Status:    status,
		Detail:    definition.detail,
		Code:      code,
		RequestID: requestID,
	}
	if code != protocol.InternalError {
		problem.Errors = errorItems(errs)
	}
	return problem
}

// outcomeCode returns the business code of the first typed domain outcome in
// errs. Handlers report domain failures as errors and rely on this model to
// name the HTTP outcome, so a domain failure that no entry here recognizes
// stays a safe internal error instead of leaking its text.
func outcomeCode(errs []error) (protocol.ErrorCode, bool) {
	for _, err := range errs {
		for _, outcome := range businessOutcomes {
			if errors.Is(err, outcome.cause) {
				return outcome.code, true
			}
		}
	}
	return "", false
}

// businessOutcomes maps the typed causes domain operations report to the
// stable business code clients see. Code and status stay one-to-one through
// errorCodeDefinitions.
var businessOutcomes = []struct {
	cause error
	code  protocol.ErrorCode
}{
	{identity.ErrWeChatAuthorizationInvalid, protocol.WeChatAuthorizationInvalid},
	{identity.ErrIdentityProvider, protocol.IdentityProviderError},
	{identity.ErrSenderNotFound, protocol.ResourceNotFound},
	{identity.ErrSenderRevoked, protocol.DeviceRegistrationRevoked},
	{identity.ErrCredentialMissing, protocol.CredentialMissing},
	{identity.ErrCredentialInvalid, protocol.CredentialInvalid},
	{identity.ErrCredentialExpired, protocol.CredentialExpired},
	{identity.ErrCredentialRevoked, protocol.CredentialRevoked},
	{identity.ErrCredentialSourceConflicting, protocol.ValidationFailed},
	{identity.ErrRefreshReuse, protocol.RefreshReuseDetected},
	{identity.ErrOperationSuperseded, protocol.OperationSuperseded},
	{identity.ErrOperationMismatch, protocol.OperationMismatch},
	{identity.ErrAttemptUnsupported, protocol.ValidationFailed},
	{identity.ErrStorageUnavailable, protocol.ServiceUnavailable},
	{pairing.ErrSenderRevoked, protocol.DeviceRegistrationRevoked},
	{pairing.ErrSenderNotOwned, protocol.ResourceNotFound},
	{pairing.ErrCredentialMissing, protocol.CredentialMissing},
	{pairing.ErrCredentialInvalid, protocol.CredentialInvalid},
	{pairing.ErrCredentialExpired, protocol.CredentialExpired},
	{pairing.ErrCredentialRevoked, protocol.CredentialRevoked},
	{pairing.ErrStorageUnavailable, protocol.ServiceUnavailable},
	{pairing.ErrSessionNotFound, protocol.ResourceNotFound},
	{pairing.ErrSessionExpired, protocol.PairingSessionExpired},
	{pairing.ErrSessionClosed, protocol.PairingSessionClosed},
	{pairing.ErrSessionClaimed, protocol.PairingSessionClaimed},
	{pairing.ErrReceiverLimitReached, protocol.ReceiverLimitReached},
	{pairing.ErrPairingNotReady, protocol.PairingNotReady},
	{pairing.ErrModeForbidsCredentialIssue, protocol.ModeForbidsCredentialIssue},
	{pairing.ErrOperationSuperseded, protocol.OperationSuperseded},
	{pairing.ErrAttemptUnsupported, protocol.ValidationFailed},
	{pairing.ErrIssuanceInvalid, protocol.ValidationFailed},
	{pairing.ErrInvalidMode, protocol.ValidationFailed},
	{pairing.ErrReceiverNotOwned, protocol.ResourceNotFound},
	{pairing.ErrAssociationNotFound, protocol.ResourceNotFound},
	{pairing.ErrFreshIdentityRequired, protocol.FreshIdentityRequired},
	{pairing.ErrWeChatAuthorizationInvalid, protocol.WeChatAuthorizationInvalid},
	{pairing.ErrIdentityProvider, protocol.IdentityProviderError},
	{pairing.ErrCredentialMissing, protocol.CredentialMissing},
	{pairing.ErrCredentialInvalid, protocol.CredentialInvalid},
	{pairing.ErrCredentialExpired, protocol.CredentialExpired},
	{pairing.ErrStorageUnavailable, protocol.ServiceUnavailable},
	{wss.ErrReceiverAlreadyConnected, protocol.ReceiverAlreadyConnected},
	{wss.ErrReceiverUnavailable, protocol.ServiceUnavailable},
	{wss.ErrSubprotocolRequired, protocol.InvalidRequest},
}

// codeForStatus names the code reported when the service has only an HTTP
// status, such as a Huma validation failure. errorCodeDefinitions stays the
// authority for each code's status; this representative map is separate and
// deliberate because several business codes share one status in the v1
// vocabulary, so the reported code cannot be derived from that table. Add a
// representative here whenever a new status joins it. Unmapped 4xx statuses
// keep their actual status and report the closest documented code.
func codeForStatus(status int) protocol.ErrorCode {
	switch status {
	case http.StatusBadRequest:
		return protocol.InvalidRequest
	case http.StatusNotFound:
		return protocol.ResourceNotFound
	case http.StatusUnauthorized:
		return protocol.CredentialInvalid
	case http.StatusMethodNotAllowed:
		return protocol.MethodNotAllowed
	case http.StatusUnprocessableEntity:
		return protocol.ValidationFailed
	case http.StatusServiceUnavailable:
		return protocol.ServiceUnavailable
	default:
		if status >= http.StatusInternalServerError {
			return protocol.InternalError
		}
		return protocol.InvalidRequest
	}
}

// errorItems keeps validation locations and messages, dropping rejected values.
// An error that cannot state where it failed is dropped as well: its text may
// be a provider response, SQL detail, credential or other internal description
// that must never become client-visible.
func errorItems(errs []error) []protocol.ErrorItem {
	var items []protocol.ErrorItem
	for _, err := range errs {
		var detailed huma.ErrorDetailer
		if err == nil || !errors.As(err, &detailed) {
			continue
		}
		detail := detailed.ErrorDetail()
		if detail == nil {
			continue
		}
		items = append(items, protocol.ErrorItem{Location: detail.Location, Message: detail.Message})
	}
	return items
}
