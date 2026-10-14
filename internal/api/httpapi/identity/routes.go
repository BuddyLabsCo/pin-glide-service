package identity

import (
	"context"
	"fmt"
	"net/http"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/danielgtaylor/huma/v2"
)

// TagIdentity groups the published identity operations.
const TagIdentity = "identity"

// Register installs the Sender authentication operations. A failed operation
// returns the domain's typed outcome; the shared HTTP error model maps it to
// the stable business code the published contract defines.
func Register(api huma.API, service *domain.Service) {
	registerLogin(api, service)
	registerRefresh(api, service)
	registerLogout(api, service)
}

// registerLogin serves the fresh WeChat login that creates or restores one
// Sender installation.
func registerLogin(api huma.API, service *domain.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "PostAuthWechat",
		Tags:        []string{TagIdentity},
		Method:      http.MethodPost,
		Path:        "/v1/auth/wechat",
		Summary:     "Verify WeChat login and issue Sender credentials",
		Description: "Verifies a fresh WeChat authorization code, maps it to its Account and creates or restores this installation's Sender registration, replacing that Sender's previous credential family. " +
			"Omitting senderId registers a new Sender; supplying one restores only an owned active registration. " +
			"Failures report 401 wechat_authorization_invalid, 404 resource_not_found for an unknown or foreign senderId, 409 device_registration_revoked for a permanently revoked one, 502 identity_provider_error or 503 service_unavailable. " +
			"Raw M, U and F credentials are returned only to this holder and the response is never cacheable.",
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusNotFound,
			http.StatusConflict,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
		},
		// A fresh WeChat code is the credential of this operation, so it is
		// deliberately anonymous.
		Security: []map[string][]string{},
	}, func(ctx context.Context, input *loginInput) (*loginOutput, error) {
		bundle, err := service.Login(ctx, domain.LoginRequest{
			WeChatCode: input.Body.WeChatCode,
			SenderID:   input.Body.SenderID,
		})
		if err != nil {
			return nil, err
		}

		output := &loginOutput{}
		output.Body = credentialBundle(bundle)
		return output, nil
	})
}

// registerRefresh serves ordered renewal of one Sender's credential family.
// Its description states the client's adopted renewal contract, because that
// contract is what the published operation is built around, and states the
// retransmission window from its domain constant so the published contract
// cannot advertise a window the service does not enforce.
func registerRefresh(api huma.API, service *domain.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "PostAuthRefresh",
		Tags:        []string{TagIdentity},
		Method:      http.MethodPost,
		Path:        "/v1/auth/refresh",
		Summary:     "Rotate the Sender credential bundle",
		Description: fmt.Sprintf("Rotates the Sender's userSession, uploader and refresh credentials through one operation ID and its attempt number. "+
			"The client persists that operation ID with the current bundle before it sends it, keeps one single-flight renewal at a time, stores a returned bundle atomically in secure storage, and discards a response that belongs to an inactive local login or logout generation. "+
			"Attempt 0 is the original request and attempt 1 its one permitted retransmission of an uncertain outcome; either can commit first, and the service never replays a lost response. "+
			"A recorded attempt equal to or higher than the submitted one reports 409 operation_superseded without issuing or revoking anything. "+
			"A higher attempt of the recorded operation recovers one lost response only while the consumed token is that operation's recorded predecessor, inside a %g-second overlap and before that token's original expiry, and receives a freshly minted bundle. "+
			"A higher attempt that presents a credential no operation has consumed is not that operation's retransmission and reports 409 operation_mismatch without issuing or revoking anything. "+
			"Any other use of a consumed token reports 401 refresh_reuse_detected and revokes only that Sender's family, so unknown or expired tokens never revoke anything. "+
			"Existing userSession and uploader tokens keep their original expiry during ordinary rotation. Raw credentials appear only in this response, which is never cacheable.", domain.RefreshOverlap.Seconds()),
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusConflict,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
		// The renewal credential is the authority of this operation, and it
		// travels in the body rather than in a header or URL.
		Security: []map[string][]string{},
	}, func(ctx context.Context, input *refreshInput) (*refreshOutput, error) {
		rotated, err := service.Refresh(ctx, domain.RefreshRequest{
			RefreshToken: input.Body.RefreshToken,
			OperationID:  input.Body.OperationID,
			Attempt:      input.Body.Attempt,
		})
		if err != nil {
			return nil, err
		}

		output := &refreshOutput{}
		output.Body = RefreshBundle{
			CredentialBundle: credentialBundle(rotated),
			OperationID:      input.Body.OperationID,
			Attempt:          input.Body.Attempt,
		}
		return output, nil
	})
}

// registerLogout serves ordinary logout: one Sender revoking its own service
// access through exactly one credential source, including while its access
// credentials have expired. Its description states what logout does not touch
// and the client's obligation not to claim a sign-out the service never
// confirmed.
func registerLogout(api huma.API, service *domain.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "PostAuthLogout",
		Tags:        []string{TagIdentity},
		Method:      http.MethodPost,
		Path:        "/v1/auth/logout",
		Summary:     "Revoke the Sender's service access",
		Description: "Revokes the current Sender's credential family, which is both access scopes and the renewal credential, without permanently closing its Device Registration and without removing its Pairings; other installations and a family a fresh login replaced keep working. " +
			"The Sender's own authority for this operation is exactly one source: its management or upload credential in the Authorization header, or its renewal credential in the refreshToken body field. " +
			"Renewing access first is never required, so a renewal credential authorizes logout while the access credentials have expired. " +
			"A request offering both sources is rejected as 422 validation_failed, as was one without a usable source: an absent credential reports 401 credential_missing, an unknown or wrongly scoped one 401 credential_invalid, an expired one 401 credential_expired, and a credential an earlier logout or a fresh login revoked 401 credential_revoked. " +
			"A request that never reached the service revokes nothing, and the client stops forwarding locally at once without claiming remote sign-out it did not receive. " +
			"Account switching, which also removes this Sender's own Pairings, is not part of this release. " +
			"This response carries no credential and is never cacheable.",
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
		// This operation's credential travels in the Authorization header or in
		// the body, so the published operation states no bearer requirement.
		Security: []map[string][]string{},
	}, func(ctx context.Context, input *logoutInput) (*struct{}, error) {
		request := domain.LogoutRequest{AccessToken: observability.CredentialFrom(ctx)}
		if input.Body != nil {
			request.RefreshToken = input.Body.RefreshToken
		}
		if err := service.Logout(ctx, request); err != nil {
			return nil, err
		}
		return nil, nil
	})
}
