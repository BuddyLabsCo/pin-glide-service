//go:build integration

package pairing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// associationView is the decoded ReceiverAssociation one Sender receives for
// its own associations.
type associationView struct {
	ReceiverID     string    `json:"receiverId"`
	PairingID      string    `json:"pairingId"`
	Alias          string    `json:"alias"`
	Presence       string    `json:"presence"`
	StateChangedAt time.Time `json:"stateChangedAt"`
}

// listOwnReceivers reads one Sender's own Receiver list.
func (e *environment) listOwnReceivers(t *testing.T, credential string) (int, []associationView, []byte) {
	t.Helper()

	status, payload := e.request(t, http.MethodGet, "/v1/self/receivers", "", credential)
	if status != http.StatusOK {
		return status, nil, payload
	}
	var list struct {
		Items []associationView `json:"items"`
	}
	if err := json.Unmarshal(payload, &list); err != nil {
		t.Fatalf("decode Receiver list: %v: %s", err, payload)
	}
	return status, list.Items, payload
}

// renameOwnPairing posts one alias change for a Pairing and decodes the
// association it reports.
func (e *environment) renameOwnPairing(t *testing.T, pairingID, alias, credential string) (int, associationView, []byte) {
	t.Helper()

	status, payload := e.request(t, http.MethodPatch, "/v1/self/pairings/"+pairingID,
		fmt.Sprintf(`{"alias":%q}`, alias), credential)
	if status != http.StatusOK {
		return status, associationView{}, payload
	}
	var association associationView
	if err := json.Unmarshal(payload, &association); err != nil {
		t.Fatalf("decode renamed association: %v: %s", err, payload)
	}
	return status, association, payload
}

// removeOwnPairing issues one authenticated association removal.
func (e *environment) removeOwnPairing(t *testing.T, pairingID, credential string) (int, []byte) {
	t.Helper()

	return e.request(t, http.MethodDelete, "/v1/self/pairings/"+pairingID, "", credential)
}

