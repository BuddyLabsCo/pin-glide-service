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
	// ModelProviderError marks a model provider that could not issue a
	// temporary credential or answered unusably.
	ModelProviderError ErrorCode = "model_provider_error"
	// DeviceRegistrationRevoked marks an owned Sender or Receiver registration
	// that security revocation closed permanently.
	DeviceRegistrationRevoked ErrorCode = "device_registration_revoked"
	// CredentialMissing marks a protected operation without a credential.
	CredentialMissing ErrorCode = "credential_missing"
	// CredentialInvalid marks a supplied credential that is unknown or that
	// does not authorize the requested operation.
	CredentialInvalid ErrorCode = "credential_invalid"
	// CredentialExpired marks a supplied credential whose expiry has passed.
	CredentialExpired ErrorCode = "credential_expired"
	// CredentialRevoked marks a supplied credential or its credential family
	// as revoked.
	CredentialRevoked ErrorCode = "credential_revoked"
	// RefreshReuseDetected marks confirmed reuse of an already consumed
	// renewal credential; the family it belonged to has been revoked.
	RefreshReuseDetected ErrorCode = "refresh_reuse_detected"
	// OperationSuperseded marks an attempt at or below the attempt another
	// request already committed for the same operation.
	OperationSuperseded ErrorCode = "operation_superseded"
	// OperationMismatch marks a higher attempt that claims a recorded operation
	// without presenting the credential that operation consumed. It neither
	// issues nor revokes anything.
	OperationMismatch ErrorCode = "operation_mismatch"
	// FreshIdentityRequired marks an operation that needs fresh owner
	// verification.
	FreshIdentityRequired ErrorCode = "fresh_identity_required"
	// ReceiverLimitReached marks the five-Receiver association limit.
	ReceiverLimitReached ErrorCode = "receiver_limit_reached"
	// ReceiverAlreadyConnected marks a Receiver that already holds the one
	// reserved/active delivery connection.
	ReceiverAlreadyConnected ErrorCode = "receiver_already_connected"
	// PairingSessionClaimed marks a session another Sender already claimed.
	PairingSessionClaimed ErrorCode = "pairing_session_claimed"
	// PairingSessionExpired marks an authorized caller's expired session.
	PairingSessionExpired ErrorCode = "pairing_session_expired"
	// PairingSessionClosed marks a canceled or activated session.
	PairingSessionClosed ErrorCode = "pairing_session_closed"
	// PairingNotReady marks a Pairing Session that cannot perform the requested
	// step yet, for example because the peer participant is not connected.
	PairingNotReady ErrorCode = "pairing_not_ready"
	// ModeForbidsCredentialIssue marks an add_sender Pairing Session that may
	// not enter the Receiver credential issuance path.
	ModeForbidsCredentialIssue ErrorCode = "mode_forbids_credential_issue"
	// ServiceUnavailable marks temporary service or storage unavailability.
	ServiceUnavailable ErrorCode = "service_unavailable"
	// ModelConfigurationUnavailable marks an absent or disabled model
	// configuration the service cannot issue credentials from.
	ModelConfigurationUnavailable ErrorCode = "model_configuration_unavailable"
	// InternalError marks an unexpected failure with a safe public detail.
	InternalError ErrorCode = "internal_error"
)

// ErrorItem locates one rejected input without echoing the rejected value.
type ErrorItem struct {
	Location string `json:"location,omitempty" doc:"Where the error occurred, e.g. 'body.items[0]'"`
	Message  string `json:"message" doc:"Why the input was rejected"`
}
