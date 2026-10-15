//go:build integration

package delivery_test

import (
	"bytes"
	"encoding/json"
	"errors"
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

	arrived := expectView(t, views, live, "received", "")
	if arrived.PairingID != livePairing {
		t.Errorf("the confirmed Delivery names Pairing %q, want %q", arrived.PairingID, livePairing)
	}
	if arrived.CreatedAt.IsZero() {
		t.Error("the confirmed Delivery carries no admission time")
	}

	expectView(t, views, offline, "offline", "offline")

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
	expectView(t, views, receiverID, "interrupted", "interrupted")

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

// expectView requires one retained metadata view to carry the status the query
// reports, the stable business code of a terminal failure, and a completion
// time for exactly the terminal statuses: processing is the one status the
// adopted view reports without one.
func expectView(t *testing.T, views map[string]deliveryView, receiverID, status, code string) deliveryView {
	t.Helper()

	view, ok := views[receiverID]
	if !ok {
		t.Fatalf("metadata reports no %s Delivery of %s: %+v", status, receiverID, views)
	}
	if view.Status != status {
		t.Errorf("the Delivery of %s has status %q, want %q", receiverID, view.Status, status)
	}
	if code == "" {
		if view.Error != nil {
			t.Errorf("the %s Delivery of %s carries error %+v, want none", status, receiverID, view.Error)
		}
	} else if view.Error == nil || view.Error.Code != code {
		t.Errorf("the Delivery of %s carries error %+v, want %s", receiverID, view.Error, code)
	}
	if status == "processing" {
		if view.CompletedAt != nil {
			t.Errorf("the processing Delivery of %s carries a completion time", receiverID)
		}
	} else if view.CompletedAt == nil {
		t.Errorf("the %s Delivery of %s carries no completion time", status, receiverID)
	}
	return view
}

// TestMessageMetadataReportsLiveProcessingWhileTheDeliveryRuns shows the one
// status only this query reports: work that is still running is processing,
// without a completion time or an outcome, and the same query reports the
// terminal result once the Receiver's arrival is confirmed. The answer stays
// metadata, so nothing the Sender uploaded appears in it.
func TestMessageMetadataReportsLiveProcessingWhileTheDeliveryRuns(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-processing", "openid-processing")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Processing")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-processing"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	// The service is waiting for this Receipt, so the Delivery it admitted is
	// still running while the query reads it.
	frame := readDelivery(t, socket)

	status, views, payload := environment.message(t, sender.Uploader.Token, messageID)
	if status != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", status, payload)
	}
	if live := expectView(t, views, receiverID, "processing", ""); live.PairingID != pairingID {
		t.Errorf("the processing Delivery names Pairing %q, want %q", live.PairingID, pairingID)
	}
	if bytes.Contains(payload, []byte(syntheticCiphertext)) {
		t.Errorf("the metadata payload carries the uploaded ciphertext: %s", payload)
	}

	receipt(t, socket, frame)
	if status, _, payload := awaitUpload(t, answered); status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}

	// Processing was never a stored outcome: the confirmed arrival is what the
	// same query reports afterwards.
	status, views, payload = environment.message(t, sender.Uploader.Token, messageID)
	if status != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", status, payload)
	}
	expectView(t, views, receiverID, "received", "")
}

// TestMessageMetadataOutlivesPairingRemovalAndReceiverRevocation shows that
// reading one's own retained metadata does not need the Pairing to stay active:
// the Deliveries admitted under an association the Sender removed, and under a
// Receiver the Account owner revoked, still report the outcome each stored.
// That history authorizes no forwarding, so a later submission that lost its
// authority is rejected on its own and leaves the history as it was.
func TestMessageMetadataOutlivesPairingRemovalAndReceiverRevocation(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-outlives", "openid-outlives")
	removed, removedPairing, removedToken := environment.paired(t, sender, "Removed")
	revoked, revokedPairing, revokedToken := environment.paired(t, sender, "Revoked")
	removedSocket := receiverConnected(t, environment, removedToken)
	revokedSocket := receiverConnected(t, environment, revokedToken)

	const messageID = "message-outlives"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID,
		entryOf(removed, removedPairing),
		entryOf(revoked, revokedPairing),
	))
	receipt(t, removedSocket, readDelivery(t, removedSocket))
	receipt(t, revokedSocket, readDelivery(t, revokedSocket))
	if status, _, payload := awaitUpload(t, answered); status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}

	// The Sender removes one own association and the Account owner revokes the
	// other Receiver, so neither Pairing forwards any more.
	if status, payload := environment.request(t, http.MethodDelete, "/v1/self/pairings/"+removedPairing,
		"", sender.UserSession.Token); status != http.StatusNoContent {
		t.Fatalf("association removal = %d: %s", status, payload)
	}
	environment.revoke(t, sender, revoked, "openid-outlives")

	status, views, payload := environment.message(t, sender.Uploader.Token, messageID)
	if status != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", status, payload)
	}
	if len(views) != 2 {
		t.Fatalf("metadata reports %d Deliveries, want the 2 admitted ones: %s", len(views), payload)
	}
	expectView(t, views, removed, "received", "")
	expectView(t, views, revoked, "received", "")

	// The retained history never overrides upload validation: the entry whose
	// Pairing was removed is rejected, and that rejection leaves the admitted
	// Deliveries of the same Message untouched.
	status, response, payload := environment.upload(t, sender.Uploader.Token,
		uploadBody(messageID, entryOf(removed, removedPairing)))
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, removed, "rejected", "receiver_not_authorized")
	if rows := environment.deliveriesOf(t, sender.SenderID, messageID); rows != 2 {
		t.Errorf("the rejected submission left %d admitted Deliveries, want the 2 it found", rows)
	}
	status, views, payload = environment.message(t, sender.Uploader.Token, messageID)
	if status != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", status, payload)
	}
	expectView(t, views, removed, "received", "")
}

