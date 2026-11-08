package identity

import (
	"context"
	"net/http"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
)

// SenderList is the complete unrevoked Sender inventory of the verified Account.
type SenderList struct {
	Items []protocol.SenderRegistration `json:"items" nullable:"false" doc:"All unrevoked Senders of the authenticated Account, including self and logged-out registrations; newest creation first, then Sender ID ascending"`
}

type sendersOutput struct{ Body SenderList }

func registerListSenders(api huma.API, service *domain.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "GetSelfSenders",
		Tags:        []string{TagIdentity},
		Method:      http.MethodGet,
		Path:        "/v1/self/senders",
		Summary:     "List the Account's unrevoked Sender registrations",
		Description: "Returns the complete list for the Account resolved from the current Sender management credential M, including the calling Sender and ordinary-logged-out registrations. Permanently revoked registrations are excluded. No Account selector, paging, search or filter parameters are supported. Ordered by createdAt descending, then senderId ascending. Active means unrevoked, not online, logged in or forwarding. " +
			"U, R, F and restricted Pairing credentials do not authorize this operation. Failures report 401 for absent, unusable, wrongly scoped or expired M, 409 device_registration_revoked for a permanently closed acting Sender, and 503 service_unavailable for temporary storage failure. Listing makes no identity-provider call or credential, presence or revocation mutation. " +
			"After a successful reload for the same Account, a missing selected Sender confirms revocation; a remaining target can be retried through POST /v1/devices/{id}/revoke with M and fresh matching owner proof. A failed list request establishes neither outcome.",
		Errors:   []int{http.StatusUnauthorized, http.StatusConflict, http.StatusServiceUnavailable},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, _ *struct{}) (*sendersOutput, error) {
		senders, err := service.Senders(ctx, observability.CredentialFrom(ctx))
		if err != nil {
			return nil, err
		}
		output := &sendersOutput{Body: SenderList{Items: make([]protocol.SenderRegistration, 0, len(senders))}}
		for _, sender := range senders {
			output.Body.Items = append(output.Body.Items, protocol.SenderRegistration{SenderID: sender.ID, DeviceName: sender.DeviceName, State: "active", CreatedAt: sender.CreatedAt})
		}
		return output, nil
	})
}
