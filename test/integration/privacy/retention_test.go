//go:build integration

// Table-level retention of the composed service: which target tables have a
// cleanup policy, how long each retains, and that scheduled cleanup deletes
// exactly the rows its policies put before their cutoff.
package privacy_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
		configured[entry.schema+"."+entry.table] = entry
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read retention policies: %v", err)
	}
	return configured
}

// TestRetentionPoliciesCoverTwoTiersAndDeleteByAge shows the adopted table
// tiers as retained rows and as behaviour: one policy per target table, seven
// days for transaction data, thirty for diagnostics, and scheduled cleanup
// deleting exactly the rows each policy puts before its cutoff.
func TestRetentionPoliciesCoverTwoTiersAndDeleteByAge(t *testing.T) {
	environment := start(t)
	now := time.Now().UTC()

	// The policies are the shipped data script's, applied the way the release
	// applies them: after the schema both streams own.
	environment.applyDataScript(t)

	configured := environment.policies(t)
	if len(configured) != len(retainedTables) {
		t.Fatalf("configured %d policies, want %d: %v",
			len(configured), len(retainedTables), configured)
	}
	for target, days := range retainedTables {
		entry, found := configured[target]
		if !found {
			t.Errorf("no policy configures %s", target)
			continue
		}
		if entry.retentionDays != days {
			t.Errorf("%s retains %d days, want %d", target, entry.retentionDays, days)
		}
		if entry.dateColumn != retainedDateColumns[target] {
			t.Errorf("%s filters on %q, want %q", target, entry.dateColumn,
				retainedDateColumns[target])
		}
	}

	// One row well inside every window, one row well outside it.
	senderID, receiverID := environment.registrations(t)
	environment.exec(t, `
INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at, completed_at)
SELECT 'delivery-old', $1, 'message-old', $2, id, 'offline', $3, $3
FROM pin_glide.pairing_pairings LIMIT 1`, senderID, receiverID, now.AddDate(0, 0, -8))
	environment.exec(t, `
INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at, completed_at)
SELECT 'delivery-fresh', $1, 'message-fresh', $2, id, 'offline', $3, $3
FROM pin_glide.pairing_pairings LIMIT 1`, senderID, receiverID, now.AddDate(0, 0, -6))
	environment.exec(t, `
INSERT INTO pin_glide.pairing_sessions
    (id, mode, receiver_id, state, token_digest, created_at, expires_at, closed_at)
VALUES ('session-old', 'new_receiver', $1, 'expired', 'digest-old', $2, $2, $2)`,
		receiverID, now.AddDate(0, 0, -8))
	environment.exec(t, `
INSERT INTO pin_glide.pairing_sessions
    (id, mode, receiver_id, state, token_digest, created_at, expires_at, closed_at)
VALUES ('session-fresh', 'new_receiver', $1, 'expired', 'digest-fresh', $2, $2, $2)`,
		receiverID, now.AddDate(0, 0, -6))
	environment.exec(t, `
INSERT INTO pin_glide.service_diagnostics
    (occurred_at, request_id, kind, method, code, duration_ms)
VALUES ($1, 'request-old', 'http', 'GET', 200, 3)`, now.AddDate(0, 0, -31))
	environment.exec(t, `
INSERT INTO pin_glide.service_diagnostics
    (occurred_at, request_id, kind, method, code, duration_ms)
VALUES ($1, 'request-fresh', 'http', 'GET', 200, 3)`, now.AddDate(0, 0, -29))

	// Scheduled cleanup measures every window back from the reference time the
	// job passes and deletes only what each policy puts before its cutoff.
	var removed int
	if err := environment.pool.QueryRowContext(context.Background(),
		`SELECT pin_glide.pin_glide_fn_cleanup_expired_rows($1)`, now).Scan(&removed); err != nil {
		t.Fatalf("run scheduled cleanup: %v", err)
	}
	if removed != 3 {
		t.Errorf("scheduled cleanup removed %d rows, want the 3 outside their windows", removed)
	}
	for name, query := range map[string]string{
		"expired Delivery": `SELECT count(*) FROM pin_glide.delivery_deliveries
		    WHERE id IN ('delivery-old', 'delivery-fresh')`,
		"expired Pairing Session": `SELECT count(*) FROM pin_glide.pairing_sessions
		    WHERE id IN ('session-old', 'session-fresh')`,
		"expired diagnostic": `SELECT count(*) FROM pin_glide.service_diagnostics
		    WHERE request_id IN ('request-old', 'request-fresh')`,
	} {
		var kept int
		if err := environment.pool.QueryRowContext(context.Background(), query).Scan(&kept); err != nil {
			t.Fatalf("count %s rows: %v", name, err)
		}
		if kept != 1 {
			t.Errorf("cleanup kept %d %s rows, want the 1 inside its window", kept, name)
		}
	}
}

// retainedTables is the adopted whole-table retention of every target the
// shipped policies configure, in days.
var retainedTables = map[string]int{
	"pin_glide.delivery_deliveries":         7,
	"pin_glide.identity_credentials":        7,
	"pin_glide.identity_refresh_operations": 7,
	"pin_glide.pairing_sessions":            7,
	"pin_glide.service_diagnostics":         30,
}

// retainedDateColumns is the clock every configured target is filtered on.
var retainedDateColumns = map[string]string{
	"pin_glide.delivery_deliveries":         "created_at",
	"pin_glide.identity_credentials":        "expires_at",
	"pin_glide.identity_refresh_operations": "expires_at",
	"pin_glide.pairing_sessions":            "expires_at",
	"pin_glide.service_diagnostics":         "occurred_at",
}

// applyDataScript runs the shipped retention data script, so this test reads
// the policies and the scheduled job the release deploys rather than a
// test-only copy of them.
func (e *environment) applyDataScript(t *testing.T) {
	t.Helper()

	path := filepath.Join(repositoryRoot(t), "db", "scripts",
		"issue_30_1_retention_cleanup.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the retention data script: %v", err)
	}
	if _, err := e.pool.ExecContext(context.Background(), string(body)); err != nil {
		t.Fatalf("apply the retention data script: %v", err)
	}

	var jobname string
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT jobname FROM cron.job WHERE jobname = 'pin-glide-retention-cleanup'`).
		Scan(&jobname); err != nil {
		t.Fatalf("read the scheduled cleanup job: %v", err)
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

// registrations inserts one Account with an active Sender, an active Receiver
// and their Pairing, which is the durable state the retention targets hang off.
// The business workflow that would create it is exercised by its own suite.
func (e *environment) registrations(t *testing.T) (senderID, receiverID string) {
	t.Helper()

	e.exec(t, `
INSERT INTO pin_glide.identity_accounts
    (id, wechat_app_id, wechat_open_id)
VALUES ('account-retention', 'wx-app', 'openid-retention')`)
	e.exec(t, `
INSERT INTO pin_glide.identity_sender_registrations (id, account_id)
VALUES ('sender-retention', 'account-retention')`)
	e.exec(t, `
INSERT INTO pin_glide.identity_receiver_registrations (id, account_id, state)
VALUES ('receiver-retention', 'account-retention', 'active')`)
	e.exec(t, `
INSERT INTO pin_glide.pairing_pairings
    (id, sender_id, receiver_id, state, alias, activated_at)
VALUES ('pairing-retention', 'sender-retention', 'receiver-retention', 'active',
    'Retention', now())`)
	return "sender-retention", "receiver-retention"
}
