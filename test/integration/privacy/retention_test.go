//go:build integration

// Table-level retention of the composed service: the adopted whole-table
// policy of every implemented table, and that scheduled cleanup deletes
// exactly the rows those policies put before their cutoff.
package privacy_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// policy is one retained whole-table cleanup policy.
type policy struct {
	schema        string
	table         string
	retentionDays int
	dateColumn    string
}

// target names the configured schema and table a policy cleans up.
func (p policy) target() string { return p.schema + "." + p.table }

// retainedTargets is the purge set of the adopted whole-table inventory: the
// transaction data that expires after seven days and the diagnostic and audit
// history that expires after thirty, each with the clock cleanup filters on.
// The clocks preserve validity: a credential is measured from its own expiry,
// and the ordering guard of a refresh operation from the expiry of the token
// that operation consumed.
var retainedTargets = []policy{
	{"pin_glide", "delivery_deliveries", 7, "created_at"},
	{"pin_glide", "identity_credentials", 7, "expires_at"},
	{"pin_glide", "identity_refresh_operations", 7, "expires_at"},
	{"pin_glide", "pairing_sessions", 7, "expires_at"},
	{"pin_glide", "service_diagnostics", 30, "occurred_at"},
	{"release", "release_schema_audits", 30, "executed_at"},
	{"cron", "job_run_details", 30, "end_time"},
}

// durableTables is every implemented table the adopted inventory keeps without
// scheduled deletion: durable identity and authority state, configuration, and
// the permanent release and job ledgers.
var durableTables = []string{
	"cron.job",
	"pin_glide.identity_accounts",
	"pin_glide.identity_receiver_credentials",
	"pin_glide.identity_receiver_registrations",
	"pin_glide.identity_sender_registrations",
	"pin_glide.model_configurations",
	"pin_glide.pairing_pairings",
	"pin_glide.retention_policies",
	"release.release_data_scripts",
	"release.release_deploy_history",
	"release.release_migration_scripts",
}

// TestRetentionInventoryAssignsEveryImplementedTableOnePolicy shows the
// adopted inventory as a fact about the deployed database: every implemented
// table carries exactly one whole-table policy, only the purge targets are
// configured, and each configured policy carries the adopted window and clock.
func TestRetentionInventoryAssignsEveryImplementedTableOnePolicy(t *testing.T) {
	environment := start(t)
	environment.applyDataScripts(t)

	adopted := map[string]int{}
	for _, target := range retainedTargets {
		adopted[target.target()] = target.retentionDays
	}
	for _, table := range durableTables {
		if _, purgeTarget := adopted[table]; purgeTarget {
			t.Errorf("%s is classified as both durable and a purge target", table)
		}
		adopted[table] = 0
	}

	implemented := environment.tables(t)
	for table := range implemented {
		if _, classified := adopted[table]; !classified {
			t.Errorf("implemented table %s has no adopted whole-table policy", table)
		}
	}
	for table := range adopted {
		if !implemented[table] {
			t.Errorf("the adopted inventory classifies %s, which is not implemented", table)
		}
	}

	configured := environment.policies(t)
	if len(configured) != len(retainedTargets) {
		t.Errorf("configured %d policies, want the %d purge targets: %v",
			len(configured), len(retainedTargets), configured)
	}
	for _, target := range retainedTargets {
		entry, found := configured[target.target()]
		if !found {
			t.Errorf("no configured policy purges %s", target.target())
			continue
		}
		if entry.retentionDays != target.retentionDays || entry.dateColumn != target.dateColumn {
			t.Errorf("%s configures %d days on %q, want %d days on %q", target.target(),
				entry.retentionDays, entry.dateColumn, target.retentionDays, target.dateColumn)
		}
	}
	for target := range configured {
		if adopted[target] == 0 {
			t.Errorf("configured policy purges %s, which the inventory keeps", target)
		}
	}
}

