//go:build integration

// Package pairing_test exercises attaching one more Sender to an already
// credentialed Receiver and replacing one Sender's own Pairing the way a
// client experiences them: HTTP and real WSS requests against the composed
// service, a disposable real PostgreSQL holding the shipped schema, and a fake
// WeChat boundary.
package pairing_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// A stale replacement is decided by commit order rather than request order:
// two replacements of the same association whose last reports race can only
// produce one current Pairing, so the loser releases its own reservation and
// reports operation_superseded while the shared Receiver access stays on the
// association that won.
func TestConcurrentReplacementsCommitOnlyOneAssociation(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-replacement-race", "openid-replacement-race")
	held, issued := environment.paired(t, sender, "Phone")

	left, leftToken := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	right, rightToken := environment.addSender(t, held.ReceiverID, issued.ReceiverToken)
	sessions := []sessionView{left, right}
	tokens := []string{leftToken, rightToken}
	claims := make([]sessionView, len(sessions))
	for index, session := range sessions {
		status, claimed, payload := environment.authorize(t, session.SessionID, "", sender.UserSession.Token)
		if status != http.StatusOK {
			t.Fatalf("claim %d status = %d: %s", index, status, payload)
		}
		claims[index] = claimed
	}

	// Every participant holds its own live channel while the last report of
	// each replacement races, so both sides of both replacements observe the
	// interleaving of the two commits.
	senders := []*websocket.Conn{
		environment.dial(t, left.SessionID, sender.UserSession.Token),
		environment.dial(t, right.SessionID, sender.UserSession.Token),
	}
	receivers := []*websocket.Conn{
		environment.dial(t, left.SessionID, leftToken),
		environment.dial(t, right.SessionID, rightToken),
	}
	sockets := append(append([]*websocket.Conn{}, senders...), receivers...)
	for _, socket := range sockets {
		var frame struct {
			Type    string      `json:"type"`
			Session sessionView `json:"session"`
		}
		readFrame(t, socket, &frame)
		if frame.Type != "pairing_state" {
			t.Fatalf("opening frame type = %q, want the subscription's pairing_state", frame.Type)
		}
	}

	// One report per replacement leaves both one confirmation away from
	// committing, so only their confirming commits can race.
	for index, session := range sessions {
		if status, _, payload := environment.complete(t, session.SessionID, sender.UserSession.Token); status != http.StatusOK {
			t.Fatalf("Sender report %d status = %d: %s", index, status, payload)
		}
	}

	statuses := make([]int, len(sessions))
	problems := make([][]byte, len(sessions))
	var wait sync.WaitGroup
	for index, session := range sessions {
		wait.Add(1)
		go func(index int, sessionID, token string) {
			defer wait.Done()
			statuses[index], _, problems[index] = environment.complete(t, sessionID, token)
		}(index, session.SessionID, tokens[index])
	}
	wait.Wait()

	winner, loser := -1, -1
	committed, stale := 0, 0
	for index, status := range statuses {
		switch status {
		case http.StatusOK:
			committed, winner = committed+1, index
		case http.StatusConflict:
			stale, loser = stale+1, index
			if code := decodeProblem(t, problems[index]).Code; code != "operation_superseded" {
				t.Errorf("losing replacement %d code = %q, want operation_superseded", index, code)
			}
		default:
			t.Errorf("racing report %d = %d, want 200 or 409: %s", index, status, problems[index])
		}
	}
	if committed != 1 || stale != 1 {
		t.Fatalf("the race committed %d and superseded %d replacements, want exactly one of each", committed, stale)
	}

	if state := environment.sessionState(t, sessions[loser].SessionID); state != "superseded" {
		t.Errorf("losing session state = %q, want superseded", state)
	}
	if count := environment.count(t,
		`SELECT count(*) FROM pin_glide.pairing_pairings WHERE id = $1`, claims[loser].PairingID); count != 0 {
		t.Errorf("losing replacement left %d reservations, want it released", count)
	}
	if state := environment.associationOf(t, claims[winner].PairingID).state; state != "active" {
		t.Errorf("winning Pairing state = %q, want active", state)
	}
	if state := environment.associationOf(t, held.PairingID).state; state != "removed" {
		t.Errorf("replaced association state = %q, want removed", state)
	}
	if count := environment.count(t,
		`SELECT count(*) FROM pin_glide.pairing_pairings
		WHERE sender_id = $1 AND receiver_id = $2 AND state = 'active'`,
		sender.SenderID, held.ReceiverID); count != 1 {
		t.Errorf("active associations after the race = %d, want the winner only", count)
	}

	// Both sides of the winning session follow the commit, and both sides of
	// the losing one end on the state that made its channel unusable.
	for _, socket := range []*websocket.Conn{senders[winner], receivers[winner]} {
		if state := replacementOutcome(t, socket); state != "completed" {
			t.Errorf("winning channel state = %q, want completed", state)
		}
	}
	for _, socket := range []*websocket.Conn{senders[loser], receivers[loser]} {
		if state := replacementOutcome(t, socket); state != "superseded" {
			t.Errorf("losing channel state = %q, want superseded", state)
		}
	}
}

// replacementOutcome reads one channel until the session reports the state the
// race committed, so a change still queued for that participant is not
// mistaken for the settled one.
func replacementOutcome(t *testing.T, socket *websocket.Conn) string {
	t.Helper()

	for {
		var frame struct {
			Type    string      `json:"type"`
			Session sessionView `json:"session"`
		}
		readFrame(t, socket, &frame)
		if frame.Type == "pairing_state" &&
			(frame.Session.State == "completed" || frame.Session.State == "superseded") {
			return frame.Session.State
		}
	}
}
