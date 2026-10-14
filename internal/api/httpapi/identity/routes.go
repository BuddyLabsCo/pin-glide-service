package identity

import (
	"context"
	"net/http"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/danielgtaylor/huma/v2"
)

// TagIdentity groups the published identity operations.
const TagIdentity = "identity"

// Register installs the Sender authentication operations. A failed operation
// returns the domain's typed outcome; the shared HTTP error model maps it to
// the stable business code the published contract defines.
func Register(api huma.API, service *domain.Service) {
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
		output.Body = CredentialBundle{
			AccountID: bundle.AccountID,
			SenderID:  bundle.SenderID,
			UserSession: IssuedCredential{
				Token:     bundle.UserSession.Value,
				ExpiresAt: bundle.UserSession.ExpiresAt,
			},
			Uploader: IssuedCredential{
				Token:     bundle.Uploader.Value,
				ExpiresAt: bundle.Uploader.ExpiresAt,
			},
			Refresh: IssuedCredential{
				Token:     bundle.Refresh.Value,
				ExpiresAt: bundle.Refresh.ExpiresAt,
			},
		}
		return output, nil
	})
}