// TestScheduledCleanupDeletesExactlyTheRowsOutsideTheirWindows shows the
// adopted tiers as behaviour: one row well outside and one row well inside its
// window for every purge target, and scheduled cleanup deleting exactly the
// seven outside rows. Durable identity and authority state survives the run,
// and an authorized configuration deletion still removes configuration alone.
func TestScheduledCleanupDeletesExactlyTheRowsOutsideTheirWindows(t *testing.T) {
	environment := start(t)
	environment.applyDataScripts(t)
	now := time.Now().UTC()
	senderID, receiverID, pairingID := environment.registrations(t)

	// Seven-day transaction data: a Delivery, a consumed credential, the
	// ordering guard of a consumed token, and a Pairing Session.
	environment.exec(t, `
INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at, completed_at)
VALUES ('delivery-old', $1, 'message-old', $2, $3, 'offline', $4, $4),
       ('delivery-fresh', $1, 'message-fresh', $2, $3, 'offline', $5, $5)`,
		senderID, receiverID, pairingID, now.AddDate(0, 0, -8), now.AddDate(0, 0, -6))
	environment.exec(t, `
INSERT INTO pin_glide.identity_credentials
    (sender_id, family_id, scope, token_digest, issued_at, expires_at)
VALUES ($1, 'family-policy', 'refresh', 'digest-old', $2, $2),
       ($1, 'family-policy', 'refresh', 'digest-fresh', $3, $3)`,
		senderID, now.AddDate(0, 0, -8), now.AddDate(0, 0, -6))
	environment.exec(t, `
INSERT INTO pin_glide.identity_refresh_operations
    (sender_id, operation_id, attempt, parent_digest, expires_at, committed_at)
VALUES ($1, 'operation-old', 0, 'parent-old', $2, $2),
       ($1, 'operation-fresh', 0, 'parent-fresh', $3, $3)`,
		senderID, now.AddDate(0, 0, -8), now.AddDate(0, 0, -6))
	environment.exec(t, `
INSERT INTO pin_glide.pairing_sessions
    (id, mode, receiver_id, state, token_digest, created_at, expires_at, closed_at)
VALUES ('session-old', 'new_receiver', $1, 'expired', 'session-digest-old', $2, $2, $2),
       ('session-fresh', 'new_receiver', $1, 'expired', 'session-digest-fresh', $3, $3, $3)`,
		receiverID, now.AddDate(0, 0, -8), now.AddDate(0, 0, -6))

	// Thirty-day diagnostic and audit history: one service diagnostic, one
	// schema audit record, and one pg_cron execution record.
	environment.exec(t, `
INSERT INTO pin_glide.service_diagnostics
    (occurred_at, request_id, kind, method, code, duration_ms)
VALUES ($1, 'request-old', 'http', 'GET', 200, 3),
       ($2, 'request-fresh', 'http', 'GET', 200, 3)`,
		now.AddDate(0, 0, -31), now.AddDate(0, 0, -29))
	environment.exec(t, `
INSERT INTO release.release_schema_audits
    (target_schema, operation, stdout, stderr, executed_at)
VALUES ('pin_glide', 'apply', 'old', '', $1),
       ('pin_glide', 'apply', 'fresh', '', $2)`,
		now.AddDate(0, 0, -31), now.AddDate(0, 0, -29))
	environment.exec(t, `
INSERT INTO cron.job_run_details
    (jobid, database, username, command, status, start_time, end_time)
VALUES (1, current_database(), current_user, 'retention-probe-old', 'succeeded', $1, $1),
       (1, current_database(), current_user, 'retention-probe-fresh', 'succeeded', $2, $2)`,
		now.AddDate(0, 0, -31), now.AddDate(0, 0, -29))

	// Scheduled cleanup measures every window back from the reference time the
	// job passes and deletes only what each policy puts before its cutoff.
	if removed := environment.runCleanup(t, now); removed != len(retainedTargets) {
		t.Errorf("scheduled cleanup removed %d rows, want the %d outside their windows",
			removed, len(retainedTargets))
	}

	for name, query := range map[string]string{
		"Delivery": `SELECT count(*) FROM pin_glide.delivery_deliveries
		    WHERE status = 'offline' AND created_at < now() - interval '7 days'`,
		"credential": `SELECT count(*) FROM pin_glide.identity_credentials
		    WHERE token_digest = 'digest-old'`,
		"refresh operation": `SELECT count(*) FROM pin_glide.identity_refresh_operations
		    WHERE operation_id = 'operation-old'`,
		"Pairing Session": `SELECT count(*) FROM pin_glide.pairing_sessions
		    WHERE id = 'session-old'`,
		"service diagnostic": `SELECT count(*) FROM pin_glide.service_diagnostics
		    WHERE request_id = 'request-old'`,
		"schema audit": `SELECT count(*) FROM release.release_schema_audits
		    WHERE stdout = 'old'`,
		"cron execution": `SELECT count(*) FROM cron.job_run_details
		    WHERE command = 'retention-probe-old'`,
	} {
		if expired := environment.count(t, query); expired != 0 {
			t.Errorf("cleanup kept %d expired %s rows", expired, name)
		}
	}
	for name, query := range map[string]string{
		"Delivery": `SELECT count(*) FROM pin_glide.delivery_deliveries
		    WHERE id = 'delivery-fresh'`,
		"credential": `SELECT count(*) FROM pin_glide.identity_credentials
		    WHERE token_digest = 'digest-fresh'`,
		"refresh operation": `SELECT count(*) FROM pin_glide.identity_refresh_operations
		    WHERE operation_id = 'operation-fresh'`,
		"Pairing Session": `SELECT count(*) FROM pin_glide.pairing_sessions
		    WHERE id = 'session-fresh'`,
		"service diagnostic": `SELECT count(*) FROM pin_glide.service_diagnostics
		    WHERE request_id = 'request-fresh'`,
		"schema audit": `SELECT count(*) FROM release.release_schema_audits
		    WHERE stdout = 'fresh'`,
		"cron execution": `SELECT count(*) FROM cron.job_run_details
		    WHERE command = 'retention-probe-fresh'`,
	} {
		if kept := environment.count(t, query); kept != 1 {
			t.Errorf("cleanup deleted the %s row inside its window: %d survive", name, kept)
		}
	}

	// The rows every tier retains survive an authorized configuration deletion,
	// and the durable authority those targets hang off is not a purge target at
	// all.
	environment.exec(t, `
INSERT INTO pin_glide.model_configurations (id, workspace_id, model, enabled)
VALUES ('default', 'retention-workspace', 'qwen3.7-flash', true)
ON CONFLICT (id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id, model = EXCLUDED.model,
    enabled = EXCLUDED.enabled, updated_at = now()`)
	environment.exec(t, `DELETE FROM pin_glide.model_configurations WHERE id = 'default'`)
	if configurations := environment.count(t,
		`SELECT count(*) FROM pin_glide.model_configurations`); configurations != 0 {
		t.Errorf("the authorized configuration deletion left %d rows", configurations)
	}
	for name, query := range map[string]string{
		"Account": `SELECT count(*) FROM pin_glide.identity_accounts
		    WHERE id = 'account-retention'`,
		"Sender registration": `SELECT count(*) FROM pin_glide.identity_sender_registrations
		    WHERE id = 'sender-retention'`,
		"Receiver registration": `SELECT count(*) FROM pin_glide.identity_receiver_registrations
		    WHERE id = 'receiver-retention'`,
		"active Pairing": `SELECT count(*) FROM pin_glide.pairing_pairings
		    WHERE id = 'pairing-retention' AND state = 'active'`,
	} {
		if kept := environment.count(t, query); kept != 1 {
			t.Errorf("%d durable %s rows survived cleanup, want the 1 it started with", kept, name)
		}
	}
}

