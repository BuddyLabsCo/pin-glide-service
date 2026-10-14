// Package protocol owns the shared HTTP and WebSocket wire types and error
// definitions used by the API surfaces.
package protocol

// ErrorCode is a stable, machine-readable error code. Clients match on codes,
// never on titles or details; an unfamiliar code is still a failure.
type ErrorCode string

const (
	// InvalidRequest marks a request the service could not accept as sent.
	InvalidRequest ErrorCode = "invalid_request"
	// ResourceNotFound marks a missing resource, or one invisible to the caller.
	ResourceNotFound ErrorCode = "resource_not_found"
	// MethodNotAllowed marks a routing fallback for an unsupported HTTP method.
	MethodNotAllowed ErrorCode = "method_not_allowed"
	// ValidationFailed marks whole-request shape or field validation failures.
	ValidationFailed ErrorCode = "validation_failed"
	// WeChatAuthorizationInvalid marks a WeChat authorization code the provider
	// rejected, so the caller must obtain a fresh one.
	WeChatAuthorizationInvalid ErrorCode = "wechat_authorization_invalid"
	// IdentityProviderError marks a WeChat provider that could not verify an
	// authorization code.
	IdentityProviderError ErrorCode = "identity_provider_error"
	// DeviceRegistrationRevoked marks an owned Sender registration that
	// security revocation closed permanently.
	DeviceRegistrationRevoked ErrorCode = "device_registration_revoked"
	// ServiceUnavailable marks temporary service or storage unavailability.
	ServiceUnavailable ErrorCode = "service_unavailable"
	// InternalError marks an unexpected failure with a safe public detail.
	InternalError ErrorCode = "internal_error"
)

// ErrorItem locates one rejected input without echoing the rejected value.
type ErrorItem struct {
	Location string `json:"location,omitempty" doc:"Where the error occurred, e.g. 'body.items[0]'"`
	Message  string `json:"message" doc:"Why the input was rejected"`
}
