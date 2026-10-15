//go:build integration

// An Account switch is exercised the way a Receiver experiences it: one HTTP
// request over the composed service and the pairing_removed announcement on the
// delivery connection that Receiver already holds.
package pairing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// switchAccountBody is the wire body of one Account switch.
const switchAccountBody = `{"mode":"switch_account"}`

// A Sender's Account switch withdraws exactly its own forwarding authority: the
// Receiver learns about the removed association on the delivery connection it
// already holds, and that connection, the Receiver's credential and every other
// Sender's association keep working.
func TestAccountSwitchAnnouncesItsRemovedAssociationToTheReceiver(t *testing.T) {
	environment := start(t)
	first := environment.sender(t, "code-switch-notify", "openid-switch-notify")
	held, issued := environment.paired(t, first, "Phone")
	// The second Sender is another installation of the same Account, which is
	// the only Sender that can hold an association with the same Receiver.
	second := environment.sender(t, "code-switch-bystander", "openid-switch-notify")
	attached, token := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	status, claimed, payload := environment.authorize(t, attached.SessionID, "Tablet", second.UserSession.Token)
	if status != http.StatusOK || claimed.PairingID == "" {
		t.Fatalf("the other Sender's claim = %d %+v, want a live association: %s", status, claimed, payload)
	}
	// Its association is live, so the switch has to leave working authority
	// alone rather than only a reservation.
	environment.activate(t, claimed.SessionID, second.UserSession.Token, token)
	if state := environment.associationOf(t, claimed.PairingID); state.state != "active" {
		t.Fatalf("the other Sender's association = %q, want active", state.state)
	}
	delivery := receiverConnected(t, environment, issued.ReceiverToken)

	if status, payload := environment.post(t, "/v1/auth/logout", switchAccountBody, first.UserSession.Token); status != http.StatusNoContent {
		t.Fatalf("switch status = %d, want %d: %s", status, http.StatusNoContent, payload)
	}

	var control struct {
		Version   int    `json:"version"`
		Type      string `json:"type"`
		SenderID  string `json:"senderId"`
		PairingID string `json:"pairingId"`
	}
	readFrame(t, delivery, &control)
	if control.Type != "pairing_removed" || control.Version != 1 ||
		control.SenderID != first.SenderID || control.PairingID != held.PairingID {
		t.Errorf("switch control = %+v, want pairing_removed of %q by %q", control, held.PairingID, first.SenderID)
	}
	if state := environment.associationOf(t, claimed.PairingID); state.state != "active" {
		t.Errorf("the other Sender's association = %q after the switch, want its untouched active one", state.state)
	}
	if status, kept, payload := environment.listOwnReceivers(t, second.UserSession.Token); status != http.StatusOK || len(kept) != 1 {
		t.Errorf("the other Sender's list = %d %+v, want its association kept: %s", status, kept, payload)
	}

	// The announcement is not a close: the shared delivery connection keeps
	// serving the other Sender's association.
	quiet, cancelQuiet := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelQuiet()
	if _, _, err := delivery.Read(quiet); err == nil {
		t.Error("the shared delivery connection received unexpected traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("the announcement closed the shared delivery connection: %v", err)
	}
}
