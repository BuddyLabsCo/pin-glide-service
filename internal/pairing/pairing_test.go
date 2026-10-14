package pairing

import (
	"errors"
	"testing"
	"time"
)

// A session that passed its absolute lifetime reports itself expired, refuses
// relay and cancellation, and reports the expired state to a client even
// though its stored row still records earlier progress.
func TestSessionExpiryWinsOverStoredProgress(t *testing.T) {
	created := time.Now().UTC()
	session := Session{
		ID:         "session-expired",
		Mode:       ModeNewReceiver,
		ReceiverID: "receiver-a",
		State:      StateAwaitingConfirmation,
		CreatedAt:  created,
		ExpiresAt:  created.Add(SessionLifetime),
	}

	if session.Expired(created) {
		t.Fatal("a fresh session reports itself expired")
	}
	if !session.Live(created) {
		t.Error("a claimed session cannot continue")
	}
	if err := session.TerminalError(created); err != nil {
		t.Errorf("a live session reports %v", err)
	}

	later := created.Add(SessionLifetime)
	if !session.Expired(later) {
		t.Error("a lapsed session does not report itself expired")
	}
	if session.Live(later) {
		t.Error("a lapsed session still accepts relay or cancellation")
	}
	if err := session.TerminalError(later); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("a lapsed session reports %v, want ErrSessionExpired", err)
	}
	if view := session.View(later); view.State != StateExpired {
		t.Errorf("lapsed session state = %q, want %q", view.State, StateExpired)
	}
}

// Cancellation, supersession and completion are closed sessions rather than
// expired ones, so a caller learns which outcome it has to handle.
func TestTerminalSessionReportsItsOwnOutcome(t *testing.T) {
	now := time.Now().UTC()
	for _, terminal := range []SessionState{
		StateCanceled,
		StateSuperseded,
		StateCompleted,
		StateCredentialReady,
		StateCredentialIssued,
	} {
		session := Session{State: terminal, ExpiresAt: now.Add(SessionLifetime)}
		if err := session.TerminalError(now); !errors.Is(err, ErrSessionClosed) {
			t.Errorf("%s reports %v, want ErrSessionClosed", terminal, err)
		}
		if session.Live(now) {
			t.Errorf("%s still accepts relay or cancellation", terminal)
		}
	}
}

// The published view carries the claim and the reserved Pairing ID only once
// they exist, so a client can tell an unclaimed session from a claimed one
// without reading a credential.
func TestSessionViewReportsClaimAndReservation(t *testing.T) {
	now := time.Now().UTC()
	unclaimed := Session{
		ID:         "session-a",
		Mode:       ModeNewReceiver,
		ReceiverID: "receiver-a",
		State:      StateAwaitingAuthorization,
		ExpiresAt:  now.Add(SessionLifetime),
	}
	view := unclaimed.View(now)
	if view.SenderID != "" || view.PairingID != "" {
		t.Errorf("unclaimed view = %+v, want no claim and no reservation", view)
	}
	if view.SenderConfirmed || view.ReceiverConfirmed {
		t.Errorf("unclaimed view reports a confirmation: %+v", view)
	}

	claimedAt := now
	claimed := unclaimed
	claimed.State = StateAwaitingConfirmation
	claimed.AuthorizedSenderID = "sender-a"
	claimed.PendingPairingID = "pairing-a"
	claimed.SenderConfirmedAt = &claimedAt
	view = claimed.View(now)
	if view.SenderID != "sender-a" || view.PairingID != "pairing-a" {
		t.Errorf("claimed view = %+v, want the claim and reservation", view)
	}
	if !view.SenderConfirmed || view.ReceiverConfirmed {
		t.Errorf("claimed view confirmations = %+v, want only the Sender side", view)
	}
}