// policies reads every configured cleanup policy by target.
func (e *environment) policies(t *testing.T) map[string]policy {
	t.Helper()

	rows, err := e.pool.QueryContext(context.Background(), `
SELECT schema_name, table_name, retention_days, date_column
FROM pin_glide.retention_policies`)
	if err != nil {
		t.Fatalf("read retention policies: %v", err)
	}
	defer func() { _ = rows.Close() }()

	configured := map[string]policy{}
	for rows.Next() {
		var entry policy
		if err := rows.Scan(&entry.schema, &entry.table, &entry.retentionDays,
			&entry.dateColumn); err != nil {
			t.Fatalf("read one retention policy: %v", err)
		}
		configured[entry.target()] = entry
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read retention policies: %v", err)
	}
	return configured
}

// tables reads every implemented table of the application, release and cron
// schemas the adopted inventory classifies.
func (e *environment) tables(t *testing.T) map[string]bool {
	t.Helper()

	rows, err := e.pool.QueryContext(context.Background(), `
SELECT schemaname, tablename
FROM pg_tables
WHERE schemaname IN ('pin_glide', 'release', 'cron')`)
	if err != nil {
		t.Fatalf("read implemented tables: %v", err)
	}
	defer func() { _ = rows.Close() }()

	implemented := map[string]bool{}
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			t.Fatalf("read one implemented table: %v", err)
		}
		implemented[schema+"."+table] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read implemented tables: %v", err)
	}
	return implemented
}

