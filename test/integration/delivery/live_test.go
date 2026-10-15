//go:build integration

package delivery_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// secondCiphertext is a second accepted ciphertext of the same profile, so a
// test can show which of two submissions actually reached the Receiver.
const secondCiphertext = "GhscHR4fICEiIyQlJicoKSo="

// entryWithCiphertext renders one entry of the accepted profile that carries
// the given ciphertext.
func entryWithCiphertext(receiverID, pairingID, ciphertext string) string {
	return entryWith(receiverID, pairingID,
		`"cryptoVersion":1,"nonce":"`+syntheticNonce+`","ciphertext":"`+ciphertext+`"`)
}

// completedAtOf reads the stored completion time of one Delivery identity.
func (e *environment) completedAtOf(t *testing.T, senderID, messageID, receiverID string) time.Time {
	t.Helper()

	var completed time.Time
	if err := e.pool.QueryRowContext(context.Background(), `
SELECT completed_at FROM pin_glide.delivery_deliveries
WHERE sender_id = $1 AND message_id = $2 AND receiver_id = $3`,
		senderID, messageID, receiverID).Scan(&completed); err != nil {
		t.Fatalf("read the stored completion time: %v", err)
	}
	return completed
}

// TestDeliveryArrivesLiveAndItsReceiptConfirmsIt shows one accepted entry
// forwarded to the Receiver's current committed connection as the exact v1
// delivery envelope, and the Receipt that follows stored as a confirmed
// arrival.
func TestDeliveryArrivesLiveAndItsReceiptConfirmsIt(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-live", "openid-live")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Live")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-live"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	frame := readDelivery(t, socket)

	if frame.Version != protocol.WSSVersion || frame.Type != "delivery" {
		t.Errorf("frame = %+v, want a v1 delivery message", frame)
	}
	if frame.SenderID != sender.SenderID || frame.ReceiverID != receiverID || frame.PairingID != pairingID {
		t.Errorf("frame correlation = %+v, want %s, %s and %s", frame, sender.SenderID, receiverID, pairingID)
	}
	if frame.MessageID != messageID || frame.CryptoVersion != protocol.CryptoVersion {
		t.Errorf("frame = %+v, want message %s of crypto version %d", frame, messageID, protocol.CryptoVersion)
	}
	if frame.Nonce != syntheticNonce || frame.Ciphertext != syntheticCiphertext {
		t.Errorf("frame carries nonce %q and ciphertext %q, want the uploaded bytes unchanged",
			frame.Nonce, frame.Ciphertext)
	}

	receipt(t, socket, frame)

	status, response, payload := awaitUpload(t, answered)
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "received", "")

	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "received" {
		t.Errorf("the stored Delivery status = %q, want received", stored)
	}
	if completed := environment.count(t, `
SELECT count(*) FROM pin_glide.delivery_deliveries
WHERE sender_id = $1 AND message_id = $2 AND completed_at IS NOT NULL`,
		sender.SenderID, messageID); completed != 1 {
		t.Error("the confirmed arrival stored no completion time")
	}
}

// TestOnlyCorrelatedReceiptsConfirmADelivery shows that a Receipt counts by its
// authenticated Receiver, its Sender and its Message: a readable receipt that
// names anything else acknowledges nothing, and a correct Receipt that arrives
// after the terminal outcome never rewrites it.
func TestOnlyCorrelatedReceiptsConfirmADelivery(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-receipt", "openid-receipt")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Addressed")
	_, _, otherToken := environment.paired(t, sender, "Other")
	socket := receiverConnected(t, environment, receiverToken)
	other := receiverConnected(t, environment, otherToken)

	const messageID = "message-receipt"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	frame := readDelivery(t, socket)

	// Each of these frames is readable, but none of them acknowledges this
	// delivery: another Receiver connection, another Message and another
	// Sender. The delivery connection itself stays open.
	writeFrame(t, other, fmt.Sprintf(`{"version":1,"type":"receipt","senderId":%q,"messageId":%q}`,
		sender.SenderID, messageID))
	writeFrame(t, socket, fmt.Sprintf(`{"version":1,"type":"receipt","senderId":%q,"messageId":"message-other"}`,
		sender.SenderID))
	writeFrame(t, socket, fmt.Sprintf(`{"version":1,"type":"receipt","senderId":"sender-other","messageId":%q}`,
		messageID))

	status, response, payload := awaitUpload(t, answered)
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "ack_timeout", "ack_timeout")
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "ack_timeout" {
		t.Errorf("the stored Delivery status = %q, want ack_timeout", stored)
	}

	// The delivery already has its terminal outcome, and arrival is
	// acknowledged once: a late correct Receipt changes nothing.
	receipt(t, socket, frame)
	time.Sleep(200 * time.Millisecond)
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "ack_timeout" {
		t.Errorf("a late Receipt rewrote the terminal outcome to %q", stored)
	}
	expectNoDelivery(t, socket)
	expectNoDelivery(t, other)
}

