package identity

import (
	"context"
	"fmt"
	"net/http"
	"time"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/danielgtaylor/huma/v2"
)

// TagIdentity groups the published identity operations.
const TagIdentity = "identity"

// Register installs the Sender authentication operations. A failed operation
// returns the domain's typed outcome; the shared HTTP error model maps it to
// the stable business code the published contract defines.
func Register(api huma.API, service *domain.Service, devices DeviceRevocation) {
	registerWeChatLogin(api, service)
	registerQQLogin(api, service)
	registerRefresh(api, service)
	registerLogout(api, service, devices)
	registerRevokeDevice(api, devices)
}

// DeviceRevocation is the two identity operations that change a device
// registration rather than only its credentials: an Account switch, which the
// logout operation requests, and the Account owner's security revocation of one
// Sender or Receiver registration.
type DeviceRevocation interface {
	SwitchAccount(ctx context.Context, managementCredential string, now time.Time) error
	DeviceRevocation(ctx context.Context, managementCredential, deviceID string, proof domain.Proof, now time.Time) error
}

// registerWeChatLogin serves the fresh WeChat login that creates or restores one
// Sender installation.
func registerWeChatLogin(api huma.API, service *domain.Service) {
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
			Proof:    domain.Proof{Provider: domain.ProviderWeChat, Code: input.Body.WeChatCode},
			SenderID: input.Body.SenderID,
		})
		if err != nil {
			return nil, err
		}

		output := &loginOutput{}
		output.Body = credentialBundle(bundle)
		return output, nil
	})
}

