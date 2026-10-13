package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
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
	code := codeForStatus(status)
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

// codeForStatus maps statuses Huma or the router emit without an explicit
// business code onto the documented code vocabulary. Unmapped 4xx statuses
// keep their actual status and report the closest documented code.
func codeForStatus(status int) protocol.ErrorCode {
	switch status {
	case http.StatusBadRequest:
		return protocol.InvalidRequest
	case http.StatusNotFound:
		return protocol.ResourceNotFound
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

// errorItems keeps validation locations and messages, dropping rejected values
// and any internal error detail.
func errorItems(errs []error) []protocol.ErrorItem {
	var items []protocol.ErrorItem
	for _, err := range errs {
		if err == nil {
			continue
		}
		var detailed huma.ErrorDetailer
		if errors.As(err, &detailed) {
			detail := detailed.ErrorDetail()
			if detail == nil {
				continue
			}
			items = append(items, protocol.ErrorItem{Location: detail.Location, Message: detail.Message})
			continue
		}
		items = append(items, protocol.ErrorItem{Message: err.Error()})
	}
	return items
}