// runCleanup executes the routine the deployed scheduled job runs at the
// reference time of a run and reports how many rows it deleted.
func (e *environment) runCleanup(t *testing.T, reference time.Time) int {
	t.Helper()

	var removed int
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT pin_glide.pin_glide_fn_cleanup_expired_rows($1)`, reference).
		Scan(&removed); err != nil {
		t.Fatalf("run scheduled cleanup: %v", err)
	}
	return removed
}

// applyDataScripts applies every shipped data script in filename order, the
// order a release runs the scripts one commit introduces. The tests read the
// policies and the scheduled job the release deploys rather than a test-only
// copy of them, and apply the files directly: the release records each script
// and never applies it again, so a second application here shows the
// rerunnability the scripts owe instead of that skip.
func (e *environment) applyDataScripts(t *testing.T) {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(repositoryRoot(t), "db", "scripts", "*.sql"))
	if err != nil {
		t.Fatalf("locate the retention data scripts: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("the repository ships no data scripts")
	}
	sort.Strings(paths)
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read the data script %s: %v", path, err)
		}
		if _, err := e.pool.ExecContext(context.Background(), string(body)); err != nil {
			t.Fatalf("apply the data script %s: %v", path, err)
		}
	}
}

// repositoryRoot returns the checkout this test file lives in.
func repositoryRoot(t *testing.T) string {
	t.Helper()

	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
}

// exec runs one statement against the database the composed service uses.
func (e *environment) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := e.pool.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

// count reads one row count from the database the composed service uses.
func (e *environment) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()

	var value int64
	if err := e.pool.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// registrations inserts one Account with an active Sender, an active Receiver
// and their Pairing, which is the durable state the retention targets hang off.
// The business workflow that would create it is exercised by its own suite.
func (e *environment) registrations(t *testing.T) (senderID, receiverID, pairingID string) {
	t.Helper()

	e.exec(t, `
INSERT INTO pin_glide.identity_accounts
    (id, provider, app_id, open_id)
VALUES ('account-retention', 'wechat', 'wx-app', 'openid-retention')`)
	e.exec(t, `
INSERT INTO pin_glide.identity_sender_registrations (device_name, id, account_id)
VALUES ('Test Sender', 'sender-retention', 'account-retention')`)
	e.exec(t, `
INSERT INTO pin_glide.identity_receiver_registrations (id, account_id, state)
VALUES ('receiver-retention', 'account-retention', 'active')`)
	e.exec(t, `
INSERT INTO pin_glide.pairing_pairings
    (id, sender_id, receiver_id, state, alias, activated_at)
VALUES ('pairing-retention', 'sender-retention', 'receiver-retention', 'active',
    'Retention', now())`)
	return "sender-retention", "receiver-retention", "pairing-retention"
}
