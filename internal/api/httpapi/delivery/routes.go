package delivery

import (
	"context"
	"net/http"
	"reflect"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/delivery"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
)

// Register installs the Delivery operations. A failed operation returns the
// domain's typed outcome; the shared HTTP error model maps it to the stable
// business code the published contract defines.
func Register(api huma.API, service *domain.Service) {
	registerUpload(api, service)
	registerMessage(api, service)
}

// uploadInput is the raw body the upload operation reads. Huma hands the bytes
// over unparsed because the envelope is read entry by entry: one entry the
// service cannot read must answer its own indexed result instead of failing
// the whole request, which Huma's body validation cannot express.
type uploadInput struct {
	RawBody []byte `contentType:"application/json" doc:"Upload envelope with one entry per selected Receiver"`
}

type uploadOutput struct {
	Body protocol.UploadResponse
}

type messageInput struct {
	MessageID string `path:"messageId" maxLength:"128" doc:"Message ID of one of the Sender's own uploads"`
}

type messageOutput struct {
	Body protocol.MessageDeliveries
}

// registerUpload installs POST /v1/messages.
func registerUpload(api huma.API, service *domain.Service) {
	const path = "/v1/messages"
	huma.Register(api, huma.Operation{
		OperationID:   "PostMessages",
		Tags:          []string{TagDelivery},
		Method:        http.MethodPost,
		Path:          path,
		DefaultStatus: http.StatusMultiStatus,
		Summary:       "Upload indexed ciphertexts for live Delivery",
		Description: "Admits one Sender's Message and forwards each entry to the Receiver it selected, answering 207 with one terminal result per input entry and its original index. " +
			"Upload authority is required, so an absent credential reports 401 credential_missing; an unknown or wrongly scoped credential, and one an earlier logout, fresh login or family-reuse revocation invalidated, reports 401 credential_invalid; an expired one reports 401 credential_expired; and one whose Sender registration an owner permanently closed reports 401 credential_revoked. The Sender identity comes from that credential, never from the payload. " +
			"The envelope is the whole-request contract: a body that is not JSON reports 400 invalid_request, a missing request body reports 400 invalid_request, and a JSON body without a readable messageId or without an items array reports 422 validation_failed. " +
			"Once entries can be enumerated, each entry is validated on its own index, so a malformed entry never suppresses a valid one. Two entries naming the same Receiver are both rejected with duplicate_receiver, because the service never chooses between different ciphertexts for one Delivery. " +
			"Each entry is validated and then authorized before its dedup lookup: another readable cryptoVersion reports unsupported_crypto_version; a missing field, unpadded or non-Base64 value, a nonce that is not 96 bits or a ciphertext that cannot hold its 128-bit tag reports invalid_delivery_item; and a target that is unknown to this Sender, not its own active Pairing or revoked reports receiver_not_authorized without confirming whether it exists. " +
			"An entry whose Receiver holds no current delivery connection reports offline, a write that failed or exceeded its bound reports send_failed, and a Receiver that did not report arrival within the bound after a successful write reports ack_timeout; arrival is not guaranteed absent in either of those two. " +
			"The retained identity is (Sender, messageId, Receiver): the winning claim sends once while a concurrent duplicate of the same identity joins that live work or reads its retained terminal result without sending again, and later submissions validate their own authority before that lookup and never rewrite an earlier Delivery history. An identity a caller re-submits after a restart, or one whose registered work this process lost, reports interrupted rather than resending ciphertext. " +
			"A Receipt means arrival only: it is reported once the receiver's minimal correlation is readable, before the Receiver validates, decrypts or displays anything. Late or duplicated Receipts never change a terminal result. " +
			"The service validates the accepted encryption profile without decrypting content, keeps ciphertext only in memory for the attempt, and never stores or logs a ciphertext, a key or a plaintext. It performs no retry, no offline queue and no history replay. " +
			"A 207 result carrying offline, rejected or interrupted means this invocation did not forward that entry, and does not disprove an earlier Delivery of the same Message. " +
			"When a reliable terminal result cannot be obtained, the whole request reports 503 delivery_status_unavailable instead of a pending success, because delivery may already have occurred. " +
			"The 207 response uses application/json and carries requestId once at the top level.",
		Errors: []int{
			http.StatusBadRequest,
			http.StatusUnauthorized,
			http.StatusUnprocessableEntity,
			http.StatusServiceUnavailable,
		},
		Security:         []map[string][]string{{"bearerAuth": {}}},
		SkipValidateBody: true,
	}, func(ctx context.Context, input *uploadInput) (*uploadOutput, error) {
		// A body failure is a typed outcome of this surface, so the shared
		// error model reports it with the problem code and request identifier
		// every other failure of the operation carries.
		request, err := readUpload(input.RawBody)
		if err != nil {
			return nil, err
		}

		results, err := service.Upload(ctx, observability.CredentialFrom(ctx), request)
		if err != nil {
			return nil, err
		}

		output := &uploadOutput{}
		output.Body = wireResults(request.MessageID, observability.RequestIDFrom(ctx), results)
		return output, nil
	})

	describeUploadRequest(api, path)
}

// describeUploadRequest states the published request body of the upload
// operation from the shared wire schema. The operation reads its body itself,
// so the derived description of a raw body would understate what clients send.
func describeUploadRequest(api huma.API, path string) {
	document := api.OpenAPI()
	operation := document.Paths[path].Post
	if operation == nil || operation.RequestBody == nil {
		panic("the upload operation must be registered before its request body is described")
	}
	operation.RequestBody.Content["application/json"].Schema =
		document.Components.Schemas.Schema(reflect.TypeOf(protocol.UploadRequest{}), true, "")
}

// registerMessage installs GET /v1/messages/{messageId}.
func registerMessage(api huma.API, service *domain.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "GetMessage",
		Tags:        []string{TagDelivery},
		Method:      http.MethodGet,
		Path:        "/v1/messages/{messageId}",
		Summary:     "Read the retained metadata of one own Message",
		Description: "Returns the retained metadata of the authenticated Sender's own admitted Deliveries of one Message: each Receiver and Pairing, the status, when the Delivery was admitted and, for a terminal failure, when it finished and why. " +
			"Upload authority is required, and only this Sender's own Deliveries appear: another Sender's or another Account's Message is invisible here. The query grants no forwarding authority and does not require the Pairing to stay active, so it never overrides upload validation. " +
			"Processing appears only here, never in a synchronous 207 result; an upload that admitted no Delivery, including one whose entries were all rejected, is not queryable, and a Message whose rows scheduled cleanup already deleted is reported the same way. " +
			"Registered work whose owner is gone is resolved as interrupted rather than left pending, and it is never resumed or re-sent. " +
			"Failures report 401 for an absent or unusable upload credential, 404 resource_not_found for a Message with no retained admitted Delivery, or 503 delivery_status_unavailable when the retained metadata cannot be read reliably.",
		Errors: []int{
			http.StatusUnauthorized,
			http.StatusNotFound,
			http.StatusServiceUnavailable,
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}, func(ctx context.Context, input *messageInput) (*messageOutput, error) {
		deliveries, err := service.Deliveries(ctx, observability.CredentialFrom(ctx), input.MessageID)
		if err != nil {
			return nil, err
		}

		output := &messageOutput{}
		output.Body = wireDeliveries(input.MessageID, deliveries)
		return output, nil
	})
}
