// Package delivery exposes the Delivery operations of the delivery domain
// over the service's HTTP surface.
package delivery

import (
	"encoding/json"
	"errors"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/delivery"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/danielgtaylor/huma/v2"
)

// TagDelivery groups the published Delivery operations.
const TagDelivery = "delivery"

// maxIDLength bounds the opaque identifiers a client sends, matching the
// published schema of every other operation.
const maxIDLength = 128

// Errors the upload body reports before any entry can be enumerated: the
// whole-request failures the shared HTTP error model maps to the problem code
// and status the published contract states.
var (
	// ErrMalformedRequest marks a body that is not a JSON document.
	ErrMalformedRequest = errors.New("request body is not JSON")
	// ErrInvalidEnvelope marks a JSON body that is not this operation's
	// envelope, such as one without a readable messageId or items array.
	ErrInvalidEnvelope = errors.New("upload envelope is invalid")
)

// envelopeFailure is one whole-request failure of the upload body. Its typed
// cause selects the problem code the shared model reports, and its item is the
// location and message the problem carries, never the rejected values.
type envelopeFailure struct {
	cause error
	item  protocol.ErrorItem
}

// Error satisfies the error interface for the whole-request failure.
func (f *envelopeFailure) Error() string { return f.cause.Error() }

// Unwrap exposes the typed cause the shared error model maps.
func (f *envelopeFailure) Unwrap() error { return f.cause }

// ErrorDetail states where the failure was read, which is the validation
// location the published problem reports.
func (f *envelopeFailure) ErrorDetail() *huma.ErrorDetail {
	return &huma.ErrorDetail{Location: f.item.Location, Message: f.item.Message}
}

// uploadEnvelope is the first view of one upload body: the Message ID and the
// entries as they arrived, so an entry that cannot be read is still answered
// on its own index instead of failing the whole request.
type uploadEnvelope struct {
	MessageID json.RawMessage `json:"messageId"`
	Items     json.RawMessage `json:"items"`
}

// readUpload reads one upload body. A body that is not JSON is the malformed
// request the contract answers with 400; a JSON body that is not this envelope
// is a whole-request validation failure; and only then are the entries read
// individually, so a malformed entry cannot suppress a valid one.
func readUpload(body []byte) (domain.Request, error) {
	if !json.Valid(body) {
		return domain.Request{}, malformed()
	}

	var envelope uploadEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return domain.Request{}, invalid(&protocol.ErrorItem{
			Location: "body",
			Message:  "The body is not an upload request object.",
		})
	}

	var messageID string
	if err := json.Unmarshal(envelope.MessageID, &messageID); err != nil || messageID == "" || len(messageID) > maxIDLength {
		return domain.Request{}, invalid(&protocol.ErrorItem{
			Location: "body.messageId",
			Message:  "messageId must be a non-empty string of at most 128 characters.",
		})
	}
	// The entries are read as an array of raw entries, so an entry that is not
	// an object of this profile is still answered on its own index.
	entries := []json.RawMessage{}
	if err := json.Unmarshal(envelope.Items, &entries); err != nil || entries == nil {
		return domain.Request{}, invalid(&protocol.ErrorItem{
			Location: "body.items",
			Message:  "items must be an array of entries.",
		})
	}

	request := domain.Request{MessageID: messageID, Items: make([]domain.Item, 0, len(entries))}
	for _, entry := range entries {
		request.Items = append(request.Items, readItem(entry))
	}
	return request, nil
}

// readItem reads one entry as far as it is readable. A field the entry did not
// carry as its expected type stays unread, and the entry's own validation
// decides its outcome, so the service answers one failed result for that index
// instead of ending the batch.
func readItem(entry json.RawMessage) domain.Item {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(entry, &fields); err != nil {
		// The entry is not an object, so nothing in it is readable.
		return domain.Item{}
	}

	item := domain.Item{
		ReceiverID: readString(fields["receiverId"]),
		PairingID:  readString(fields["pairingId"]),
		Nonce:      readString(fields["nonce"]),
		Ciphertext: readString(fields["ciphertext"]),
	}
	if version := readVersion(fields["cryptoVersion"]); version != nil {
		item.CryptoVersion = version
	}
	return item
}

// readString reads one field as its expected string type, reporting an absent,
// unreadable or null value as the empty string the entry's validation rejects.
func readString(field json.RawMessage) string {
	var value string
	if err := json.Unmarshal(field, &value); err != nil {
		return ""
	}
	return value
}

