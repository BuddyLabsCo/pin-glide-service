//go:build integration

// Package pairing_test exercises Receiver credential recovery the way a client
// experiences it: HTTP and real WSS requests against the composed service, a
// disposable real PostgreSQL holding the shipped schema, and a fake WeChat
// boundary.
package pairing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/coder/websocket"
)

// claimRecovery claims one recovery session with the approving Sender's
// management credential and one fresh owner proof for that Sender's Account.
func (e *environment) claimRecovery(t *testing.T, sessionID, code, openID string, sender bundle) sessionView {
	t.Helper()

	e.provider.Accept(code, identity.Identity{AppID: "wx-app", OpenID: openID})
	status, claimed, payload := e.claim(t, sessionID, `{"wechatCode":"`+code+`"}`, sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("recovery claim status = %d: %s", status, payload)
	}
	return claimed
}

// reportRecovery posts one mode-specific completion report and decodes the
// session it returns.
func (e *environment) reportRecovery(t *testing.T, sessionID, credential, body string) (int, sessionView, []byte) {
	t.Helper()

	status, payload := e.post(t, "/v1/pairing-sessions/"+sessionID+"/complete", body, credential)
	if status != http.StatusOK {
		return status, sessionView{}, payload
	}
	var view sessionView
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatalf("decode completed recovery session: %v: %s", err, payload)
	}
	return status, view, payload
}

// retainedReport is the Receiver-side report of one recovery: the Pairing IDs
// whose local keys survived, or the missing-key report when none did.
func retainedReport(pairingIDs ...string) string {
	if len(pairingIDs) == 0 {
		return `{"keyConfirmation":"not_available"}`
	}
	encoded, err := json.Marshal(pairingIDs)
	if err != nil {
		panic(err)
	}
	return `{"keyConfirmation":"confirmed","retainedPairingIds":` + string(encoded) + `}`
}

// approvedRecovery drives one recovery to credential_ready: the approving
// Sender confirms the key it still holds and the Receiver side reports the
// Pairing IDs it kept.
func (e *environment) approvedRecovery(t *testing.T, sender bundle, receiverID, code, openID string, retained ...string) (sessionView, string) {
	t.Helper()

	recovery, restricted := e.createRecovery(t, receiverID)
	e.claimRecovery(t, recovery.SessionID, code, openID, sender)

	if status, _, payload := e.reportRecovery(t, recovery.SessionID, sender.UserSession.Token,
		`{"keyConfirmation":"confirmed"}`); status != http.StatusOK {
		t.Fatalf("Sender recovery report status = %d: %s", status, payload)
	}
	status, view, payload := e.reportRecovery(t, recovery.SessionID, restricted, retainedReport(retained...))
	if status != http.StatusOK {
		t.Fatalf("Receiver recovery report status = %d: %s", status, payload)
	}
	if view.State != "credential_ready" {
		t.Fatalf("recovered session = %+v, want credential_ready", view)
	}
	return recovery, restricted
}

// attach adds one more Sender of the owning Account to a credentialed
// Receiver, running the whole add_sender flow, and returns the claimed
// session.
func (e *environment) attach(t *testing.T, sender bundle, receiverID, credential string) sessionView {
	t.Helper()

	created, token := e.addSender(t, receiverID, credential)
	status, claimed, payload := e.authorize(t, created.SessionID, "Laptop", sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("add_sender claim status = %d: %s", status, payload)
	}
	if status, _, payload := e.complete(t, created.SessionID, sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("add_sender Sender report status = %d: %s", status, payload)
	}
	if status, completed, payload := e.complete(t, created.SessionID, token); status != http.StatusOK {
		t.Fatalf("add_sender Receiver report status = %d: %s", status, payload)
	} else if completed.State != "completed" {
		t.Fatalf("add_sender session = %+v, want completed", completed)
	}
	return claimed
}

