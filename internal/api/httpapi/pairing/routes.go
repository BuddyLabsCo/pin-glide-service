package pairing

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	domain "github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
)

// Handlers are the HTTP operations of the pairing domain.
type Handlers struct {
	Sessions *domain.Service
	// Senders resolves a Sender management credential, so /authorize can fix
	// the claiming Sender before the domain decides capacity.
	Senders SenderCredentials
}

// SenderCredentials resolves one Sender management credential.
type SenderCredentials interface {
	SenderOf(ctx context.Context, token string) (string, error)
}

// Register installs the Pairing Session operations.
func Register(api huma.API, handlers Handlers) {
	registerCreate(api, handlers)
	registerAuthorize(api, handlers)
	registerCancel(api, handlers)
}

// registerCreate installs POST /v1/pairing-sessions. The credential depends on
// the mode: new_receiver is deliberately anonymous, and recover_receiver
// requires fresh owner proof in the body.
func registerCreate(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID:   "PostPairingSessions",
		Tags:          []string{TagPairing},
		Method:        http.MethodPost,
		Path:          "/v1/pairing-sessions",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a restricted Pairing Session",
		Description: "Creates one mode-fixed session that lives five minutes and hands its restricted credential only to the Receiver-side peer. " +
			"new_receiver allocates a Receiver and takes no credential; recover_receiver acts on an existing Receiver of the Account the fresh wechatCode proves and takes no credential either. " +
			"Associating one more Sender with an already credentialed Receiver needs that Receiver's own credential, so add_sender arrives with Receiver credential issuance rather than with this release. " +
			"Failures report 401 wechat_authorization_invalid, 403 fresh_identity_required, 404 resource_not_found for a Receiver the caller cannot see, 422 validation_failed for a mode and target that do not match, 502 identity_provider_error or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusForbidden,
			http.StatusNotFound,
			http.StatusUnprocessableEntity,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{},
	}, func(ctx context.Context, input *createInput) (*createOutput, error) {
		session, token, err := handlers.Sessions.Create(ctx, domain.CreateRequest{
			Mode:         domain.Mode(input.Body.Mode),
			ReceiverID:   input.Body.ReceiverID,
			RecoveryCode: input.Body.WeChatCode,
		}, time.Now().UTC())
		if err != nil {
			return nil, err
		}

		output := &createOutput{}
		output.Body = CreatedSession{
			Session:               wireView(session, time.Now().UTC()),
			PairingToken:          token,
			PairingTokenExpiresAt: session.ExpiresAt,
		}
		return output, nil
	})
}

// registerAuthorize installs POST /v1/pairing-sessions/{id}/authorize. The
// claiming Sender is fixed by the first successful claim, so a repeated call
// by the same Sender is idempotent and another Sender is refused.
func registerAuthorize(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID: "PostPairingSessionAuthorize",
		Tags:        []string{TagPairing},
		Method:      http.MethodPost,
		Path:        "/v1/pairing-sessions/{id}/authorize",
		Summary:     "Claim a Pairing Session for one Sender",
		Description: "Fixes the claiming Sender for a session and reserves binding capacity for a new distinct Receiver. " +
			"A repeated claim by the same Sender returns the same session; another Sender receives 409 pairing_session_claimed. " +
			"recover_receiver additionally requires fresh WeChat owner proof. " +
			"Failures report 401 for an unusable management credential, 403 fresh_identity_required, 404 resource_not_found, 409 receiver_limit_reached or pairing_session_claimed, 410 pairing_session_expired or pairing_session_closed, or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusForbidden,
			http.StatusNotFound,
			http.StatusConflict,
			http.StatusGone,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, input *authorizeInput) (*sessionOutput, error) {
		senderID, err := senderOf(ctx, handlers)
		if err != nil {
			return nil, err
		}

		session, err := handlers.Sessions.Authorize(ctx, domain.AuthorizeRequest{
			SessionID:    input.ID,
			SenderID:     senderID,
			Alias:        input.Body.Alias,
			RecoveryCode: input.Body.WeChatCode,
		}, time.Now().UTC())
		if err != nil {
			return nil, err
		}

		output := &sessionOutput{}
		output.Body = wireView(session, time.Now().UTC())
		return output, nil
	})
}

// registerCancel installs DELETE /v1/pairing-sessions/{id}. Cancellation
// releases the reservation only while the session may still continue.
func registerCancel(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID: "DeletePairingSession",
		Tags:        []string{TagPairing},
		Method:      http.MethodDelete,
		Path:        "/v1/pairing-sessions/{id}",
		Summary:     "Cancel an unfinished Pairing Session",
		Description: "Cancels a session before activation, releases its reservation and closes its restricted channel. " +
			"An already activated association is not undone: the call reports 410 pairing_session_closed and ordinary Pairing removal applies. " +
			"Failures report 401 for an unusable credential, 404 resource_not_found, 410 pairing_session_expired or pairing_session_closed, or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusNotFound,
			http.StatusGone,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, input *cancelInput) (*struct{}, error) {
		senderID, token, err := cancelParticipant(ctx, input.ID, handlers)
		if err != nil {
			return nil, err
		}
		if err := handlers.Sessions.Cancel(ctx, input.ID, senderID, token, time.Now().UTC()); err != nil {
			return nil, err
		}
		return nil, nil
	})
}

// senderOf resolves the claiming Sender's management credential from the
// Authorization header of one claim.
func senderOf(ctx context.Context, handlers Handlers) (string, error) {
	token := bearerCredential(ctx)
	if token == "" {
		return "", domain.ErrCredentialMissing
	}
	return handlers.Senders.SenderOf(ctx, token)
}

// cancelParticipant reports the authenticated participant of one
// cancellation. The owning Sender uses management authority; the Receiver
// side uses the session's restricted credential.
func cancelParticipant(ctx context.Context, sessionID string, handlers Handlers) (string, string, error) {
	token := bearerCredential(ctx)
	if token == "" {
		return "", "", domain.ErrCredentialMissing
	}
	if senderID, err := handlers.Senders.SenderOf(ctx, token); err == nil {
		return senderID, "", nil
	} else if !credentialOutcome(err) {
		return "", "", err
	}
	if _, err := handlers.Sessions.Resolve(ctx, token, time.Now().UTC()); err != nil {
		return "", "", err
	}
	return "", token, nil
}

// credentialOutcome reports whether an authentication failure is one of the
// credential outcomes that a different credential source may still resolve.
func credentialOutcome(err error) bool {
	for _, outcome := range []error{
		domain.ErrCredentialMissing,
		domain.ErrCredentialInvalid,
		domain.ErrCredentialExpired,
	} {
		if errors.Is(err, outcome) {
			return true
		}
	}
	return false
}

// bearerCredential returns the bearer credential of the current request.
func bearerCredential(ctx context.Context) string {
	return observability.CredentialFrom(ctx)
}

// wireView maps the domain view to the shared wire shape.
func wireView(session domain.Session, now time.Time) protocol.PairingSessionView {
	view := session.View(now)
	return protocol.PairingSessionView{
		SessionID:         view.SessionID,
		Mode:              string(view.Mode),
		ReceiverID:        view.ReceiverID,
		State:             string(view.State),
		ExpiresAt:         view.ExpiresAt,
		SenderConfirmed:   view.SenderConfirmed,
		ReceiverConfirmed: view.ReceiverConfirmed,
		SenderID:          view.SenderID,
		PairingID:         view.PairingID,
	}
}