// A Sender manages only its own associations: the list states its own aliases
// and the Receiver's presence, renaming touches its own alias alone, and
// removing one association withdraws only its own forwarding authority while
// the shared Receiver credential, its delivery connection and the other
// Sender's association keep working. Every credential that is not management
// authority, and every association the caller does not own, authorizes nothing.
func TestOwnAssociationListRenameAndRemovalRespectScope(t *testing.T) {
	environment := start(t)
	first := environment.sender(t, "code-assoc-first", "openid-assoc")
	second := environment.sender(t, "code-assoc-second", "openid-assoc")
	foreign := environment.sender(t, "code-assoc-foreign", "openid-assoc-foreign")

	held, issued := environment.paired(t, first, "Phone")
	delivery := receiverConnected(t, environment, issued.ReceiverToken)

	created, token := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	if status, _, payload := environment.authorize(t, created.SessionID, "Laptop", second.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim of the shared Receiver = %d: %s", status, payload)
	}
	if status, _, payload := environment.complete(t, created.SessionID, second.UserSession.Token); status != http.StatusOK {
		t.Fatalf("Sender report = %d: %s", status, payload)
	}
	status, attached, payload := environment.complete(t, created.SessionID, token)
	if status != http.StatusOK || attached.State != "completed" {
		t.Fatalf("shared attachment = %d %+v: %s", status, attached, payload)
	}
	if attached.PairingID == held.PairingID {
		t.Fatalf("the second Sender received the first Sender's Pairing %q", held.PairingID)
	}

	// Each Sender reads its own association only, with its own alias and the
	// Receiver's presence published by this process run's connection.
	status, firstList, payload := environment.listOwnReceivers(t, first.UserSession.Token)
	if status != http.StatusOK || len(firstList) != 1 {
		t.Fatalf("first Sender's list = %d %+v, want one own association: %s", status, firstList, payload)
	}
	own := firstList[0]
	if own.ReceiverID != held.ReceiverID || own.PairingID != held.PairingID || own.Alias != "Phone" {
		t.Errorf("first Sender's association = %+v, want the owned %q alias \"Phone\"", own, held.PairingID)
	}
	if own.Presence != "online" || own.StateChangedAt.IsZero() {
		t.Errorf("first Sender's association presence = %+v, want an online Receiver with a state-change time", own)
	}

	status, secondList, payload := environment.listOwnReceivers(t, second.UserSession.Token)
	if status != http.StatusOK || len(secondList) != 1 || secondList[0].PairingID != attached.PairingID || secondList[0].Alias != "Laptop" {
		t.Fatalf("second Sender's list = %d %+v, want only its own Laptop association: %s", status, secondList, payload)
	}
	if secondList[0].ReceiverID != held.ReceiverID {
		t.Errorf("second Sender's Receiver = %q, want the shared %q", secondList[0].ReceiverID, held.ReceiverID)
	}

	status, foreignList, payload := environment.listOwnReceivers(t, foreign.UserSession.Token)
	if status != http.StatusOK || len(foreignList) != 0 {
		t.Fatalf("another Account's list = %d %+v, want no associations: %s", status, foreignList, payload)
	}

	// Only management authority manages associations: an absent credential, the
	// upload scope, the Receiver credential and the restricted pairing
	// credential all authorize nothing here.
	for _, refused := range []struct {
		name       string
		credential string
		code       string
	}{
		{name: "no credential", code: "credential_missing"},
		{name: "unknown credential", credential: "credential-that-was-never-issued", code: "credential_invalid"},
		{name: "upload credential", credential: first.Uploader.Token, code: "credential_invalid"},
		{name: "receiver credential", credential: issued.ReceiverToken, code: "credential_invalid"},
		{name: "restricted pairing credential", credential: token, code: "credential_invalid"},
	} {
		if status, payload := environment.request(t, http.MethodGet, "/v1/self/receivers", "", refused.credential); status != http.StatusUnauthorized {
			t.Errorf("%s list status = %d, want 401: %s", refused.name, status, payload)
		} else if code := decodeProblem(t, payload).Code; code != refused.code {
			t.Errorf("%s list code = %q, want %q", refused.name, code, refused.code)
		}
		if status, payload := environment.request(t, http.MethodPatch, "/v1/self/pairings/"+held.PairingID,
			`{"alias":"Taken"}`, refused.credential); status != http.StatusUnauthorized {
			t.Errorf("%s rename status = %d, want 401: %s", refused.name, status, payload)
		}
		if status, payload := environment.removeOwnPairing(t, held.PairingID, refused.credential); status != http.StatusUnauthorized {
			t.Errorf("%s removal status = %d, want 401: %s", refused.name, status, payload)
		}
	}

	// Renaming changes only the owning Sender's own alias, and the association
	// it reports is the one the caller still holds.
	status, renamed, payload := environment.renameOwnPairing(t, held.PairingID, "Work phone", first.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("rename status = %d: %s", status, payload)
	}
	if renamed.PairingID != held.PairingID || renamed.ReceiverID != held.ReceiverID || renamed.Alias != "Work phone" || renamed.Presence != "online" {
		t.Errorf("renamed association = %+v, want the owned Pairing under the new alias", renamed)
	}
	if stored := environment.associationOf(t, held.PairingID); stored.alias != "Work phone" {
		t.Errorf("stored alias = %q, want \"Work phone\"", stored.alias)
	}
	if otherSecond := environment.associationOf(t, attached.PairingID); otherSecond.alias != "Laptop" {
		t.Errorf("the other Sender's alias = %q, want the unchanged \"Laptop\"", otherSecond.alias)
	}

	// A Pairing this caller does not own is invisible: another Sender's
	// association, another Account's request and an unknown ID all report the
	// same not-found outcome without changing anything.
	for _, refused := range []struct {
		name       string
		pairingID  string
		credential string
	}{
		{name: "the other Sender's Pairing", pairingID: attached.PairingID, credential: first.UserSession.Token},
		{name: "another Account's request", pairingID: held.PairingID, credential: foreign.UserSession.Token},
		{name: "an unknown Pairing", pairingID: "pairing-that-was-never-issued", credential: first.UserSession.Token},
	} {
		status, payload := environment.request(t, http.MethodPatch, "/v1/self/pairings/"+refused.pairingID,
			`{"alias":"Taken"}`, refused.credential)
		if status != http.StatusNotFound {
			t.Errorf("rename of %s = %d, want 404: %s", refused.name, status, payload)
			continue
		}
		if code := decodeProblem(t, payload).Code; code != "resource_not_found" {
			t.Errorf("rename of %s code = %q, want resource_not_found", refused.name, code)
		}
	}
	if stored := environment.associationOf(t, held.PairingID); stored.alias != "Work phone" {
		t.Errorf("a refused rename changed the alias to %q", stored.alias)
	}

	// A body without the alias field is not a rename: the published operation
	// requires it, and an empty one clears the Sender's own label.
	if status, payload := environment.request(t, http.MethodPatch, "/v1/self/pairings/"+held.PairingID, `{}`, first.UserSession.Token); status != http.StatusUnprocessableEntity {
		t.Errorf("rename without an alias = %d, want 422: %s", status, payload)
	}
	if _, cleared, payload := environment.renameOwnPairing(t, held.PairingID, "", first.UserSession.Token); cleared.Alias != "" {
		t.Errorf("alias after clearing = %q, want empty: %s", cleared.Alias, payload)
	}

	// Removing another Sender's association authorizes nothing.
	if status, payload := environment.removeOwnPairing(t, attached.PairingID, first.UserSession.Token); status != http.StatusNotFound {
		t.Fatalf("removal of the other Sender's Pairing = %d, want 404: %s", status, payload)
	}
	if stored := environment.associationOf(t, attached.PairingID); stored.state != "active" {
		t.Fatalf("the other Sender's association = %q after a refused removal, want active", stored.state)
	}

	// The second Sender's own unfinished re-pairing is not the first Sender's
	// work: removing one association leaves the other Sender's live reservation
	// for the shared Receiver alone.
	replacing, _ := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	status, liveReservation, payload := environment.authorize(t, replacing.SessionID, "", second.UserSession.Token)
	if status != http.StatusOK || liveReservation.PairingID == "" {
		t.Fatalf("the other Sender's re-pairing = %d %+v, want a live reservation: %s", status, liveReservation, payload)
	}

	// Removing the Sender's own association withdraws only its forwarding
	// authority: the shared Receiver credential, its delivery connection and
	// the other Sender's association keep working, and the Receiver is told
	// about the removal on the connection it already holds.
	if status, payload := environment.removeOwnPairing(t, held.PairingID, first.UserSession.Token); status != http.StatusNoContent {
		t.Fatalf("removal status = %d, want 204: %s", status, payload)
	}
	if status, remaining, payload := environment.listOwnReceivers(t, first.UserSession.Token); status != http.StatusOK || len(remaining) != 0 {
		t.Errorf("removed Sender's list = %d %+v, want no associations: %s", status, remaining, payload)
	}
	if removed := environment.associationOf(t, held.PairingID); removed.state != "removed" {
		t.Errorf("removed association = %q, want removed", removed.state)
	} else if count := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE id = $1`, held.PairingID); count != 1 {
		t.Errorf("the removed association kept %d rows, want its durable identity retained", count)
	}
	if stored := environment.associationOf(t, attached.PairingID); stored.state != "active" {
		t.Errorf("the other Sender's association = %q, want active", stored.state)
	}
	if status, kept, payload := environment.listOwnReceivers(t, second.UserSession.Token); status != http.StatusOK || len(kept) != 1 || kept[0].PairingID != attached.PairingID {
		t.Errorf("the other Sender's list = %d %+v, want its association kept: %s", status, kept, payload)
	}
	if state := environment.sessionState(t, replacing.SessionID); state != "awaiting_confirmation" {
		t.Errorf("the other Sender's session state = %q after the removal, want awaiting_confirmation", state)
	}
	if reservation := environment.associationOf(t, liveReservation.PairingID); reservation.state != "pending" {
		t.Errorf("the other Sender's reservation = %q after the removal, want pending", reservation.state)
	}

	assertPairingRemoved(t, delivery, first.SenderID, held.PairingID)
	receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)
	assertDeliveryStaysOpen(t, delivery)
}

// assertPairingRemoved requires the next control frame on one Receiver's
// delivery connection to announce that exactly this Sender's association was
// removed.
func assertPairingRemoved(t *testing.T, delivery *websocket.Conn, senderID, pairingID string) {
	t.Helper()

	var control struct {
		Version   int    `json:"version"`
		Type      string `json:"type"`
		SenderID  string `json:"senderId"`
		PairingID string `json:"pairingId"`
	}
	readFrame(t, delivery, &control)
	if control.Type != "pairing_removed" || control.Version != 1 ||
		control.SenderID != senderID || control.PairingID != pairingID {
		t.Errorf("control frame = %+v, want pairing_removed of %q by %q", control, pairingID, senderID)
	}
}

// assertDeliveryStaysOpen requires one delivery connection to stay open and
// silent. A read whose own context expires closes that connection, so this is
// the last interaction a test may have with the socket it checks.
func assertDeliveryStaysOpen(t *testing.T, delivery *websocket.Conn) {
	t.Helper()

	quiet, cancelQuiet := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelQuiet()
	if _, _, err := delivery.Read(quiet); err == nil {
		t.Error("the shared delivery connection received unexpected traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("the shared delivery connection closed: %v", err)
	}
}

// Removing an association cancels the Sender's unfinished work for the same
// Receiver in the same transaction: the reservation is released, both
// participants see the session closed, and the key-confirmation report a stale
// client still sends cannot recreate the association those clients removed.
func TestRemovalCancelsUnfinishedReplacementSoAStaleReportCannotRecreateIt(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-assoc-stale", "openid-assoc-stale")
	held, issued := environment.paired(t, sender, "Phone")

	// A re-pairing reserves its own Pairing ID next to the working one.
	created, token := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	status, claimed, payload := environment.authorize(t, created.SessionID, "Tablet", sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("replacement claim status = %d: %s", status, payload)
	}
	if reserved := environment.associationOf(t, claimed.PairingID); reserved.state != "pending" || reserved.replaces != held.PairingID {
		t.Fatalf("replacement reservation = %+v, want a pending reservation replacing %q", reserved, held.PairingID)
	}

	status, payload = environment.removeOwnPairing(t, held.PairingID, sender.UserSession.Token)
	if status != http.StatusNoContent {
		t.Fatalf("removal status = %d, want 204: %s", status, payload)
	}

	if state := environment.sessionState(t, created.SessionID); state != "canceled" {
		t.Errorf("unfinished session state = %q, want canceled", state)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE id = $1`, claimed.PairingID); count != 0 {
		t.Errorf("the canceled reservation kept %d rows, want it released", count)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE id = $1 AND state = 'removed' AND removed_at IS NOT NULL`, held.PairingID); count != 1 {
		t.Errorf("the removed association kept %d rows, want one removed row with its removal time", count)
	}
	if held := environment.heldReceivers(t, sender.SenderID); held != 0 {
		t.Errorf("the Sender holds %d Receivers after removal, want none", held)
	}

	// The stale reports of both participants find a closed session, and no
	// association is current again afterwards.
	status, _, payload = environment.complete(t, created.SessionID, sender.UserSession.Token)
	if status != http.StatusGone || decodeProblem(t, payload).Code != "pairing_session_closed" {
		t.Errorf("stale Sender report = %d %s, want 410 pairing_session_closed", status, payload)
	}
	status, _, payload = environment.complete(t, created.SessionID, token)
	if status != http.StatusGone || decodeProblem(t, payload).Code != "pairing_session_closed" {
		t.Errorf("stale Receiver report = %d %s, want 410 pairing_session_closed", status, payload)
	}
	if current := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE sender_id = $1 AND receiver_id = $2 AND state = 'active'`,
		sender.SenderID, held.ReceiverID); current != 0 {
		t.Errorf("a stale report recreated %d active associations, want none", current)
	}
}