// An owner who lost R replaces it after fresh Account-owner approval while the
// Pairings whose local keys survived keep forwarding. The replacement is only
// reachable through the restricted session, the superseded credential loses
// its shared delivery connection, and pure recovery creates no Pairing.
func TestRecoveryReplacesTheCredentialAndPreservesRetainedAssociations(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-recovery", "openid-recovery")

	association, issued := environment.paired(t, sender, "Phone")
	superseded := receiverConnected(t, environment, issued.ReceiverToken)

	recovery, restricted := environment.approvedRecovery(t, sender, association.ReceiverID,
		"code-recovery-approval", "openid-recovery", association.PairingID)
	if recovery.PairingID != "" {
		t.Errorf("recovery session reserved Pairing %q, want none", recovery.PairingID)
	}

	status, replacement, payload := environment.redeem(t, recovery.SessionID, restricted, "op-recovery", 0)
	if status != http.StatusOK {
		t.Fatalf("recovery redemption status = %d: %s", status, payload)
	}
	if replacement.ReceiverID != association.ReceiverID {
		t.Errorf("replacement belongs to Receiver %q, want %q", replacement.ReceiverID, association.ReceiverID)
	}
	if replacement.ReceiverToken == issued.ReceiverToken {
		t.Error("recovery returned the credential it replaced")
	}

	// The recovery replaced the current generation and revoked the old one for
	// every Sender, so the superseded credential neither opens a new
	// connection nor keeps the shared one it already held.
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	requireClose(t, superseded, websocket.StatusPolicyViolation)
	if state := environment.service.Presence.State(association.ReceiverID); state.Online {
		t.Errorf("the Receiver after its superseded connection ended = %+v, want offline", state)
	}

	// The Pairing the Receiver still held keys for survives, and pure recovery
	// created no association of its own.
	status, associations, payload := environment.listOwnReceivers(t, sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("own Receiver list status = %d: %s", status, payload)
	}
	if len(associations) != 1 || associations[0].PairingID != association.PairingID {
		t.Errorf("associations after recovery = %+v, want only %q", associations, association.PairingID)
	}
	if associations[0].Presence != "offline" {
		t.Errorf("the preserved association reports presence %q, want offline until the replacement connects", associations[0].Presence)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE receiver_id = $1`,
		association.ReceiverID); count != 1 {
		t.Errorf("recovery left %d Pairings, want the one it preserved", count)
	}

	// The replacement reaches the delivery channel, and its possession
	// completes the recovery session.
	receiverConnected(t, environment, replacement.ReceiverToken)
	if state := environment.sessionState(t, recovery.SessionID); state != "completed" {
		t.Errorf("recovery session after possession = %q, want completed", state)
	}
	if state := environment.service.Presence.State(association.ReceiverID); !state.Online {
		t.Errorf("the Receiver after its replacement connected = %+v, want online", state)
	}
}

// A Receiver that kept no local key loses every association it had with every
// Sender, while the Senders' other Receivers keep their associations and live
// connections.
func TestRecoveryRemovesLostKeysAndSparesUnrelatedReceivers(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-owner", "openid-owner")
	second := environment.sender(t, "code-second", "openid-owner")

	lost, _ := environment.paired(t, owner, "Lost")
	shared, sharedCredential := environment.paired(t, owner, "Shared")
	environment.attach(t, second, shared.ReceiverID, sharedCredential.ReceiverToken)

	recovery, restricted := environment.approvedRecovery(t, owner, lost.ReceiverID,
		"code-owner-approval", "openid-owner")
	if status, _, payload := environment.redeem(t, recovery.SessionID, restricted, "op-lost", 0); status != http.StatusOK {
		t.Fatalf("recovery redemption status = %d: %s", status, payload)
	}

	if status, associations, payload := environment.listOwnReceivers(t, owner.UserSession.Token); status != http.StatusOK {
		t.Fatalf("own Receiver list status = %d: %s", status, payload)
	} else if len(associations) != 1 || associations[0].PairingID != shared.PairingID {
		t.Errorf("associations after losing every key = %+v, want only the unrelated %q", associations, shared.PairingID)
	}
	if state := environment.pairingState(t, lost.PairingID); state != "removed" {
		t.Errorf("lost association state = %q, want removed", state)
	}

	// The unrelated Receiver keeps its association and its live connection,
	// and every Sender that held a Pairing for it still does.
	if state := environment.pairingState(t, shared.PairingID); state != "active" {
		t.Errorf("unrelated association state = %q, want active", state)
	}
	if status, associations, _ := environment.listOwnReceivers(t, second.UserSession.Token); status != http.StatusOK || len(associations) != 1 {
		t.Errorf("the other Sender's associations after the recovery = %+v, want its own one", associations)
	}
	held := receiverConnected(t, environment, sharedCredential.ReceiverToken)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, _, err := held.Read(ctx); err == nil {
		t.Error("an unrelated Receiver received unexpected traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("an unrelated Receiver's connection was closed: %v", err)
	}
}

// Two Senders share one Receiver. The recovery preserves only the Pairings the
// Receiver still holds keys for, so the association whose key was lost is
// removed while the other Sender's association keeps forwarding.
func TestRecoveryPreservesOnlyThePairingsTheReceiverStillHolds(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-mixed-owner", "openid-mixed")
	second := environment.sender(t, "code-mixed-second", "openid-mixed")

	ownerAssociation, issued := environment.paired(t, owner, "Phone")
	secondAssociation := environment.attach(t, second, ownerAssociation.ReceiverID, issued.ReceiverToken)

	recovery, restricted := environment.approvedRecovery(t, owner, ownerAssociation.ReceiverID,
		"code-mixed-approval", "openid-mixed", secondAssociation.PairingID)
	if status, _, payload := environment.redeem(t, recovery.SessionID, restricted, "op-mixed", 0); status != http.StatusOK {
		t.Fatalf("recovery redemption status = %d: %s", status, payload)
	}

	if state := environment.pairingState(t, ownerAssociation.PairingID); state != "removed" {
		t.Errorf("the lost association = %q, want removed", state)
	}
	if state := environment.pairingState(t, secondAssociation.PairingID); state != "active" {
		t.Errorf("the retained association = %q, want active", state)
	}
	if status, associations, _ := environment.listOwnReceivers(t, owner.UserSession.Token); status != http.StatusOK || len(associations) != 0 {
		t.Errorf("the approving Sender's associations = %+v, want none", associations)
	}
	if status, associations, _ := environment.listOwnReceivers(t, second.UserSession.Token); status != http.StatusOK ||
		len(associations) != 1 || associations[0].PairingID != secondAssociation.PairingID {
		t.Errorf("the retained Sender's associations = %+v, want %q", associations, secondAssociation.PairingID)
	}
}

// The approving Sender reports the key it no longer holds, so its own
// association with the recovered Receiver goes even though the Receiver kept
// its side of that key, and every other Sender keeps its own association.
func TestRecoveryRemovesTheApproversOwnLostKey(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-lost-key", "openid-lost-key")
	second := environment.sender(t, "code-lost-key-second", "openid-lost-key")

	ownerAssociation, issued := environment.paired(t, owner, "Phone")
	secondAssociation := environment.attach(t, second, ownerAssociation.ReceiverID, issued.ReceiverToken)

	recovery, restricted := environment.createRecovery(t, ownerAssociation.ReceiverID)
	environment.claimRecovery(t, recovery.SessionID, "code-lost-key-approval", "openid-lost-key", owner)
	if status, _, payload := environment.reportRecovery(t, recovery.SessionID, owner.UserSession.Token,
		`{"keyConfirmation":"not_available"}`); status != http.StatusOK {
		t.Fatalf("Sender missing-key report = %d, want 200: %s", status, payload)
	}
	status, view, payload := environment.reportRecovery(t, recovery.SessionID, restricted,
		retainedReport(ownerAssociation.PairingID, secondAssociation.PairingID))
	if status != http.StatusOK {
		t.Fatalf("Receiver report = %d, want 200: %s", status, payload)
	}
	if view.State != "credential_ready" {
		t.Fatalf("recovery after both reports = %+v, want credential_ready", view)
	}
	if status, _, payload := environment.redeem(t, recovery.SessionID, restricted, "op-lost-key", 0); status != http.StatusOK {
		t.Fatalf("recovery redemption = %d, want 200: %s", status, payload)
	}

	if state := environment.pairingState(t, ownerAssociation.PairingID); state != "removed" {
		t.Errorf("the approving Sender's association = %q, want removed", state)
	}
	if state := environment.pairingState(t, secondAssociation.PairingID); state != "active" {
		t.Errorf("the other Sender's association = %q, want active", state)
	}
	if status, associations, _ := environment.listOwnReceivers(t, owner.UserSession.Token); status != http.StatusOK || len(associations) != 0 {
		t.Errorf("the approving Sender's associations = %+v, want none", associations)
	}
	if status, associations, _ := environment.listOwnReceivers(t, second.UserSession.Token); status != http.StatusOK ||
		len(associations) != 1 || associations[0].PairingID != secondAssociation.PairingID {
		t.Errorf("the other Sender's associations = %+v, want %q", associations, secondAssociation.PairingID)
	}
}

// A Pairing the Receiver lost takes the unfinished work that reserved one of
// its lost keys with it, so a later key confirmation cannot activate an
// association whose key is gone. The other Sender must pair again.
func TestRecoveryCancelsUnfinishedWorkForLostKeys(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-lost-work", "openid-lost-work")
	second := environment.sender(t, "code-lost-work-second", "openid-lost-work")

	ownerAssociation, issued := environment.paired(t, owner, "Phone")
	secondAssociation := environment.attach(t, second, ownerAssociation.ReceiverID, issued.ReceiverToken)

	// The other Sender reserves a replacement without reporting it yet, so the
	// reservation is live work whose provisional key the Receiver can still
	// lose.
	reservation, _ := environment.addSender(t, ownerAssociation.ReceiverID, issued.ReceiverToken)
	status, claimed, payload := environment.authorize(t, reservation.SessionID, "Work", second.UserSession.Token)
	if status != http.StatusOK || claimed.PairingID == "" || claimed.PairingID == secondAssociation.PairingID {
		t.Fatalf("reserved replacement = %d %+v: %s", status, claimed, payload)
	}

	recovery, restricted := environment.approvedRecovery(t, owner, ownerAssociation.ReceiverID,
		"code-lost-work-approval", "openid-lost-work", ownerAssociation.PairingID)
	if status, _, payload := environment.redeem(t, recovery.SessionID, restricted, "op-lost-work", 0); status != http.StatusOK {
		t.Fatalf("recovery redemption = %d, want 200: %s", status, payload)
	}

	if state := environment.pairingState(t, ownerAssociation.PairingID); state != "active" {
		t.Errorf("the retained association = %q, want active", state)
	}
	if state := environment.pairingState(t, secondAssociation.PairingID); state != "removed" {
		t.Errorf("the lost association = %q, want removed", state)
	}
	if state := environment.sessionState(t, reservation.SessionID); state != "canceled" {
		t.Errorf("the reservation's session = %q, want canceled", state)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE id = $1`,
		claimed.PairingID); count != 0 {
		t.Errorf("the lost reservation left %d Pairings, want none", count)
	}
	if status, associations, _ := environment.listOwnReceivers(t, second.UserSession.Token); status != http.StatusOK || len(associations) != 0 {
		t.Errorf("the other Sender's associations = %+v, want none", associations)
	}
	if status, _, payload := environment.complete(t, reservation.SessionID, second.UserSession.Token); status != http.StatusGone {
		t.Errorf("the canceled reservation's report = %d, want 410: %s", status, payload)
	}
}
