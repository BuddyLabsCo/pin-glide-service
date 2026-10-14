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

// One Sender may hold at most five distinct Receivers. Active associations and
// pending reservations share the limit; contention for the last slot is
// decided atomically, an existing Receiver consumes no second slot, and a
// canceled or lapsed reservation releases its slot again.
func TestPairingCapacityIsAtomicAndReleasesOnCancellation(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-capacity", "openid-capacity")

	var first, second sessionView
	for index := 0; index < 4; index++ {
		created, _ := environment.createSession(t, "new_receiver", "", "")
		status, claimed, payload := environment.authorize(t, created.SessionID, "", sender.UserSession.Token)
		if status != http.StatusOK {
			t.Fatalf("claim %d status = %d: %s", index, status, payload)
		}
		switch index {
		case 0:
			first = claimed
		case 1:
			second = claimed
		}
	}

	// An existing Receiver consumes no second slot. Recovery reserves no
	// association at all, so the repeated Receiver stays one of the four.
	environment.provider.Accept("code-recover", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-capacity"})
	if status, payload := environment.post(t, "/v1/pairing-sessions",
		`{"mode":"recover_receiver","receiverId":"`+first.ReceiverID+`"}`, ""); status != http.StatusForbidden {
		t.Fatalf("recovery without fresh owner proof = %d, want 403: %s", status, payload)
	}
	recovery, restricted := environment.createRecovery(t, first.ReceiverID, "code-recover")
	if restricted == "" {
		t.Error("recovery session returned no restricted credential for its receiver side")
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