// The Sender's own Receiver list states the Receiver's current presence and
// when that state last changed, so a Sender follows a Receiver that connects,
// disconnects and connects again without polling the delivery channel.
func TestOwnReceiverListReportsPresenceTransitions(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-assoc-presence", "openid-assoc-presence")
	ready, token := environment.readySession(t, sender)
	status, issued, payload := environment.redeem(t, ready.SessionID, token, "op-assoc-presence", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}

	offline := func() associationView {
		t.Helper()
		status, items, payload := environment.listOwnReceivers(t, sender.UserSession.Token)
		if status != http.StatusOK || len(items) != 1 {
			t.Fatalf("Receiver list = %d %+v: %s", status, items, payload)
		}
		if items[0].Presence != "offline" {
			t.Fatalf("Receiver presence = %+v, want offline", items[0])
		}
		return items[0]
	}

	first := offline()
	if first.StateChangedAt.IsZero() || first.StateChangedAt.After(time.Now().UTC()) {
		t.Errorf("offline state changed at %v, want the process run's start", first.StateChangedAt)
	}

	receiver := receiverConnected(t, environment, issued.ReceiverToken)
	receiverPresence(t, environment.service, issued.ReceiverID, true, 5*time.Second)
	status, items, payload := environment.listOwnReceivers(t, sender.UserSession.Token)
	if status != http.StatusOK || len(items) != 1 {
		t.Fatalf("Receiver list while connected = %d %+v: %s", status, items, payload)
	}
	if items[0].Presence != "online" || !items[0].StateChangedAt.After(first.StateChangedAt) {
		t.Errorf("connected presence = %+v, want online after %v", items[0], first.StateChangedAt)
	}
	onlineAt := items[0].StateChangedAt

	if err := receiver.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close delivery connection: %v", err)
	}
	receiverPresence(t, environment.service, issued.ReceiverID, false, 5*time.Second)
	reconnected := offline()
	if !reconnected.StateChangedAt.After(onlineAt) {
		t.Errorf("offline state changed at %v, want it after the online transition %v", reconnected.StateChangedAt, onlineAt)
	}
}
