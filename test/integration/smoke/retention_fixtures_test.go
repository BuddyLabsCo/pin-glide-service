//go:build integration

package smoke_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

func TestRetentionProbeJobRunsCleanupAndRemovesItself(t *testing.T) {
	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")
	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open disposable database: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	sql := generateSmokeSQL(t)
	const jobName = "smoke-integration-retention-probe"
	var jobID int64
	if err := pool.QueryRowContext(context.Background(),
		`SELECT cron.schedule($1, '1 second', $2)`, jobName, sql.RetentionProbe).
		Scan(&jobID); err != nil {
		t.Fatalf("schedule retention probe: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		var jobs, succeeded int
		if err := pool.QueryRowContext(context.Background(),
			`SELECT count(*) FROM cron.job WHERE jobid = $1`, jobID).Scan(&jobs); err != nil {
			t.Fatalf("read retention probe job: %v", err)
		}
		if err := pool.QueryRowContext(context.Background(), `
SELECT count(*) FROM cron.job_run_details
WHERE jobid = $1 AND status = 'succeeded' AND end_time IS NOT NULL`, jobID).
			Scan(&succeeded); err != nil {
			t.Fatalf("read retention probe runs: %v", err)
		}
		if jobs == 0 && succeeded > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("retention probe job=%d succeededRuns=%d did not self-remove", jobs, succeeded)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRetentionFixtureSQLInsertsAndRemovesOnlyRunOwnedRows(t *testing.T) {
	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")
	postgres.ApplySchema(t, "release")
	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open disposable database: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	sql := generateSmokeSQL(t)
	execSQL(t, pool, sql.Bootstrap)
	execSQL(t, pool, `
INSERT INTO pin_glide.identity_receiver_registrations (id, account_id, state)
VALUES ('smoke-integration-retention-receiver', $1, 'active')`, sql.FirstAccountID)
	execSQL(t, pool, `
INSERT INTO pin_glide.identity_receiver_credentials
    (receiver_id, generation, token_digest)
VALUES ('smoke-integration-retention-receiver', 1, repeat('d', 64))`)
	execSQL(t, pool, `
INSERT INTO pin_glide.pairing_pairings
    (id, sender_id, receiver_id, alias, state, activated_at)
VALUES ('smoke-integration-retention-pairing', $1,
        'smoke-integration-retention-receiver', '', 'active', now())`, sql.FirstSenderID)
	execSQL(t, pool, `
SELECT cron.schedule(
    $1,
    $2,
    $3
)`, sql.RetentionJobName, sql.RetentionSchedule, sql.RetentionCommand)
	execSQL(t, pool, `
INSERT INTO pin_glide.model_configurations (id, workspace_id, model, enabled)
VALUES ('default', 'retention-workspace', 'qwen3.7-flash', true)`)
	execSQL(t, pool, `
INSERT INTO pin_glide.retention_policies
    (schema_name, table_name, retention_days, date_column)
VALUES
    ('pin_glide', 'delivery_deliveries', 7, 'created_at'),
    ('pin_glide', 'identity_credentials', 7, 'expires_at'),
    ('pin_glide', 'identity_refresh_operations', 7, 'expires_at'),
    ('pin_glide', 'pairing_sessions', 7, 'expires_at'),
    ('pin_glide', 'service_diagnostics', 30, 'occurred_at'),
    ('release', 'release_schema_audits', 30, 'executed_at'),
    ('cron', 'job_run_details', 30, 'end_time')`)

	execSQL(t, pool, sql.RetentionAging)
	execSQL(t, pool, sql.RetentionBoundary)
	state := retentionState(t, pool, sql.RetentionState)
	assertNestedCount(t, state, "durable", "retentionJob", 1)
	assertNestedCount(t, state, "aging", "deliveryOld", 1)
	assertNestedCount(t, state, "aging", "deliveryFresh", 1)
	assertNestedCount(t, state, "aging", "cronRunning", 1)
	assertNestedCount(t, state, "boundary", "deliveryCutoff", 1)
	assertNestedCount(t, state, "boundary", "deliveryPastCutoff", 1)
	assertNestedCount(t, state, "live", "credential", 1)
	assertNestedCount(t, state, "live", "guard", 1)
	var privacy string
	if err := pool.QueryRowContext(context.Background(), sql.RetentionPrivacy).Scan(&privacy); err != nil {
		t.Fatalf("read run-owned privacy rows: %v", err)
	}
	if privacy == "" {
		t.Fatal("run-owned privacy query returned no evidence")
	}
	var privacyRows map[string]any
	if err := json.Unmarshal([]byte(privacy), &privacyRows); err != nil {
		t.Fatalf("decode run-owned privacy rows: %v: %s", err, privacy)
	}
	for _, category := range []string{
		"accounts", "senders", "credentials", "refreshOperations", "receivers",
		"receiverCredentials", "pairings", "pairingSessions", "deliveries",
		"diagnostics", "schemaAudits", "cronRuns",
	} {
		rows, ok := privacyRows[category].([]any)
		if !ok || len(rows) == 0 {
			t.Fatalf("privacy category %s is empty: %s", category, privacy)
		}
	}
	execSQL(t, pool, sql.ConfigurationProbe)
	assertCount(t, pool, 1,
		`SELECT count(*) FROM pin_glide.model_configurations WHERE id = 'default'`)
	execSQL(t, pool, sql.RetentionCall)
	state = retentionState(t, pool, sql.RetentionState)
	assertNestedCount(t, state, "aging", "deliveryOld", 0)
	assertNestedCount(t, state, "aging", "deliveryFresh", 1)
	assertNestedCount(t, state, "aging", "cronRunning", 1)
	assertNestedCount(t, state, "boundary", "deliveryCutoff", 1)
	assertNestedCount(t, state, "boundary", "deliveryPastCutoff", 0)
	assertNestedCount(t, state, "live", "credential", 1)
	assertNestedCount(t, state, "live", "guard", 1)

	execSQL(t, pool, sql.RetentionCleanup)
	state = retentionState(t, pool, sql.RetentionState)
	for _, section := range []string{"aging", "boundary", "live", "owned"} {
		for name, count := range state[section].(map[string]any) {
			if count != float64(0) {
				t.Errorf("run-owned retention fixture %s.%s survived cleanup: %v", section, name, count)
			}
		}
	}
	execSQL(t, pool, sql.Cleanup)
	assertCleanupVerification(t, pool, sql.CleanupVerification, sql.CleanupCategories)
}

func TestRetentionProbeCleanupTargetsOnlyItsOwnJob(t *testing.T) {
	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")
	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open disposable database: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	sql := generateSmokeSQL(t)

	execSQL(t, pool, `
SELECT cron.schedule('smoke-integration-retention-probe', '1 second', 'SELECT 1;');
SELECT cron.schedule('smoke-integration-unrelated-probe', '1 second', 'SELECT 1;');
`)
	assertCount(t, pool, 1,
		`SELECT count(*) FROM cron.job WHERE jobname = 'smoke-integration-retention-probe'`)
	assertCount(t, pool, 1,
		`SELECT count(*) FROM cron.job WHERE jobname = 'smoke-integration-unrelated-probe'`)

	targetPool, targetConnection := heldConnection(t, postgres.DSN, "pg_cron")
	unrelatedPool, unrelatedConnection := heldConnection(t, postgres.DSN, "pg_cron")
	targetContext, targetCancel := context.WithCancel(context.Background())
	unrelatedContext, unrelatedCancel := context.WithCancel(context.Background())
	t.Cleanup(func() { _ = targetPool.Close() })
	t.Cleanup(func() { _ = unrelatedPool.Close() })
	t.Cleanup(targetCancel)
	t.Cleanup(unrelatedCancel)
	go func() {
		_, _ = targetConnection.ExecContext(targetContext,
			`SELECT pg_sleep(20); -- smoke-integration-retention-probe`)
	}()
	go func() {
		_, _ = unrelatedConnection.ExecContext(unrelatedContext,
			`SELECT pg_sleep(20); -- smoke-integration-unrelated-probe`)
	}()
	waitForRunningQuery(t, pool, "%smoke-integration-retention-probe%", 1)
	waitForRunningQuery(t, pool, "%smoke-integration-unrelated-probe%", 1)

	execSQL(t, pool, sql.ProbeCleanup)
	assertCount(t, pool, 0,
		`SELECT count(*) FROM cron.job WHERE jobname = 'smoke-integration-retention-probe'`)
	assertCount(t, pool, 1,
		`SELECT count(*) FROM cron.job WHERE jobname = 'smoke-integration-unrelated-probe'`)
	waitForRunningQuery(t, pool, "%smoke-integration-retention-probe%", 0)
	assertRunningQuery(t, pool, "%smoke-integration-unrelated-probe%", 1)
	unrelatedCancel()
	targetCancel()
	execSQL(t, pool, sql.ProbeHistoryCleanup)
}

func waitForRunningQuery(t *testing.T, pool *sql.DB, pattern string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := runningQueryCount(t, pool, pattern); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("running pg_cron query count for %s did not reach %d", pattern, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertRunningQuery(t *testing.T, pool *sql.DB, pattern string, want int) {
	t.Helper()
	if got := runningQueryCount(t, pool, pattern); got != want {
		t.Fatalf("running pg_cron query count for %s is %d, want %d", pattern, got, want)
	}
}

func runningQueryCount(t *testing.T, pool *sql.DB, pattern string) int {
	t.Helper()
	var count int
	if err := pool.QueryRowContext(context.Background(), `
SELECT count(*)
FROM pg_stat_activity
WHERE application_name = 'pg_cron'
  AND state = 'active'
  AND query LIKE $1`, pattern).Scan(&count); err != nil {
		t.Fatalf("query running pg_cron statements: %v", err)
	}
	return count
}

func assertCleanupVerification(
	t *testing.T,
	pool *sql.DB,
	statement string,
	expectedCategories []string,
) {
	t.Helper()
	var raw string
	if err := pool.QueryRowContext(context.Background(), statement).Scan(&raw); err != nil {
		t.Fatalf("read cleanup verification: %v", err)
	}
	var remaining map[string]float64
	if err := json.Unmarshal([]byte(raw), &remaining); err != nil {
		t.Fatalf("decode cleanup verification: %v: %s", err, raw)
	}
	if len(remaining) != len(expectedCategories) {
		t.Fatalf("cleanup verification has %d categories, want %d: %s", len(remaining), len(expectedCategories), raw)
	}
	for _, category := range expectedCategories {
		count, ok := remaining[category]
		if !ok {
			t.Fatalf("cleanup verification has no %s category: %s", category, raw)
		}
		if count != 0 {
			t.Errorf("run-owned %s rows survived cleanup: %v", category, count)
		}
	}
}
