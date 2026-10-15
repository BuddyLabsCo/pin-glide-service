//go:build integration

package delivery_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/delivery"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// writeDecision is what one controlled write attempt ended in.
type writeDecision struct {
	delivered bool
	err       error
}

// controlledChannel is the live delivery channel of a test that decides what
// one write to a Receiver does. It reports the frame it was given and waits for
// that decision, so a failing write or a write that never finishes is produced
// on demand instead of raced against a real Receiver connection. It sends no
// ciphertext of its own.
type controlledChannel struct {
	writes    chan protocol.Delivery
	decisions chan writeDecision
}

// newControlledChannel returns a channel no test has decided anything about.
func newControlledChannel() *controlledChannel {
	return &controlledChannel{
		writes:    make(chan protocol.Delivery, 4),
		decisions: make(chan writeDecision),
	}
}

// Deliver reports one write attempt and waits for the test's decision about it.
func (c *controlledChannel) Deliver(_ string, frame protocol.Delivery, _ time.Duration) (bool, error) {
	c.writes <- frame
	decision := <-c.decisions
	return decision.delivered, decision.err
}

// nextWrite waits for one write attempt to reach the channel.
func (c *controlledChannel) nextWrite(t *testing.T) protocol.Delivery {
	t.Helper()

	select {
	case frame := <-c.writes:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery write reached the channel")
		return protocol.Delivery{}
	}
}

// confirm ends the waiting write as one the Receiver connection accepted.
func (c *controlledChannel) confirm() {
	c.decisions <- writeDecision{delivered: true}
}

// fail ends the waiting write as one that failed, which leaves arrival
// unconfirmed rather than absent.
func (c *controlledChannel) fail(err error) {
	c.decisions <- writeDecision{delivered: false, err: err}
}

// expectNoWrite requires the channel to receive no further write, which is what
// shows the service did not compensate for an outcome with a second send.
func (c *controlledChannel) expectNoWrite(t *testing.T) {
	t.Helper()

	select {
	case frame := <-c.writes:
		t.Fatalf("the channel received another write: %+v", frame)
	case <-time.After(750 * time.Millisecond):
	}
}

// directOutcome is one finished upload of the controlled service.
type directOutcome struct {
	results []domain.Result
	err     error
}

// directRequest builds the accepted request of one entry for the service whose
// transport the test decides.
func directRequest(messageID, receiverID, pairingID string) domain.Request {
	version := protocol.CryptoVersion
	return domain.Request{MessageID: messageID, Items: []domain.Item{{
		ReceiverID:    receiverID,
		PairingID:     pairingID,
		CryptoVersion: &version,
		Nonce:         syntheticNonce,
		Ciphertext:    syntheticCiphertext,
	}}}
}

// directUpload runs one upload through the service whose transport the test
// decides, reporting its results when it finishes.
func (e *environment) directUpload(credential string, request domain.Request) <-chan directOutcome {
	finished := make(chan directOutcome, 1)
	go func() {
		defer close(finished)
		results, err := e.delivery.Upload(context.Background(), credential, request)
		finished <- directOutcome{results: results, err: err}
	}()
	return finished
}

// expectOutcome requires one result of a controlled upload to carry the index,
// Receiver, status and typed failure the domain reports for it.
func expectOutcome(t *testing.T, outcome directOutcome, index int, receiverID, status string, cause error) {
	t.Helper()

	if outcome.err != nil {
		t.Fatalf("upload failed: %v", outcome.err)
	}
	if index >= len(outcome.results) {
		t.Fatalf("the upload answered %d results, so index %d is absent", len(outcome.results), index)
	}
	result := outcome.results[index]
	if result.Index != index || result.ReceiverID != receiverID {
		t.Errorf("result %d = %+v, want index %d of Receiver %s", index, result, index, receiverID)
	}
	if string(result.Status) != status {
		t.Errorf("result %d status = %q, want %q", index, result.Status, status)
	}
	if !errors.Is(result.Err, cause) {
		t.Errorf("result %d error = %v, want %v", index, result.Err, cause)
	}
}

// TestWriteFailureAfterAPossibleArrivalIsTerminal shows the outcome a caller
// cannot be sure about: the write to the Receiver failed, so arrival is not
// guaranteed absent. The service reports and stores send_failed and never sends
// a compensating copy of the ciphertext.
func TestWriteFailureAfterAPossibleArrivalIsTerminal(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-send-failed", "openid-send-failed")
	receiverID, pairingID, _ := environment.paired(t, sender, "Failing")

	const messageID = "message-send-failed"
	answered := environment.directUpload(sender.Uploader.Token, directRequest(messageID, receiverID, pairingID))

	frame := environment.channel.nextWrite(t)
	if frame.MessageID != messageID || frame.ReceiverID != receiverID || frame.Ciphertext != syntheticCiphertext {
		t.Fatalf("the attempted write = %+v, want the submitted entry of %s", frame, messageID)
	}
	environment.channel.fail(errors.New("connection reset"))

	outcome := <-answered
	expectOutcome(t, outcome, 0, receiverID, "send_failed", domain.ErrSendFailed)
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "send_failed" {
		t.Errorf("the stored Delivery status = %q, want send_failed", stored)
	}
	environment.channel.expectNoWrite(t)
}