// TestMessageMetadataOfPurgedRowsIsAbsent shows the other end of the retained
// lifetime: a Message whose rows scheduled cleanup deleted reads like one that
// never admitted a Delivery, and the deleted identity deduplicates nothing, so
// a later submission of it is admitted as new work rather than answered from
// the history cleanup removed.
func TestMessageMetadataOfPurgedRowsIsAbsent(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-purged", "openid-purged")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Purged")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-purged"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	receipt(t, socket, readDelivery(t, socket))
	if status, _, payload := awaitUpload(t, answered); status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}

	// Scheduled cleanup deletes whole retained rows by age, so the query that
	// reads that table finds nothing left to report.
	environment.exec(t, `
DELETE FROM pin_glide.delivery_deliveries
WHERE sender_id = $1 AND message_id = $2`, sender.SenderID, messageID)

	status, _, payload := environment.message(t, sender.Uploader.Token, messageID)
	if status != http.StatusNotFound {
		t.Fatalf("purged metadata status = %d, want 404: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "resource_not_found" {
		t.Errorf("purged metadata code = %q, want resource_not_found", code)
	}

	// Deduplication is row existence, so the same Message ID is new work again.
	answered = environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	receipt(t, socket, readDelivery(t, socket))
	status, response, payload := awaitUpload(t, answered)
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "received", "")
	if rows := environment.deliveriesOf(t, sender.SenderID, messageID); rows != 1 {
		t.Errorf("the Message retains %d Deliveries after cleanup, want the 1 admitted again", rows)
	}
}

// TestMessageMetadataLeavesTheRetentionClockAlone shows the query only reads:
// repeated lookups neither rewrite, add nor remove a retained Delivery, and the
// admission time scheduled cleanup counts from keeps exactly the value
// admission stored, so a lookup cannot extend how long a row is retained.
func TestMessageMetadataLeavesTheRetentionClockAlone(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-retention", "openid-retention")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Retained")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-retention"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	receipt(t, socket, readDelivery(t, socket))
	if status, _, payload := awaitUpload(t, answered); status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}

	// Ageing the retained row is what shows whether a lookup refreshes the time
	// cleanup counts from.
	environment.exec(t, `
UPDATE pin_glide.delivery_deliveries
SET created_at = now() - interval '1 hour'
WHERE sender_id = $1 AND message_id = $2`, sender.SenderID, messageID)

	for range 2 {
		status, views, payload := environment.message(t, sender.Uploader.Token, messageID)
		if status != http.StatusOK {
			t.Fatalf("metadata status = %d: %s", status, payload)
		}
		expectView(t, views, receiverID, "received", "")
	}

	if refreshed := environment.count(t, `
SELECT count(*) FROM pin_glide.delivery_deliveries
WHERE sender_id = $1 AND message_id = $2 AND created_at > now() - interval '30 minutes'`,
		sender.SenderID, messageID); refreshed != 0 {
		t.Errorf("a metadata read refreshed the retention clock of %d retained rows", refreshed)
	}
	if rows := environment.deliveriesOf(t, sender.SenderID, messageID); rows != 1 {
		t.Errorf("a metadata read left %d retained rows, want the 1 it admitted", rows)
	}
}

// TestMessageMetadataReportsTheOutcomesOnlyItCanShow shows the two uncertain
// outcomes of an attempt as the query reports them: the write that failed and
// the Receipt that never came keep the business code the Sender was told, with
// the time the Delivery finished.
func TestMessageMetadataReportsTheOutcomesOnlyItCanShow(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-outcomes", "openid-outcomes")
	failed, failedPairing, _ := environment.paired(t, sender, "Failed")
	late, latePairing, lateToken := environment.paired(t, sender, "Late")
	socket := receiverConnected(t, environment, lateToken)

	const messageID = "message-outcomes"
	// One entry's write fails, which leaves arrival unconfirmed rather than
	// absent and is what a real Receiver connection cannot be made to produce.
	// Its Delivery finishes before the other entry of the same Message is
	// admitted, so each outcome belongs to the attempt that stored it.
	written := environment.directUpload(sender.Uploader.Token, directRequest(messageID, failed, failedPairing))
	environment.channel.nextWrite(t)
	environment.channel.fail(errors.New("connection reset"))
	if outcome := <-written; outcome.err != nil {
		t.Fatalf("the failing write stored no outcome: %v", outcome.err)
	}

	// The other entry is written to a Receiver that never acknowledges it, so
	// its Delivery waits out the adopted Receipt bound.
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(late, latePairing)))
	frame := readDelivery(t, socket)
	if frame.MessageID != messageID || frame.ReceiverID != late {
		t.Fatalf("the forwarded frame = %+v, want the Delivery of %s to %s", frame, messageID, late)
	}
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
	expectView(t, views, failed, "send_failed", "send_failed")
	expectView(t, views, late, "ack_timeout", "ack_timeout")
}
