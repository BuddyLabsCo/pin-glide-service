//go:build integration

// Package pairing_test exercises Pairing Session creation, Sender
// authorization and the restricted pairing channel the way a client
// experiences them: HTTP and real WSS requests against the composed service, a
// disposable real PostgreSQL holding the shipped schema, and a fake WeChat
// boundary.
package pairing_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// reserveSlots claims one new_receiver session for each of count distinct
// Receivers, so the Sender holds that many live reservations, and returns each
// claim in order.
func (e *environment) reserveSlots(t *testing.T, sender bundle, count int) []sessionView {
	t.Helper()

	reservations := make([]sessionView, 0, count)
	for index := 0; index < count; index++ {
		created, _ := e.createSession(t, "new_receiver", "", "")
		status, claimed, payload := e.authorize(t, created.SessionID, "", sender.UserSession.Token)
		if status != http.StatusOK {
			t.Fatalf("claim %d status = %d: %s", index, status, payload)
		}
		reservations = append(reservations, claimed)
	}
	return reservations
}

// heldReceivers reads how many distinct Receivers one Sender holds with an
// association that still exists.
func (e *environment) heldReceivers(t *testing.T, senderID string) int64 {
	t.Helper()
	return e.count(t, `SELECT count(DISTINCT receiver_id) FROM pin_glide.pairing_pairings
		WHERE sender_id = $1 AND state <> 'removed'`, senderID)
}

// One Sender may hold at most five distinct Receivers. Active associations and
// pending reservations share the limit; contention for the last slot is
// decided atomically, an existing Receiver consumes no second slot, and a
// canceled or lapsed reservation releases its slot again.
func TestPairingCapacityIsAtomicAndReleasesOnCancellation(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-capacity", "openid-capacity")

	reserved := environment.reserveSlots(t, sender, 4)
	first, second := reserved[0], reserved[1]

	// An existing Receiver consumes no second slot. Recovery reserves no
	// association at all, so the repeated Receiver stays one of the four, and
	// the fresh owner proof its claim requires is what approves the replacement.
	environment.provider.Accept("code-recover", identity.Identity{AppID: "wx-app", OpenID: "openid-capacity"})
	recovery, restricted := environment.createRecovery(t, first.ReceiverID)
	if restricted == "" {
		t.Error("recovery session returned no restricted credential for its receiver side")
	}
	if status, _, payload := environment.claim(t, recovery.SessionID, `{}`, sender.UserSession.Token); status != http.StatusForbidden {
		t.Errorf("recovery claim without fresh owner proof = %d, want 403: %s", status, payload)
	}
	if status, claimed, payload := environment.claim(t, recovery.SessionID,
		`{"wechatCode":"code-recover"}`, sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("recovery claim status = %d: %s", status, payload)
	} else if claimed.ReceiverID != first.ReceiverID || claimed.PairingID != "" {
		t.Errorf("recovery claim = %+v, want the existing Receiver without a new Pairing", claimed)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE sender_id = $1`, sender.SenderID); count != 4 {
		t.Errorf("recovery left %d associations, want the original 4", count)
	}

	// Two concurrent claims for two distinct Receivers cannot both take the
	// last free slot.
	left, _ := environment.createSession(t, "new_receiver", "", "")
	right, _ := environment.createSession(t, "new_receiver", "", "")

	var wait sync.WaitGroup
	statuses := make([]int, 2)
	for index, session := range []sessionView{left, right} {
		wait.Add(1)
		go func(index int, sessionID string) {
			defer wait.Done()
			statuses[index], _, _ = environment.authorize(t, sessionID, "", sender.UserSession.Token)
		}(index, session.SessionID)
	}
	wait.Wait()
	accepted := 0
	for _, status := range statuses {
		if status == http.StatusOK {
			accepted++
		} else if status != http.StatusConflict {
			t.Errorf("concurrent claim status = %d, want 200 or 409", status)
		}
	}
	if accepted != 1 {
		t.Fatalf("concurrent claims accepted %d, want exactly 1", accepted)
	}

	// The Sender is at its limit now, so a sixth distinct Receiver is refused
	// until a reservation is released.
	vacated, _ := environment.createSession(t, "new_receiver", "", "")
	if status, _, payload := environment.authorize(t, vacated.SessionID, "", sender.UserSession.Token); status != http.StatusConflict {
		t.Fatalf("sixth association status = %d, want 409: %s", status, payload)
	}

	// A reservation both clients confirmed still holds its slot until its
	// Pairing activates, so the limit stays full while second is pending.
	environment.exec(t, `UPDATE pin_glide.pairing_sessions SET state = 'credential_ready' WHERE id = $1`, second.SessionID)
	if status, _, payload := environment.authorize(t, vacated.SessionID, "", sender.UserSession.Token); status != http.StatusConflict {
		t.Fatalf("a confirmed reservation released its slot: %d %s", status, payload)
	}

	// Cancelling a claimed unfinished session releases its reservation, so the
	// refused Receiver can be paired afterwards.
	if status, payload := environment.request(t, http.MethodDelete,
		"/v1/pairing-sessions/"+first.SessionID, "", sender.UserSession.Token); status != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204: %s", status, payload)
	}
	if status, _, payload := environment.authorize(t, vacated.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim after release status = %d: %s", status, payload)
	}

	// A reservation whose session lapses releases its slot as well, so the
	// next distinct Receiver can be paired without any cleanup work.
	lapsed, _ := environment.createSession(t, "new_receiver", "", "")
	if status, _, payload := environment.authorize(t, lapsed.SessionID, "", sender.UserSession.Token); status != http.StatusConflict {
		t.Fatalf("sixth association status = %d, want 409: %s", status, payload)
	}
	environment.exec(t, `UPDATE pin_glide.pairing_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, second.SessionID)
	afterExpiry, _ := environment.createSession(t, "new_receiver", "", "")
	if status, _, payload := environment.authorize(t, afterExpiry.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim after expiry status = %d, want a released slot: %s", status, payload)
	}
	if status, _, payload := environment.authorize(t, lapsed.SessionID, "", sender.UserSession.Token); status != http.StatusConflict {
		t.Errorf("claim with the limit full again = %d, want 409: %s", status, payload)
	}

	// A cancelled session stays closed for its former participants.
	if status, payload := environment.post(t, "/v1/pairing-sessions/"+first.SessionID+"/authorize", `{}`, sender.UserSession.Token); status != http.StatusGone {
		t.Errorf("cancelled session claim = %d, want 410: %s", status, payload)
	}
}

