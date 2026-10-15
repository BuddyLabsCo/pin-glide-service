//go:build integration

package delivery_test

import (
	"net/http"
	"testing"
)

// profileFields is the accepted encryption profile of one entry. Its bytes are
// synthetic: the service validates the envelope without decrypting anything, so
// only syntax and length matter here.
const profileFields = `"cryptoVersion":1,"nonce":"` + syntheticNonce + `","ciphertext":"` + syntheticCiphertext + `"`

// notAnObject is an entry the envelope carries as JSON but not as an item of
// this profile, so nothing in it is readable.
const notAnObject = `"not-an-object"`

// entryWith renders one upload entry from explicit profile fields, so a test
// states exactly which part of the accepted envelope an entry carries.
func entryWith(receiverID, pairingID, fields string) string {
	return `{"receiverId":"` + receiverID + `","pairingId":"` + pairingID + `",` + fields + `}`
}

// expectResult requires one 207 entry to carry the original index, the readable
// Receiver, the status and the business code a client matches on.
func expectResult(t *testing.T, response uploadResponse, index int, receiverID, status, code string) {
	t.Helper()

	if index >= len(response.Results) {
		t.Fatalf("the response carries %d results, so index %d is absent", len(response.Results), index)
	}
	result := response.Results[index]
	if result.Index != index {
		t.Errorf("result %d reports index %d", index, result.Index)
	}
	if result.ReceiverID != receiverID {
		t.Errorf("result %d reports receiver %q, want %q", index, result.ReceiverID, receiverID)
	}
	if result.Status != status {
		t.Errorf("result %d status = %q, want %q", index, result.Status, status)
	}
	if code == "" {
		if result.Error != nil {
			t.Errorf("result %d carries error %+v, want none", index, result.Error)
		}
		return
	}
	if result.Error == nil {
		t.Fatalf("result %d carries no error, want %s", index, code)
	}
	if result.Error.Code != code {
		t.Errorf("result %d error code = %q, want %q", index, result.Error.Code, code)
	}
	if result.Error.Detail == "" {
		t.Errorf("result %d reports %s without a detail", index, code)
	}
}

// TestUploadAnswersOneIndexedTerminalResultPerEntry shows one processable
// upload answered with exactly one terminal result per input entry: the entry
// with current authority is forwarded, and every other entry is rejected or
// reported on its own index without suppressing its neighbours.
func TestUploadAnswersOneIndexedTerminalResultPerEntry(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-mixed", "openid-mixed")

	live, livePairing, liveToken := environment.paired(t, sender, "Live")
	malformed, malformedPairing, _ := environment.paired(t, sender, "Malformed")
	offline, offlinePairing, _ := environment.paired(t, sender, "Offline")
	stray, _, _ := environment.paired(t, sender, "Stray")
	revoked, revokedPairing, _ := environment.paired(t, sender, "Revoked")
	environment.revoke(t, sender, revoked, "openid-mixed")

	// A Receiver of another Account, which this Sender's credential never
	// authorizes even when it names the Pairing that Account activated.
	otherAccount := environment.sender(t, "code-foreign", "openid-foreign")
	foreign, foreignPairing, _ := environment.paired(t, otherAccount, "Foreign")

	socket := receiverConnected(t, environment, liveToken)

	const messageID = "message-mixed"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID,
		entryOf(live, livePairing),
		entryWith(malformed, malformedPairing,
			`"cryptoVersion":1,"nonce":"not-padded-base64","ciphertext":"`+syntheticCiphertext+`"`),
		entryWith("receiver-unknown", livePairing, profileFields),
		entryWith(stray, livePairing, profileFields),
		entryWith(foreign, foreignPairing, profileFields),
		entryWith(revoked, revokedPairing, profileFields),
		entryOf(offline, offlinePairing),
		notAnObject,
		entryWith("receiver-unversioned", livePairing,
			`"cryptoVersion":2,"nonce":"`+syntheticNonce+`","ciphertext":"`+syntheticCiphertext+`"`),
	))

	// Only the entry with current authority reaches the Receiver.
	frame := readDelivery(t, socket)
	if frame.MessageID != messageID || frame.ReceiverID != live {
		t.Fatalf("the forwarded frame = %+v, want the live entry of %s", frame, messageID)
	}
	receipt(t, socket, frame)

	status, response, payload := awaitUpload(t, answered)
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	if len(response.Results) != 9 {
		t.Fatalf("upload answered %d results, want one per input entry: %s", len(response.Results), payload)
	}

	expectResult(t, response, 0, live, "received", "")
	expectResult(t, response, 1, malformed, "rejected", "invalid_delivery_item")
	expectResult(t, response, 2, "receiver-unknown", "rejected", "receiver_not_authorized")
	expectResult(t, response, 3, stray, "rejected", "receiver_not_authorized")
	expectResult(t, response, 4, foreign, "rejected", "receiver_not_authorized")
	expectResult(t, response, 5, revoked, "rejected", "receiver_not_authorized")
	expectResult(t, response, 6, offline, "offline", "offline")
	expectResult(t, response, 7, "", "rejected", "invalid_delivery_item")
	expectResult(t, response, 8, "receiver-unversioned", "rejected", "unsupported_crypto_version")

	if stored := environment.statusOf(t, sender.SenderID, messageID, live); stored != "received" {
		t.Errorf("the stored live Delivery status = %q, want received", stored)
	}
	if stored := environment.statusOf(t, sender.SenderID, messageID, offline); stored != "offline" {
		t.Errorf("the stored offline Delivery status = %q, want offline", stored)
	}
	if rows := environment.deliveriesOf(t, sender.SenderID, messageID); rows != 2 {
		t.Errorf("the request admitted %d Deliveries, want the 2 entries it forwarded", rows)
	}
}

