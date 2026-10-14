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
	registerComplete(api, handlers)
	registerRedeem(api, handlers)
	registerCancel(api, handlers)
}

// registerCreate installs POST /v1/pairing-sessions. The Receiver side creates
// the context and holds the restricted token; a new or recovering Receiver is
// anonymous, while attaching one more Sender to a credentialed Receiver is
// authorized by the credential that Receiver already holds.
func registerCreate(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID:   "PostPairingSessions",
		Tags:          []string{TagPairing},
		Method:        http.MethodPost,
		Path:          "/v1/pairing-sessions",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a restricted Pairing Session",
		Description: "Creates one mode-fixed session that lives five minutes and hands its restricted credential only to the Receiver-side peer. " +
			"new_receiver allocates a Receiver. add_sender attaches one more Sender to the Receiver named by receiverId and authenticates with that Receiver's own current credential in the Authorization header; presenting it proves possession and consumes any issuance grant still outstanding, and it authorizes no other Receiver. recover_receiver acts on an existing Receiver and takes no credential at creation. " +
			"The Sender that approves the session supplies its own authority when it claims the session. " +
			"Failures report 401 credential_missing or credential_invalid for add_sender, 404 resource_not_found for a Receiver the credential does not authenticate, 422 validation_failed for a mode and target that do not match, or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusNotFound,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
		// One operation serves two entry authorities: new_receiver and
		// recover_receiver are anonymous, add_sender requires the Receiver's
		// own credential. The empty requirement publishes the anonymous
		// alternative, and the description states what each mode accepts.
		Security: []map[string][]string{{}, {"bearerAuth": {}}},
	}, func(ctx context.Context, input *createInput) (*createOutput, error) {
		session, token, err := handlers.Sessions.Create(ctx, domain.CreateRequest{
			Mode:       domain.Mode(input.Body.Mode),
			ReceiverID: input.Body.ReceiverID,
			Token:      bearerCredential(ctx),
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

// registerComplete installs POST /v1/pairing-sessions/{id}/complete. Each
// participant reports with its own credential, and the service derives the
// reporting side from authentication rather than from a caller-supplied label.
func registerComplete(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID: "PostPairingSessionComplete",
		Tags:        []string{TagPairing},
		Method:      http.MethodPost,
		Path:        "/v1/pairing-sessions/{id}/complete",
		Summary:     "Report key confirmation for one Pairing Session side",
		Description: "Records one participant's authenticated report that it saved the derived Pairing key and verified its peer's key confirmation. The claiming Sender reports with its management credential; the Receiver side reports with the session's restricted credential. Both reports atomically activate the session's reservation: a new_receiver session then becomes credential_ready, while an add_sender session completes without issuing a Receiver credential. Repeating a successful side's report is idempotent. Receiver credential recovery reports through its own retained-key reconciliation instead. " +
			"A re-pairing commits its own Pairing ID and retires only the confirming Sender's association that this session replaced, and only while that association is still current; any other outcome fails with 409 operation_superseded and preserves the association that is current. " +
			"Failures report 401 for an unusable credential, 404 resource_not_found, 409 pairing_not_ready or operation_superseded, 410 pairing_session_expired or pairing_session_closed, 422 validation_failed, or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusNotFound,
			http.StatusConflict,
			http.StatusGone,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, input *completeInput) (*sessionOutput, error) {
		senderID, token, err := participantOf(ctx, handlers)
		if err != nil {
			return nil, err
		}
		session, err := handlers.Sessions.Complete(ctx, domain.CompleteRequest{
			SessionID:       input.ID,
			SenderID:        senderID,
			Token:           token,
			KeyConfirmation: domain.KeyConfirmation(input.Body.KeyConfirmation),
		}, time.Now().UTC())
		if err != nil {
			return nil, err
		}

		output := &sessionOutput{}
		output.Body = wireView(session, time.Now().UTC())
		return output, nil
	})
}

// registerRedeem installs POST /v1/pairing-sessions/{id}/receiver-token. Only
// the Receiver-side restricted credential may redeem, and only a session whose
// mode issues credentials.
func registerRedeem(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID: "PostPairingSessionReceiverToken",
		Tags:        []string{TagPairing},
		Method:      http.MethodPost,
		Path:        "/v1/pairing-sessions/{id}/receiver-token",
		Summary:     "Redeem the Pairing Session's Receiver credential",
		Description: "Redeems one session's credential grant for the Receiver side and returns the long-lived Receiver credential to that holder only, as a no-store response. The session's one issuance operation orders the request by operationId and attempt: an equal or lower attempt and any competing operation are superseded, while the one higher attempt may replace a credential no one has proved possession of yet, so a lost response costs at most one retransmission. Once the credential proves possession, redemption can no longer rotate it. An add_sender session returns mode_forbids_credential_issue without touching credentials. The credential is stored as a digest only and never reaches the Sender, the QR or a session view. " +
			"Failures report 401 for an unusable restricted credential, 409 operation_superseded, pairing_not_ready or mode_forbids_credential_issue, 410 pairing_session_closed, 422 validation_failed, or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusConflict,
			http.StatusGone,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, input *redeemInput) (*redeemOutput, error) {
		token := bearerCredential(ctx)
		if token == "" {
			return nil, domain.ErrCredentialMissing
		}
		issued, err := handlers.Sessions.Redeem(ctx, domain.RedeemRequest{
			SessionID:   input.ID,
			Token:       token,
			OperationID: input.Body.OperationID,
			Attempt:     input.Body.Attempt,
		}, time.Now().UTC())
		if err != nil {
			return nil, err
		}

		output := &redeemOutput{}
		output.Body = IssuedReceiverToken{ReceiverID: issued.ReceiverID, ReceiverToken: issued.Token}
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
		Description: "Cancels a session before activation and releases its reservation. " +
			"Both participants are told the session closed, and the restricted channel stops relaying: its next frame closes the connection. " +
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
		senderID, token, err := participantOf(ctx, handlers)
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

// participantOf reports the participant one session operation authenticates.
// The owning Sender uses management authority; the Receiver side uses the
// session's restricted credential. A caller that presents neither usable
// source receives the credential outcome of the one it tried.
func participantOf(ctx context.Context, handlers Handlers) (string, string, error) {
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