// TestDuplicateCallerWithoutASharedResultReportsUncertainty shows a duplicate
// submission of an identity whose live work this process owns: it joins that
// work instead of sending its own copy, and when its own budget ends without the
// shared terminal result it reports the accepted uncertainty rather than a
// pending success.
func TestDuplicateCallerWithoutASharedResultReportsUncertainty(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-joined-timeout", "openid-joined-timeout")
	receiverID, pairingID, _ := environment.paired(t, sender, "Shared")

	const messageID = "message-joined-timeout"
	owner := environment.directUpload(sender.Uploader.Token, directRequest(messageID, receiverID, pairingID))
	// The owner's send is in flight and stays so, which is what keeps the
	// identity live for the whole budget of every caller that joins it.
	frame := environment.channel.nextWrite(t)
	if frame.MessageID != messageID {
		t.Fatalf("the attempted write = %+v, want %s", frame, messageID)
	}

	outcome := <-environment.directUpload(sender.Uploader.Token, directRequest(messageID, receiverID, pairingID))
	if len(outcome.results) != 0 {
		t.Errorf("the duplicate caller was answered %+v, want an uncertainty rather than a result", outcome.results)
	}
	if !errors.Is(outcome.err, domain.ErrStatusUnavailable) {
		t.Errorf("the duplicate caller failed with %v, want %v", outcome.err, domain.ErrStatusUnavailable)
	}
	// Joining the live work never sends the duplicate's own ciphertext.
	environment.channel.expectNoWrite(t)

	// Releasing the owner's write lets the process finish the work it still owns.
	environment.channel.fail(errors.New("connection reset"))
	<-owner
}

// withoutDeliveryStorage hides this environment's retained Delivery table the
// way unavailable storage reaches the service: the credential still authorizes
// the caller and the Delivery statements fail.
func (e *environment) withoutDeliveryStorage(t *testing.T) {
	t.Helper()
	e.exec(t, "ALTER TABLE pin_glide.delivery_deliveries RENAME TO delivery_deliveries_hidden")
}

// withDeliveryStorage restores the retained Delivery table, which is the storage
// a later read of the retained identities needs.
func (e *environment) withDeliveryStorage(t *testing.T) {
	t.Helper()
	e.exec(t, "ALTER TABLE pin_glide.delivery_deliveries_hidden RENAME TO delivery_deliveries")
}

// TestMissingStorageAtRegistrationReportsUncertaintyWithoutSending shows an
// upload whose Delivery identity cannot be registered: the caller cannot obtain
// a reliable result, so it reports the accepted uncertainty, and neither a send
// nor a Delivery claim happens for the entry.
func TestMissingStorageAtRegistrationReportsUncertaintyWithoutSending(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-no-storage", "openid-no-storage")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Unstorable")
	socket := receiverConnected(t, environment, receiverToken)

	environment.withoutDeliveryStorage(t)
	status, payload := environment.request(t, http.MethodPost, "/v1/messages",
		uploadBody("message-no-storage", entryOf(receiverID, pairingID)), sender.Uploader.Token)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("upload status = %d, want 503: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "delivery_status_unavailable" {
		t.Errorf("upload code = %q, want delivery_status_unavailable", code)
	}

	// The entry never reached the Receiver, because nothing was claimed for it.
	expectNoDelivery(t, socket)
}

// TestMissingStorageAtFinalizationReportsUncertaintyWithoutResending shows the
// other side of the same uncertainty: the Receiver reported arrival and the
// terminal outcome cannot be stored. The caller is told arrival is unconfirmed,
// no compensating copy is sent, and the retained identity is finalized as
// interrupted once storage is back.
func TestMissingStorageAtFinalizationReportsUncertaintyWithoutResending(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-lost-outcome", "openid-lost-outcome")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Unstorable")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-lost-outcome"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	frame := readDelivery(t, socket)

	// The Receiver reports arrival, and storing that outcome fails.
	environment.withoutDeliveryStorage(t)
	receipt(t, socket, frame)

	status, _, payload := awaitUpload(t, answered)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("upload status = %d, want 503: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "delivery_status_unavailable" {
		t.Errorf("upload code = %q, want delivery_status_unavailable", code)
	}

	// The terminal outcome could not be stored, so the identity keeps its
	// registered processing state and is never sent a second time.
	environment.withDeliveryStorage(t)
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "processing" {
		t.Errorf("the unstoreable outcome left the identity %q, want processing", stored)
	}

	// Storage is available again, so the identity without a live owner is
	// finalized as interrupted rather than left pending or resumed.
	status, views, payload := environment.message(t, sender.Uploader.Token, messageID)
	if status != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", status, payload)
	}
	unconfirmed, ok := views[receiverID]
	if !ok {
		t.Fatalf("metadata is missing the Delivery of %s: %s", receiverID, payload)
	}
	if unconfirmed.Status != "interrupted" {
		t.Errorf("the unconfirmed Delivery status = %q, want interrupted", unconfirmed.Status)
	}
	if unconfirmed.CompletedAt == nil {
		t.Error("the interrupted Delivery carries no completion time")
	}
	expectNoDelivery(t, socket)
}