// TestUploadRejectsEveryDuplicateReceiverEntry shows a request that names one
// Receiver twice: both entries are rejected, because the service never chooses
// between two ciphertexts for one Delivery, and an unrelated unique entry of
// the same request still continues on its own.
func TestUploadRejectsEveryDuplicateReceiverEntry(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-duplicate", "openid-duplicate")

	duplicated, duplicatedPairing, _ := environment.paired(t, sender, "Duplicated")
	survivor, survivorPairing, survivorToken := environment.paired(t, sender, "Survivor")
	socket := receiverConnected(t, environment, survivorToken)

	const messageID = "message-duplicate"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID,
		entryOf(duplicated, duplicatedPairing),
		entryOf(duplicated, duplicatedPairing),
		entryOf(survivor, survivorPairing),
	))

	frame := readDelivery(t, socket)
	if frame.ReceiverID != survivor {
		t.Fatalf("the forwarded frame = %+v, want the unrelated unique entry only", frame)
	}
	receipt(t, socket, frame)

	status, response, payload := awaitUpload(t, answered)
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}

	expectResult(t, response, 0, duplicated, "rejected", "duplicate_receiver")
	expectResult(t, response, 1, duplicated, "rejected", "duplicate_receiver")
	expectResult(t, response, 2, survivor, "received", "")

	if stored := environment.statusOf(t, sender.SenderID, messageID, duplicated); stored != "" {
		t.Errorf("a duplicated Receiver stored the Delivery %q", stored)
	}
	if stored := environment.statusOf(t, sender.SenderID, messageID, survivor); stored != "received" {
		t.Errorf("the unrelated unique entry stored %q, want received", stored)
	}
	if rows := environment.deliveriesOf(t, sender.SenderID, messageID); rows != 1 {
		t.Errorf("the request admitted %d Deliveries, want only the unrelated unique entry", rows)
	}
	expectNoDelivery(t, socket)
}

// TestUploadDistinguishesWholeRequestFailures shows an unreadable envelope
// answered as a whole-request problem, while a processable envelope is answered
// entry by entry even when it carries no entries at all.
func TestUploadDistinguishesWholeRequestFailures(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-envelope", "openid-envelope")

	cases := []struct {
		name     string
		body     string
		status   int
		code     string
		location string
	}{
		{
			name:     "a body that is not JSON",
			body:     `{"messageId":`,
			status:   http.StatusBadRequest,
			code:     "invalid_request",
			location: "body",
		},
		{
			name:     "a JSON body that is not the envelope",
			body:     `[]`,
			status:   http.StatusUnprocessableEntity,
			code:     "validation_failed",
			location: "body",
		},
		{
			name:     "an envelope without messageId",
			body:     `{"items":[]}`,
			status:   http.StatusUnprocessableEntity,
			code:     "validation_failed",
			location: "body.messageId",
		},
		{
			name:     "an envelope without items",
			body:     `{"messageId":"message-envelope"}`,
			status:   http.StatusUnprocessableEntity,
			code:     "validation_failed",
			location: "body.items",
		},
		{
			name:     "an envelope whose items are not an array",
			body:     `{"messageId":"message-envelope","items":{}}`,
			status:   http.StatusUnprocessableEntity,
			code:     "validation_failed",
			location: "body.items",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, payload := environment.request(t, http.MethodPost, "/v1/messages", test.body, sender.Uploader.Token)
			if status != test.status {
				t.Fatalf("status = %d, want %d: %s", status, test.status, payload)
			}

			failure := decodeProblem(t, payload)
			if failure.Code != test.code || failure.Status != test.status {
				t.Errorf("problem = %s of status %d, want %s of status %d", failure.Code, failure.Status, test.code, test.status)
			}
			if failure.RequestID == "" {
				t.Errorf("the problem carries no requestId: %s", payload)
			}
			if len(failure.Errors) == 0 || failure.Errors[0].Location != test.location {
				t.Errorf("problem errors = %+v, want location %q", failure.Errors, test.location)
			}
		})
	}

	// An enumerable envelope is processable whatever its entries are, so an
	// empty batch is a 207 with no result rather than a whole-request failure.
	status, response, payload := environment.upload(t, sender.Uploader.Token,
		`{"messageId":"message-empty","items":[]}`)
	if status != http.StatusMultiStatus {
		t.Fatalf("empty-batch status = %d, want 207: %s", status, payload)
	}
	if len(response.Results) != 0 {
		t.Errorf("empty-batch results = %+v, want none", response.Results)
	}
}
