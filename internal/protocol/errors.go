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
	// CredentialMissing marks a protected operation without a credential.
	CredentialMissing ErrorCode = "credential_missing"
	// CredentialInvalid marks a credential that authorizes nothing here.
	CredentialInvalid ErrorCode = "credential_invalid"
	// CredentialExpired marks a credential past its recorded lifetime.
	CredentialExpired ErrorCode = "credential_expired"
	// FreshIdentityRequired marks an operation that needs fresh owner
	// verification.
	FreshIdentityRequired ErrorCode = "fresh_identity_required"
	// ReceiverLimitReached marks the five-Receiver association limit.
	ReceiverLimitReached ErrorCode = "receiver_limit_reached"
	// PairingSessionClaimed marks a session another Sender already claimed.
	PairingSessionClaimed ErrorCode = "pairing_session_claimed"
	// PairingSessionExpired marks an authorized caller's expired session.
	PairingSessionExpired ErrorCode = "pairing_session_expired"
	// PairingSessionClosed marks a canceled or activated session.
	PairingSessionClosed ErrorCode = "pairing_session_closed"
	// PairingNotReady marks a Pairing Session that cannot perform the requested
	// step yet: the peer participant may not be connected, or the session may
	// not have both confirmations.
	PairingNotReady ErrorCode = "pairing_not_ready"
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
