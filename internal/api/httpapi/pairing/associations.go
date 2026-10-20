package pairing

import (
	"context"
	"net/http"
	"time"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
)

// ReceiverPresence reports the current published liveness of one Receiver and
// when that state last changed, which is the presence a Sender's own Receiver
// list states. Presence belongs to the running process, so the HTTP surface
// reads it here instead of through the pairing domain.
type ReceiverPresence interface {
	Reading(receiverID string) (online bool, stateChangedAt time.Time)
}

// ReceiverList is the accepted own-Receiver-list payload. It carries only the
// authenticated Sender's own active associations.
type ReceiverList struct {
	Items []protocol.ReceiverAssociation `json:"items" nullable:"false" doc:"The Sender's own active associations; empty when it has none"`
}

// PairingAlias is the accepted PATCH /v1/self/pairings/{id} body: the owning
// Sender's own alias for one association. An empty value clears it.
type PairingAlias struct {
	Alias string `json:"alias" required:"true" maxLength:"256" doc:"Replacement alias for the Sender's own association; an empty value clears it"`
}

type receiversOutput struct {
	Body ReceiverList
}

type renamePairingInput struct {
	ID   string `path:"id" maxLength:"128" doc:"Pairing ID of the Sender's own association"`
	Body PairingAlias
}

type renamePairingOutput struct {
	Body protocol.ReceiverAssociation
}

type removePairingInput struct {
	ID string `path:"id" maxLength:"128" doc:"Pairing ID of the Sender's own association"`
}

// registerAssociations installs the operations one Sender uses on its own
// Receiver associations. Every one of them derives the Sender from
// authentication and answers for that Sender's own active associations only.
func registerAssociations(api huma.API, handlers Handlers) {
	registerListReceivers(api, handlers)
	registerRenamePairing(api, handlers)
	registerRemovePairing(api, handlers)
}

// registerListReceivers installs GET /v1/self/receivers.
func registerListReceivers(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID: "GetSelfReceivers",
		Tags:        []string{TagPairing},
		Method:      http.MethodGet,
		Path:        "/v1/self/receivers",
		Summary:     "List the Sender's own Receiver associations",
		Description: "Returns every active association the authenticated Sender owns: the Receiver it forwards to, the Pairing ID both clients stored the Pairing key under, the Sender's own alias, and the Receiver's current presence with the time that state last changed. " +
			"Only this Sender's own associations appear: another Sender sharing the same Receiver is invisible here, and a pending reservation appears only as its Pairing Session. " +
			"Management authority is required, so an upload, Receiver or restricted pairing credential authorizes nothing here. " +
			"Failures report 401 for an absent or unusable management credential, or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, _ *struct{}) (*receiversOutput, error) {
		senderID, err := senderOf(ctx, handlers)
		if err != nil {
			return nil, err
		}
		associations, err := handlers.Sessions.Associations(ctx, senderID)
		if err != nil {
			return nil, err
		}

		output := &receiversOutput{}
		output.Body.Items = make([]protocol.ReceiverAssociation, 0, len(associations))
		for _, association := range associations {
			output.Body.Items = append(output.Body.Items, wireAssociation(handlers.Presence, association))
		}
		return output, nil
	})
}

// registerRenamePairing installs PATCH /v1/self/pairings/{id}: the owning
// Sender's own alias changes, and nothing else about the association does.
func registerRenamePairing(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID: "PatchSelfPairing",
		Tags:        []string{TagPairing},
		Method:      http.MethodPatch,
		Path:        "/v1/self/pairings/{id}",
		Summary:     "Rename the Sender's own association alias",
		Description: "Replaces the authenticated Sender's own alias for one of its active associations and returns that association as it now is, with the Receiver's current presence. An empty alias clears the Sender's own label. " +
			"The Pairing identity, its Receiver, the Receiver's delivery connection and every other Sender's association are untouched. " +
			"A Pairing that is unknown, owned by another Sender, still pending or already removed reports 404 resource_not_found exactly as one that does not exist, so no caller can confirm another Sender's association. " +
			"Failures report 401 for an absent or unusable management credential, 404 resource_not_found, 422 validation_failed for a body without an alias within its length, or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusNotFound,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, input *renamePairingInput) (*renamePairingOutput, error) {
		senderID, err := senderOf(ctx, handlers)
		if err != nil {
			return nil, err
		}
		association, err := handlers.Sessions.RenameAssociation(ctx, senderID, input.ID, input.Body.Alias)
		if err != nil {
			return nil, err
		}

		output := &renamePairingOutput{}
		output.Body = wireAssociation(handlers.Presence, association)
		return output, nil
	})
}

// registerRemovePairing installs DELETE /v1/self/pairings/{id}: the Sender
// withdraws its own authorization, and every shared resource survives it.
func registerRemovePairing(api huma.API, handlers Handlers) {
	huma.Register(api, huma.Operation{
		OperationID: "DeleteSelfPairing",
		Tags:        []string{TagPairing},
		Method:      http.MethodDelete,
		Path:        "/v1/self/pairings/{id}",
		Summary:     "Remove the Sender's own association",
		Description: "Removes this Sender's own authorization to forward to a Receiver and answers 204. The Receiver credential, its delivery connection and every other Sender's association survive, and the removed association keeps its durable identity. " +
			"The Sender's unfinished association work for the same Receiver is canceled with it, so a later key-confirmation report cannot recreate the association those clients removed; a connected Receiver is told with pairing_removed on its delivery connection, which stays open. " +
			"A Pairing that is unknown, owned by another Sender, still pending or already removed reports 404 resource_not_found exactly as one that does not exist. " +
			"Failures report 401 for an absent or unusable management credential, 404 resource_not_found, or 503 service_unavailable.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusNotFound,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, input *removePairingInput) (*struct{}, error) {
		senderID, err := senderOf(ctx, handlers)
		if err != nil {
			return nil, err
		}
		if err := handlers.Sessions.RemoveAssociation(ctx, senderID, input.ID, time.Now().UTC()); err != nil {
			return nil, err
		}
		return nil, nil
	})
}

// wireAssociation maps one domain association to the published view, adding
// the Receiver's current published presence.
func wireAssociation(presence ReceiverPresence, association domain.Association) protocol.ReceiverAssociation {
	online, stateChangedAt := presence.Reading(association.ReceiverID)
	view := protocol.ReceiverAssociation{
		ReceiverID:     association.ReceiverID,
		PairingID:      association.PairingID,
		Alias:          association.Alias,
		Presence:       protocol.PresenceOffline,
		StateChangedAt: stateChangedAt,
	}
	if online {
		view.Presence = protocol.PresenceOnline
	}
	return view
}
