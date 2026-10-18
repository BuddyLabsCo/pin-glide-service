//go:build integration

package delivery_test

import (
	"net/http"
	"testing"
)

// A QQ-issued uploader credential admits Delivery work under the same uploader
// authorization rules as a WeChat-issued credential.
func TestQQUploaderAdmitsDelivery(t *testing.T) {
	environment := start(t)
	sender := environment.qqSender(t, "code-delivery-qq", "openid-delivery-qq")
	receiverID, pairingID, _ := environment.paired(t, sender, "QQ Phone")

	messageID := "message-delivery-qq"
	status, response, payload := environment.upload(t, sender.Uploader.Token,
		uploadBody(messageID, entryOf(receiverID, pairingID)))
	if status != http.StatusMultiStatus {
		t.Fatalf("QQ upload status = %d, want 207: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "offline", "offline")
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "offline" {
		t.Errorf("stored Delivery status = %q, want offline", stored)
	}
}
