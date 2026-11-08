package identity

import (
	"context"
	"net/http"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/danielgtaylor/huma/v2"
)

// AccountProfile is the caller's own service Account and cached display data.
type AccountProfile struct {
	AccountID string  `json:"accountId" doc:"Internal opaque service Account identifier; not a numeric QQ number or OpenID"`
	Nickname  *string `json:"nickname" doc:"Cached QQ nickname; null when no profile has been cached"`
	AvatarURL *string `json:"avatarUrl" doc:"Cached QQ avatar URL; null when no profile has been cached"`
}

type accountOutput struct {
	Body AccountProfile
}

func registerOwnAccount(api huma.API, service *domain.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "GetSelfAccount",
		Tags:        []string{TagIdentity},
		Method:      http.MethodGet,
		Path:        "/v1/self/account",
		Summary:     "Read the authenticated device's own Account",
		Description: "Accepts a Sender U credential or an active current-generation Receiver R credential in the Bearer Authorization header. M, F and restricted Pairing credentials are not accepted. " +
			"Resolves the Account entirely from the validated credential; no caller-selected Account ID is accepted. Returns accountId and the shared cached nickname/avatarUrl, with null display fields for Accounts without a cached profile, including existing WeChat Accounts. " +
			"Reads never call QQ, refresh or write profiles, consume Receiver issuance grants, or change connections or presence, regardless of cache age. A later successful QQ login updates the shared cache; profile-provider failures preserve known values. " +
			"Failures report 401 credential_missing, credential_invalid (including revoked or superseded R and wrong scopes), or credential_expired, 409 device_registration_revoked for a permanently revoked Sender, and 503 service_unavailable. Responses are never cacheable.",
		Security: []map[string][]string{{"bearerAuth": {}}},
		Errors:   []int{http.StatusUnauthorized, http.StatusConflict, http.StatusServiceUnavailable},
	}, func(ctx context.Context, _ *struct{}) (*accountOutput, error) {
		profile, err := service.OwnAccount(ctx, observability.CredentialFrom(ctx))
		if err != nil {
			return nil, err
		}
		return &accountOutput{Body: AccountProfile{
			AccountID: profile.AccountID, Nickname: profile.Nickname, AvatarURL: profile.AvatarURL,
		}}, nil
	})
}
