//go:build integration

package delivery_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// message reads the retained metadata of one Message.
func (e *environment) message(t *testing.T, credential, messageID string) (int, map[string]deliveryView, []byte) {
	t.Helper()

	status, payload := e.request(t, http.MethodGet, "/v1/messages/"+messageID, "", credential)
	if status != http.StatusOK {
		return status, nil, payload
	}

	var metadata struct {
		MessageID  string         `json:"messageId"`
		Deliveries []deliveryView `json:"deliveries"`
	}
	if err := json.Unmarshal(payload, &metadata); err != nil {
		t.Fatalf("decode message metadata: %v: %s", err, payload)
	}
	if metadata.MessageID != messageID {
		t.Errorf("metadata reports message %q, want %q", metadata.MessageID, messageID)
	}

	views := map[string]deliveryView{}
	for _, view := range metadata.Deliveries {
		if _, duplicate := views[view.ReceiverID]; duplicate {
			t.Errorf("metadata repeats Receiver %s", view.ReceiverID)
		}
		views[view.ReceiverID] = view
	}
	return status, views, payload
}

// TestMessageMetadataReportsRetainedOutcomes shows the metadata query of one
// Sender's own Message: every admitted Delivery with its Pairing, status and
// terminal outcome, and a Message with nothing admitted reported as absent to
// every Sender.
func TestMessageMetadataReportsRetainedOutcomes(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-metadata", "openid-metadata")
	live, livePairing, liveToken := environment.paired(t, sender, "Live")
	offline, offlinePairing, _ := environment.paired(t, sender, "Offline")
	socket := receiverConnected(t, environment, liveToken)

	const messageID = "message-metadata"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID,
		entryOf(live, livePairing),
		entryOf(offline, offlinePairing),
	))
	frame := readDelivery(t, socket)
	receipt(t, socket, frame)
	if status, _, payload := awaitUpload(t, answered); status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}

	status, views, payload := environment.message(t, sender.Uploader.Token, messageID)
	if status != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", status, payload)
	}
	if len(views) != 2 {
		t.Fatalf("metadata reports %d Deliveries, want the 2 admitted ones: %s", len(views), payload)
	}

	arrived, ok := views[live]
	if !ok {
		t.Fatalf("metadata is missing the Delivery of %s: %s", live, payload)
	}
	if arrived.Status != "received" || arrived.Error != nil {
		t.Errorf("the confirmed Delivery = %+v, want received without error", arrived)
	}
	if arrived.PairingID != livePairing {
		t.Errorf("the confirmed Delivery names Pairing %q, want %q", arrived.PairingID, livePairing)
	}
	if arrived.CreatedAt.IsZero() || arrived.CompletedAt == nil {
		t.Errorf("the confirmed Delivery = %+v, want admission and completion times", arrived)
	}

	missed, ok := views[offline]
	if !ok {
		t.Fatalf("metadata is missing the Delivery of %s: %s", offline, payload)
	}
	if missed.Status != "offline" {
		t.Errorf("the offline Delivery status = %q, want offline", missed.Status)
	}
	if missed.Error == nil || missed.Error.Code != "offline" {
		t.Errorf("the offline Delivery error = %+v, want an offline business error", missed.Error)
	}
	if missed.CompletedAt == nil {
		t.Error("the offline Delivery carries no completion time")
	}

	// A request whose only entry was rejected admitted nothing, so it is not a
	// queryable Message even though its 207 reported the rejection.
	status, response, payload := environment.upload(t, sender.Uploader.Token,
		uploadBody("message-rejected", notAnObject))
	if status != http.StatusMultiStatus {
		t.Fatalf("rejected-only upload status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, "", "rejected", "invalid_delivery_item")
	if rows := environment.deliveriesOf(t, sender.SenderID, "message-rejected"); rows != 0 {
		t.Errorf("a rejected-only request admitted %d Deliveries, want none", rows)
	}

	status, _, payload = environment.message(t, sender.Uploader.Token, "message-rejected")
	if status != http.StatusNotFound {
		t.Errorf("rejected-only metadata status = %d, want 404: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "resource_not_found" {
		t.Errorf("rejected-only metadata code = %q, want resource_not_found", code)
	}

	status, _, _ = environment.message(t, sender.Uploader.Token, "message-unknown")
	if status != http.StatusNotFound {
		t.Errorf("unknown metadata status = %d, want 404", status)
	}

	// Only the Sender that admitted the Deliveries reads them.
	stranger := environment.sender(t, "code-stranger", "openid-stranger")
	status, _, _ = environment.message(t, stranger.Uploader.Token, messageID)
	if status != http.StatusNotFound {
		t.Errorf("another Sender reads %s with status %d, want 404", messageID, status)
	}
}

// TestRegisteredWorkWithoutAnOwnerIsInterrupted shows registered work no live
// process owns, which is what a restart leaves behind: the identity is
// finalized as interrupted rather than resumed, and a reconnected Receiver
// receives only traffic admitted after the restart.
func TestRegisteredWorkWithoutAnOwnerIsInterrupted(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-orphan", "openid-orphan")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Orphan")

	const messageID = "message-orphan"
	environment.exec(t, `
INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at)
VALUES ($1, $2, $3, $4, $5, 'processing', now())`,
		"delivery-orphan", sender.SenderID, messageID, receiverID, pairingID)

	// The process that registered that work is gone, so the identity keeps its
	// registered processing state with no owner left to confirm it.
	environment.restart(t)
	socket := receiverConnected(t, environment, receiverToken)

	status, response, payload := environment.upload(t, sender.Uploader.Token,
		uploadBody(messageID, entryOf(receiverID, pairingID)))
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "interrupted", "interrupted")

	status, views, payload := environment.message(t, sender.Uploader.Token, messageID)
	if status != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", status, payload)
	}
	orphan, ok := views[receiverID]
	if !ok {
		t.Fatalf("metadata is missing the registered work of %s: %s", receiverID, payload)
	}
	if orphan.Status != "interrupted" {
		t.Errorf("the registered work status = %q, want interrupted", orphan.Status)
	}
	if orphan.Error == nil || orphan.Error.Code != "interrupted" {
		t.Errorf("the registered work error = %+v, want an interrupted business error", orphan.Error)
	}
	if orphan.CompletedAt == nil {
		t.Error("the interrupted work carries no completion time")
	}

	// The reconnected Receiver receives only new traffic: the interrupted
	// identity is never recovered or re-sent, while a new Message still arrives.
	newer := environment.sendUpload(sender.Uploader.Token,
		uploadBody("message-orphan-next", entryOf(receiverID, pairingID)))
	frame := readDelivery(t, socket)
	if frame.MessageID != "message-orphan-next" {
		t.Fatalf("the forwarded frame = %+v, want the Message admitted after the restart", frame)
	}
	receipt(t, socket, frame)
	if status, _, payload := awaitUpload(t, newer); status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
}