// registerQQLogin serves the fresh QQ login through the same shared Account and
// Sender workflow as WeChat.
func registerQQLogin(api huma.API, service *domain.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "PostAuthQQ",
		Tags:        []string{TagIdentity},
		Method:      http.MethodPost,
		Path:        "/v1/auth/qq",
		Summary:     "Verify QQ login and issue Sender credentials",
		Description: "Verifies a fresh QQ authorization code through the mobile server-side flow, maps the provider-verified OpenID to its QQ Account and creates or restores this installation's Sender registration, replacing that Sender's previous credential family. " +
			"Omitting senderId registers a new Sender; supplying one restores only an owned active registration. " +
			"Failures report 401 qq_authorization_invalid, 404 resource_not_found for an unknown or foreign senderId, 409 device_registration_revoked for a permanently revoked one, 502 identity_provider_error or 503 service_unavailable. " +
			"Raw M, U and F credentials are returned only to this holder and the response is never cacheable.",
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusNotFound,
			http.StatusConflict,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{},
	}, func(ctx context.Context, input *qqLoginInput) (*loginOutput, error) {
		bundle, err := service.Login(ctx, domain.LoginRequest{
			Proof:    domain.Proof{Provider: domain.ProviderQQ, Code: input.Body.QQCode},
			SenderID: input.Body.SenderID,
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

// registerLogout serves the two ways one Sender ends its own service access:
// ordinary logout, which revokes its credential family and preserves its
// Pairings, and the Account switch, which also removes its own associations so
// the next Account cannot inherit them. Its description states what each mode
// leaves behind and the client's obligation not to claim a sign-out the service
// never confirmed.
func registerLogout(api huma.API, service *domain.Service, devices DeviceRevocation) {
	huma.Register(api, huma.Operation{
		OperationID: "PostAuthLogout",
		Tags:        []string{TagIdentity},
		Method:      http.MethodPost,
		Path:        "/v1/auth/logout",
		Summary:     "Revoke the Sender's service access",
		Description: "Revokes the current Sender's credential family, which is both access scopes and the renewal credential, without permanently closing its Device Registration; other installations and a family a fresh login replaced keep working. " +
			"The default logout mode preserves the Sender's Pairings, and switch_account additionally removes only that Sender's own associations and cancels its unfinished reserved Pairing work in the same commit, because the next Account must not inherit them. Switching removes associations, so only the management credential may request it, and the client clears its local identity and pairing keys itself before requesting a new registration. " +
			"The Sender's own authority for this operation is exactly one source: its management or upload credential in the Authorization header, or its renewal credential in the refreshToken body field. " +
			"Renewing access first is never required, so a renewal credential authorizes logout while the access credentials have expired. " +
			"An ordinary logout request offering both sources is rejected as 422 validation_failed, as was one without a usable source: an absent credential reports 401 credential_missing, an unknown or wrongly scoped one 401 credential_invalid, an expired one 401 credential_expired, and a credential an earlier logout or a fresh login revoked 401 credential_revoked. A switch_account request is decided by the management credential in its Authorization header alone, so a renewal credential in the body adds nothing to it, and one without that header credential reports 401 credential_missing, an unknown, wrongly scoped or already replaced one 401 credential_invalid, an expired one 401 credential_expired, and one whose registration an owner permanently closed 401 credential_revoked. " +
			"Ending this Sender's access never reaches back into work already admitted: a live delivery connection and a Delivery the service already accepted keep the authorization they were admitted with until they end, while every new request validates the current credentials again. " +
			"A request that never reached the service revokes nothing, and the client stops forwarding locally at once without claiming remote sign-out it did not receive. " +
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
		accessToken := observability.CredentialFrom(ctx)
		mode := ""
		if input.Body != nil {
			mode = input.Body.Mode
		}
		// Switching Accounts removes this Sender's own associations together
		// with its family, so it is not an ordinary logout that happens to
		// touch more: the two are separate operations behind one route.
		if mode == "switch_account" {
			if err := devices.SwitchAccount(ctx, accessToken, time.Now().UTC()); err != nil {
				return nil, err
			}
			return nil, nil
		}

		request := domain.LogoutRequest{AccessToken: accessToken}
		if input.Body != nil {
			request.RefreshToken = input.Body.RefreshToken
		}
		if err := service.Logout(ctx, request); err != nil {
			return nil, err
		}
		return nil, nil
	})
}

// registerRevokeDevice installs POST /v1/devices/{id}/revoke: the Account
// owner's permanent security revocation of one of its own registrations,
// authorized by fresh identity proof of the same Account. The named registration
// decides which role's lifecycle the revocation applies.
func registerRevokeDevice(api huma.API, devices DeviceRevocation) {
	huma.Register(api, huma.Operation{
		OperationID:   "PostDeviceRevoke",
		Tags:          []string{TagIdentity},
		Method:        http.MethodPost,
		Path:          "/v1/devices/{id}/revoke",
		Summary:       "Permanently revoke one owned Sender or Receiver registration",
		DefaultStatus: http.StatusNoContent,
		Description: "Permanently closes one Sender or Receiver registration that belongs to the Account the fresh WeChat or QQ proof verifies, and answers 204. Exactly one of wechatCode or qqCode must be present; sending both is rejected. Every credential of the named device is invalidated, so nothing it still holds authorizes anything afterwards. " +
			"For a Sender registration, its unfinished reserved Pairing work is canceled and only its own associations are removed, in one commit: the Receiver's credential, its delivery connection and every other Sender's association survive, including with a Receiver the revoked Sender shared, and a later login presenting the closed registration ID is refused with 409 device_registration_revoked for as long as the registration is retained. " +
			"For a Receiver registration, its credential generation, the issuance and recovery grants it could still redeem, its delivery connection and every Pairing that forwards to it are invalidated in the same commit: a committed delivery connection receives device_revoked and then closes with 1008 policy_violation, a handshake still committing is refused before its upgrade, that Receiver's associations are removed for every Sender that owned one, and every other Receiver keeps its registration, its credential, its Pairings and its connection, including a Receiver the same Account owns. " +
			"Revoking a registration never reaches back into work already admitted: a Delivery the service already accepted keeps the authorization it was admitted with, a revoked Sender's Receiver keeps the delivery connection it already held, and every new request validates the current credentials again. A revoked Receiver's own retained history, and the Delivery records of the Pairings it lost, are not deleted by the revocation. " +
			"Ordinary logout and refresh-family reuse revocation never close a registration, so this is the only permanent outcome. " +
			"The caller's own management credential authorizes the request, so only the Account that owns the registration can revoke it. " +
			"Failures report 401 credential_missing for an absent management credential, 401 credential_invalid for an unknown or wrongly scoped one, 401 credential_expired for an expired one, 401 credential_revoked for one whose own registration an owner permanently closed, 401 wechat_authorization_invalid or qq_authorization_invalid for proof the provider refuses, 403 fresh_identity_required for proof of another Account, 404 resource_not_found for an unknown or foreign registration, 409 device_registration_revoked for an owned registration that is already permanently closed, 422 validation_failed for a body carrying no usable or conflicting fresh proof, 502 identity_provider_error, or 503 service_unavailable. " +
			"This response carries no credential and is never cacheable.",
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusForbidden,
			http.StatusNotFound,
			http.StatusConflict,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, input *revokeDeviceInput) (*struct{}, error) {
		proof, err := domain.SelectProof(input.Body.WeChatCode, input.Body.QQCode)
		if err != nil {
			return nil, err
		}
		if proof.Provider == "" {
			return nil, domain.ErrIdentityProofRequired
		}
		if err := devices.DeviceRevocation(ctx, observability.CredentialFrom(ctx), input.ID,
			proof, time.Now().UTC()); err != nil {
			return nil, err
		}
		return nil, nil
	})
}
