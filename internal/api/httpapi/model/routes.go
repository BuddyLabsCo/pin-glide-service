package model

import (
	"context"
	"net/http"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/danielgtaylor/huma/v2"
)

// Register installs the model credential operation. A failed operation
// returns the domain's typed outcome; the shared HTTP error model maps it to
// the stable business code the published contract defines.
func Register(api huma.API, service *domain.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "PostModelCredentials",
		Tags:        []string{TagModel},
		Method:      http.MethodPost,
		Path:        "/v1/model/credentials",
		Summary:     "Issue a temporary Bailian credential with its configuration",
		Description: "Issues one short-lived Bailian temporary API key together with the enabled model configuration snapshot it belongs to, so the Sender can call the configured model directly. " +
			"Upload authority is required: an absent credential reports 401 credential_missing, and invalidated upload authority mints nothing: an unknown or wrongly scoped credential, and one an earlier logout, fresh login or family-reuse revocation invalidated, report 401 credential_invalid; an expired one reports 401 credential_expired; and one whose Sender registration an owner permanently closed reports 409 device_registration_revoked. The service cannot revoke a key it already issued, so invalidation stops new issuance and leaves issued keys to their provider lifetime. " +
			"The configuration is read as one snapshot and the provider is called outside any database transaction, so the returned configuration is the one the credential was issued for. The service requests a 600-second lifetime and reports the provider's actual expiry converted to RFC 3339 UTC; the Sender caches the credential until that expiry and requests another afterwards. " +
			"The configuration states the workspace-specific Beijing base URL, the enabled row's model (the adopted default is `qwen3.7-flash`), text-only OpenAI-compatible chat completions with `enable_thinking` explicitly false, and a strict JSON Schema result with one required nullable-string `otp` and no additional properties. The Sender's SMS prompt, the inference call, credential caching, output-field validation, OTP membership in the original SMS and the decision not to upload an absent or invalid OTP all stay on the Sender: the service never sees SMS text, an OTP or inferred output, and it does not fall back to unconstrained JSON Object output or another provider. " +
			"Failures report 401 credential_missing, credential_invalid, credential_expired, 409 device_registration_revoked for a permanently closed Sender registration, 502 model_provider_error when the provider cannot be reached or answers unusably, 503 model_configuration_unavailable when no enabled configuration exists, or 503 service_unavailable. " +
			"This response carries the raw temporary credential and is never cacheable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusConflict,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, _ *struct{}) (*credentialOutput, error) {
		issued, err := service.Credentials(ctx, observability.CredentialFrom(ctx))
		if err != nil {
			return nil, err
		}

		output := &credentialOutput{}
		output.Body = wireCredential(issued)
		return output, nil
	})
}
