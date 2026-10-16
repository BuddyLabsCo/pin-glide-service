//go:build integration

// Scheduled cleanup at its validity boundaries: the clocks that keep live
// credentials and ordering guards, pg_cron's native nullable run-history
// clock, and what a run does after an interrupted predecessor.
package privacy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestScheduledCleanupPreservesLiveFamilyAndGuardClocks shows the chosen clocks
// doing their job: a live renewal credential survives cleanup and still rotates
// its family, the ordering guard of that rotation survives while the token it
// consumed is valid, and both a credential and a guard exactly at the cutoff
// survive the strict comparison while one second older does not.
func TestScheduledCleanupPreservesLiveFamilyAndGuardClocks(t *testing.T) {
	environment := start(t)
	environment.applyDataScripts(t)
	reference := time.Now().UTC()

	sender := environment.sender(t, "code-clock", "openid-clock")

	// Time passes: the three-hour access credentials are long past any window,
	// while the renewal credential's sliding seven-day lifetime is still live.
	environment.exec(t, `
UPDATE pin_glide.identity_credentials
SET issued_at = $2, expires_at = $2
WHERE sender_id = $1 AND scope <> 'refresh'`, sender.SenderID, reference.AddDate(0, 0, -8))

	if removed := environment.runCleanup(t, reference); removed != 2 {
		t.Errorf("cleanup removed %d rows, want the 2 expired access credentials", removed)
	}

	// The live renewal credential survived, so the family still rotates.
	status, payload := environment.request(t, http.MethodPost, "/v1/auth/refresh",
		`{"refreshToken":"`+sender.Refresh.Token+
			`","operationId":"live-operation","attempt":0}`, "")
	if status != http.StatusOK {
		t.Fatalf("refresh after cleanup status = %d: %s", status, payload)
	}
	var rotated bundle
	if err := json.Unmarshal(payload, &rotated); err != nil {
		t.Fatalf("decode the rotated bundle: %v: %s", err, payload)
	}
	if rotated.SenderID != sender.SenderID || rotated.Refresh.Token == "" {
		t.Errorf("the rotated bundle = %+v, want a fresh bundle of the same Sender", rotated)
	}

	// The rotation recorded one ordering guard, and that guard ends with the
	// consumed token's expiry, so cleanup keeps it while a retransmission of
	// that operation could still be recovered, and removes nothing else.
	if guards := environment.count(t, `
SELECT count(*) FROM pin_glide.identity_refresh_operations
WHERE sender_id = $1 AND operation_id = 'live-operation'`, sender.SenderID); guards != 1 {
		t.Fatalf("the rotation recorded %d ordering guards, want 1", guards)
	}
	if removed := environment.runCleanup(t, reference); removed != 0 {
		t.Errorf("cleanup removed %d rows while every retained clock was valid", removed)
	}

	// The strict comparison at the cutoff: the guard whose consumed token
	// expires exactly at the cutoff survives, the one a second past it does not.
	cutoff := reference.AddDate(0, 0, -7)
	environment.exec(t, `
INSERT INTO pin_glide.identity_refresh_operations
    (sender_id, operation_id, attempt, parent_digest, expires_at, committed_at)
VALUES ($1, 'operation-at-cutoff', 0, 'parent-at-cutoff', $2, $3),
       ($1, 'operation-past-cutoff', 0, 'parent-past-cutoff', $4, $3)`,
		sender.SenderID, cutoff, reference.AddDate(0, 0, -30), cutoff.Add(-time.Second))
	if removed := environment.runCleanup(t, reference); removed != 1 {
		t.Errorf("cleanup removed %d rows, want the guard one second past the cutoff", removed)
	}
	for operation, want := range map[string]int64{
		"operation-at-cutoff":   1,
		"operation-past-cutoff": 0,
	} {
		if kept := environment.count(t, `
SELECT count(*) FROM pin_glide.identity_refresh_operations
WHERE sender_id = $1 AND operation_id = $2`, sender.SenderID, operation); kept != want {
			t.Errorf("the guard %q has %d rows after cleanup, want %d", operation, kept, want)
		}
	}
}

// TestScheduledCleanupCoversNullableCronHistoryAndRetriesWithoutCheckpoints
// shows pg_cron's own run history filtered by its native nullable end clock:
// a finished run past the thirty-day window is deleted without adding a column
// or a restore hook, a finished run inside the window is kept, and a run that
// is still going has no end clock and is never deleted. It also shows what an
// interrupted run leaves behind: nothing, so the next scheduled run filters the
// same windows again and finishing the work needs no checkpoint.
func TestScheduledCleanupCoversNullableCronHistoryAndRetriesWithoutCheckpoints(t *testing.T) {
	environment := start(t)
	environment.applyDataScripts(t)
	reference := time.Now().UTC()

	environment.exec(t, `
INSERT INTO cron.job_run_details
    (jobid, database, username, command, status, start_time, end_time)
VALUES
    (1, current_database(), current_user, 'retention-finished-old',
        'succeeded', $1, $1),
    (1, current_database(), current_user, 'retention-finished-fresh',
        'succeeded', $2, $2),
    (1, current_database(), current_user, 'retention-running',
        'running', $1, NULL)`,
		reference.AddDate(0, 0, -31), reference.AddDate(0, 0, -29))

	senderID, receiverID, pairingID := environment.registrations(t)

	// An interrupted cleanup rolls back its whole transaction, so nothing was
	// half-deleted and there is no progress state to recover.
	tx, err := environment.pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin the interrupted cleanup: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		`SELECT pin_glide.pin_glide_fn_cleanup_expired_rows($1)`, reference); err != nil {
		_ = tx.Rollback()
		t.Fatalf("run the interrupted cleanup: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("roll back the interrupted cleanup: %v", err)
	}
	if kept := environment.count(t, `
SELECT count(*) FROM cron.job_run_details
WHERE command = 'retention-finished-old'`); kept != 1 {
		t.Errorf("the interrupted cleanup left %d finished cron rows, want the 1 it found", kept)
	}

	// The next run filters the same windows again, so work that became eligible
	// after the interrupted run is deleted without any checkpoint.
	environment.exec(t, `
INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at, completed_at)
VALUES ('delivery-missed', $1, 'message-missed', $2, $3, 'offline', $4, $4)`,
		senderID, receiverID, pairingID, reference.AddDate(0, 0, -8))
	if removed := environment.runCleanup(t, reference); removed != 2 {
		t.Errorf("the retried cleanup removed %d rows, want the 2 outside their windows", removed)
	}
	if removed := environment.runCleanup(t, reference); removed != 0 {
		t.Errorf("a rerun with nothing eligible removed %d rows, want a safe no-op", removed)
	}

	for name, query := range map[string]string{
		"finished run inside its window": `
SELECT count(*) FROM cron.job_run_details
WHERE command = 'retention-finished-fresh'`,
		"run that is still going": `
SELECT count(*) FROM cron.job_run_details
WHERE command = 'retention-running' AND end_time IS NULL`,
	} {
		if kept := environment.count(t, query); kept != 1 {
			t.Errorf("%d %s rows survived cleanup, want the 1 it started with", kept, name)
		}
	}
	if expired := environment.count(t, `
SELECT count(*) FROM cron.job_run_details
WHERE command = 'retention-finished-old'`); expired != 0 {
		t.Errorf("cleanup kept %d finished cron rows past their window", expired)
	}
}
