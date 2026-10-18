//go:build integration

package pairing_test

import (
	"net/http"
	"testing"
)

// A QQ-issued management credential authorizes the Sender side of the same
// restricted Pairing WSS channel as a WeChat-issued credential.
func TestQQManagementCredentialAuthorizesPairingWSS(t *testing.T) {
	environment := start(t)
	sender := environment.qqSender(t, "code-pairing-qq", "openid-pairing-qq")
	created, _ := environment.createSession(t, "new_receiver", "", "")

	status, claimed, payload := environment.authorize(t, created.SessionID, "QQ phone", sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("QQ Pairing claim status = %d, want 200: %s", status, payload)
	}
	if claimed.SenderID != sender.SenderID {
		t.Fatalf("QQ Pairing claim Sender = %q, want %q", claimed.SenderID, sender.SenderID)
	}

	socket := environment.dial(t, created.SessionID, sender.UserSession.Token)
	var snapshot struct {
		Type    string      `json:"type"`
		Session sessionView `json:"session"`
	}
	readFrame(t, socket, &snapshot)
	if snapshot.Type != "pairing_state" || snapshot.Session.SenderID != sender.SenderID {
		t.Errorf("QQ Pairing WSS snapshot = %+v, want the claimed Sender", snapshot)
	}
}