// A Sender already holding five Receivers can still re-pair one of them: the
// replacement reserves its own Pairing ID next to the working association and
// consumes no second slot. Attaching to a sixth distinct Receiver is refused
// even for a Receiver of the same Account.
func TestPairingAnExistingReceiverNeedsNoFreeSlot(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-reuse", "openid-reuse")
	assistant := environment.sender(t, "code-reuse-assistant", "openid-reuse")

	held, issued := environment.paired(t, sender, "Phone")
	environment.reserveSlots(t, sender, 4)

	replacement, _ := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	status, replaced, payload := environment.authorize(t, replacement.SessionID, "", sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("re-pairing a held Receiver at the limit = %d, want 200: %s", status, payload)
	}
	if replaced.PairingID == held.PairingID || replaced.PairingID == "" {
		t.Errorf("replacement pairing = %q, want a Pairing ID of its own next to %q", replaced.PairingID, held.PairingID)
	}
	if count := environment.heldReceivers(t, sender.SenderID); count != 5 {
		t.Errorf("re-pairing left %d distinct Receivers, want the original 5", count)
	}
	if existing := environment.associationOf(t, held.PairingID); existing.state != "active" {
		t.Errorf("working association = %+v, want it current until the replacement commits", existing)
	}

	// The Sender's capacity is still full, so an add_sender claim for one more
	// Receiver of the same Account is refused at the boundary.
	sixth, sixthCredential := environment.paired(t, assistant, "Desk")
	offered, _ := environment.addSender(t, sixth.ReceiverID, sixthCredential.ReceiverToken)
	status, _, payload = environment.authorize(t, offered.SessionID, "", sender.UserSession.Token)
	if status != http.StatusConflict {
		t.Fatalf("add_sender at the limit = %d, want 409: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "receiver_limit_reached" {
		t.Errorf("add_sender at the limit code = %q, want receiver_limit_reached", code)
	}
}

// A Receiver whose only association with a Sender is still an unfinished
// reservation is already counted, so a second claim for that same Receiver is
// admitted at the Sender's limit while a sixth distinct Receiver stays refused.
func TestPendingReservationForOneReceiverCountsOnce(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-once", "openid-once")
	assistant := environment.sender(t, "code-once-assistant", "openid-once")

	// Four unfinished reservations leave the Sender one free slot.
	environment.reserveSlots(t, sender, 4)

	// A Receiver of the same Account arrives for the fifth slot through the
	// add_sender mode, so its own reservation is the only association this
	// Sender holds with it.
	held, issued := environment.paired(t, assistant, "Desk")
	first, _ := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	if status, claimed, payload := environment.authorize(t, first.SessionID, "", sender.UserSession.Token); status != http.StatusOK {
		t.Fatalf("claim of the fifth Receiver = %d, want 200: %s", status, payload)
	} else if claimed.PairingID == "" {
		t.Fatalf("claim of the fifth Receiver = %+v, want a reserved Pairing ID", claimed)
	}

	second, _ := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	status, claimed, payload := environment.authorize(t, second.SessionID, "", sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("second claim of the same Receiver at the limit = %d, want 200: %s", status, payload)
	}
	if claimed.PairingID == "" {
		t.Fatalf("second claim = %+v, want a reserved Pairing ID", claimed)
	}
	if count := environment.heldReceivers(t, sender.SenderID); count != 5 {
		t.Errorf("a repeated Receiver left %d distinct Receivers, want 5", count)
	}

	// The limit still bites: a sixth distinct Receiver is refused.
	sixth, _ := environment.createSession(t, "new_receiver", "", "")
	status, _, payload = environment.authorize(t, sixth.SessionID, "", sender.UserSession.Token)
	if status != http.StatusConflict {
		t.Fatalf("sixth distinct Receiver = %d, want 409: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "receiver_limit_reached" {
		t.Errorf("sixth distinct Receiver code = %q, want receiver_limit_reached", code)
	}
}