// readVersion reads one cryptoVersion field as the integer it must be. A
// missing, null or non-integer value is not a readable version.
func readVersion(field json.RawMessage) *int {
	var version *int
	if err := json.Unmarshal(field, &version); err != nil {
		return nil
	}
	return version
}

// malformed builds the whole-request failure of a body that is not a JSON
// document.
func malformed() error {
	return &envelopeFailure{
		cause: ErrMalformedRequest,
		item: protocol.ErrorItem{
			Location: "body",
			Message:  "The request body is not a JSON document.",
		},
	}
}

// invalid builds a whole-request validation failure from one item.
func invalid(item *protocol.ErrorItem) error {
	return &envelopeFailure{cause: ErrInvalidEnvelope, item: *item}
}

// wireResults maps the domain results to the published 207 payload, adding the
// shared business error of every failed entry.
func wireResults(messageID, requestID string, results []domain.Result) protocol.UploadResponse {
	response := protocol.UploadResponse{
		MessageID: messageID,
		RequestID: requestID,
		Results:   make([]protocol.UploadResult, 0, len(results)),
	}
	for _, result := range results {
		entry := protocol.UploadResult{
			Index:      result.Index,
			ReceiverID: result.ReceiverID,
			Status:     result.Status,
		}
		if result.Err != nil {
			entry.Error = businessErrorOf(result.Err)
		}
		response.Results = append(response.Results, entry)
	}
	return response
}

// wireDeliveries maps the retained metadata to the published view, adding the
// shared business error of every terminal failure.
func wireDeliveries(messageID string, deliveries []domain.Delivery) protocol.MessageDeliveries {
	view := protocol.MessageDeliveries{
		MessageID:  messageID,
		Deliveries: make([]protocol.DeliveryView, 0, len(deliveries)),
	}
	for _, delivery := range deliveries {
		entry := protocol.DeliveryView{
			ReceiverID:  delivery.ReceiverID,
			PairingID:   delivery.PairingID,
			Status:      delivery.Status,
			CreatedAt:   delivery.CreatedAt,
			CompletedAt: delivery.CompletedAt,
		}
		if err := domain.TerminalError(delivery.Status); err != nil {
			entry.Error = businessErrorOf(err)
		}
		view.Deliveries = append(view.Deliveries, entry)
	}
	return view
}

// businessErrorOf maps one delivery failure to the shared BusinessError.
func businessErrorOf(err error) *protocol.BusinessError {
	for _, outcome := range deliveryOutcomes {
		if errors.Is(err, outcome.cause) {
			return &protocol.BusinessError{Code: outcome.code, Detail: outcome.detail}
		}
	}
	// An outcome this table does not carry is a programming error, so it is
	// reported as a generic internal failure rather than with its own text.
	return &protocol.BusinessError{
		Code:   protocol.InternalError,
		Detail: "The service encountered an unexpected error.",
	}
}

// deliveryOutcomes lists every entry outcome the client can receive, with the
// stable code and safe detail the published contract states for it. The
// details never name another Account's registration, so an unknown, foreign,
// unpaired and revoked target read the same.
var deliveryOutcomes = []struct {
	cause  error
	code   protocol.ErrorCode
	detail string
}{
	{domain.ErrDuplicateReceiver, protocol.DuplicateReceiver, "Another entry of this request named the same Receiver, so no ciphertext was chosen for it."},
	{domain.ErrInvalidItem, protocol.InvalidDeliveryItem, "The entry is not a well-formed ciphertext envelope of this profile."},
	{domain.ErrUnsupportedCryptoVersion, protocol.UnsupportedCryptoVersion, "The entry names a cryptoVersion this service does not implement."},
	{domain.ErrReceiverNotAuthorized, protocol.ReceiverNotAuthorized, "Receiver is not eligible for this Sender."},
	{domain.ErrOffline, protocol.Offline, "The Receiver holds no usable current delivery connection."},
	{domain.ErrSendFailed, protocol.SendFailed, "The delivery write failed or exceeded its bound, so arrival is not guaranteed absent."},
	{domain.ErrAckTimeout, protocol.AckTimeout, "The Receiver did not report arrival within the adopted bound."},
	{domain.ErrInterrupted, protocol.Interrupted, "The registered work ended without a confirmable outcome."},
}