// TestConcurrentDuplicateUploadsShareOneSend shows two uploads of the same
// Sender, Message and Receiver arriving together: one owner sends, the other
// shares that work and its result, and the Receiver is written once.
func TestConcurrentDuplicateUploadsShareOneSend(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-concurrent", "openid-concurrent")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Shared")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-concurrent"
	first := environment.sendUpload(sender.Uploader.Token,
		uploadBody(messageID, entryOf(receiverID, pairingID)))
	second := environment.sendUpload(sender.Uploader.Token,
		uploadBody(messageID, entryWithCiphertext(receiverID, pairingID, secondCiphertext)))

	frame := readDelivery(t, socket)
	if frame.MessageID != messageID || frame.ReceiverID != receiverID {
		t.Fatalf("the forwarded frame = %+v, want the shared Delivery of %s", frame, messageID)
	}
	if frame.Ciphertext != syntheticCiphertext && frame.Ciphertext != secondCiphertext {
		t.Errorf("the forwarded ciphertext = %q, want the bytes of one submitted entry", frame.Ciphertext)
	}
	receipt(t, socket, frame)

	for index, answered := range []<-chan uploadAttempt{first, second} {
		status, response, payload := awaitUpload(t, answered)
		if status != http.StatusMultiStatus {
			t.Fatalf("upload %d status = %d: %s", index, status, payload)
		}
		expectResult(t, response, 0, receiverID, "received", "")
	}

	if rows := environment.deliveriesOf(t, sender.SenderID, messageID); rows != 1 {
		t.Errorf("concurrent duplicate uploads admitted %d Deliveries, want one", rows)
	}
	expectNoDelivery(t, socket)
}

// TestRetainedTerminalResultIsReusedWithoutSendingAgain shows the retained
// identity answering a later submission with the outcome it stored, and the
// same submission validated again before that lookup once the Receiver's
// registration is closed.
func TestRetainedTerminalResultIsReusedWithoutSendingAgain(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-retained", "openid-retained")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Retained")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-retained"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	frame := readDelivery(t, socket)
	receipt(t, socket, frame)
	if status, _, payload := awaitUpload(t, answered); status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	completed := environment.completedAtOf(t, sender.SenderID, messageID, receiverID)

	// The identity is retained, so a later submission carrying different
	// ciphertext reads the stored result instead of sending again.
	status, response, payload := environment.upload(t, sender.Uploader.Token,
		uploadBody(messageID, entryWithCiphertext(receiverID, pairingID, secondCiphertext)))
	if status != http.StatusMultiStatus {
		t.Fatalf("re-submission status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "received", "")
	expectNoDelivery(t, socket)

	if rows := environment.deliveriesOf(t, sender.SenderID, messageID); rows != 1 {
		t.Errorf("the re-submission admitted %d Deliveries, want the retained one", rows)
	}
	if after := environment.completedAtOf(t, sender.SenderID, messageID, receiverID); !after.Equal(completed) {
		t.Errorf("the retained terminal outcome was rewritten: completed_at %s became %s", completed, after)
	}

	// Authority is validated again before that lookup: the closed Receiver is
	// reported as unauthorized rather than answered with its retained result.
	environment.revoke(t, sender, receiverID, "openid-retained")
	status, response, payload = environment.upload(t, sender.Uploader.Token,
		uploadBody(messageID, entryOf(receiverID, pairingID)))
	if status != http.StatusMultiStatus {
		t.Fatalf("upload after revocation = %d: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "rejected", "receiver_not_authorized")
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "received" {
		t.Errorf("a rejection rewrote the retained outcome to %q", stored)
	}
}

// TestAdmittedWorkKeepsItsAuthorityThroughRevocation shows the authorization
// decision an admitted request froze: revoking the Receiver while that request's
// send is in flight adds no check in front of it, while a new request validates
// the current authority again, and its rejection cannot rewrite the earlier
// Delivery's terminal record.
func TestAdmittedWorkKeepsItsAuthorityThroughRevocation(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-frozen", "openid-frozen")
	receiverID, pairingID, _ := environment.paired(t, sender, "Frozen")

	const messageID = "message-frozen"
	answered := environment.directUpload(sender.Uploader.Token, directRequest(messageID, receiverID, pairingID))
	frame := environment.channel.nextWrite(t)
	if frame.MessageID != messageID || frame.PairingID != pairingID {
		t.Fatalf("the attempted write = %+v, want the admitted entry of %s", frame, messageID)
	}

	// The entry was validated and authorized before this send began, so the
	// revocation that follows is not a second decision about it.
	environment.revoke(t, sender, receiverID, "openid-frozen")
	environment.channel.confirm()
	environment.delivery.Receipt(sender.SenderID, messageID, receiverID)

	outcome := <-answered
	expectOutcome(t, outcome, 0, receiverID, "received", nil)
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "received" {
		t.Errorf("the stored Delivery status = %q, want received", stored)
	}

	// A new request is authorized on its own, and rejecting it leaves the
	// stored outcome of the earlier Delivery alone.
	status, response, payload := environment.upload(t, sender.Uploader.Token,
		uploadBody(messageID, entryOf(receiverID, pairingID)))
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "rejected", "receiver_not_authorized")
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "received" {
		t.Errorf("a rejection rewrote the stored outcome to %q", stored)
	}
	environment.channel.expectNoWrite(t)
}
