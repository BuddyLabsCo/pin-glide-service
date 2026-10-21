//go:build integration

package smoke_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

type generatedSmokeSQL struct {
	Bootstrap           string   `json:"bootstrap"`
	Cleanup             string   `json:"cleanup"`
	ExternalWriterSQL   string   `json:"externalWriterSql"`
	TerminateWriterSQL  string   `json:"terminateWriterSql"`
	ExpireAccess        string   `json:"expireAccess"`
	ExpireRefresh       string   `json:"expireRefresh"`
	AgeOperation        string   `json:"ageOperation"`
	ExpireParent        string   `json:"expireParent"`
	RevokeRegistration  string   `json:"revokeRegistration"`
	SupersedeFamily     string   `json:"supersedeFamily"`
	InvalidateReceiver  string   `json:"invalidateReceiver"`
	RetentionAging      string   `json:"retentionAging"`
	RetentionBoundary   string   `json:"retentionBoundary"`
	RetentionState      string   `json:"retentionState"`
	RetentionCall       string   `json:"retentionCall"`
	RetentionPrivacy    string   `json:"retentionPrivacy"`
	RetentionCleanup    string   `json:"retentionCleanup"`
	RetentionProbe      string   `json:"retentionProbeCommand"`
	ProbeCleanup        string   `json:"probeCleanup"`
	ProbeHistoryCleanup string   `json:"probeHistoryCleanup"`
	CleanupVerification string   `json:"cleanupVerification"`
	CleanupCategories   []string `json:"cleanupCategories"`
	ConfigurationProbe  string   `json:"configurationProbe"`
	RetentionJobName    string   `json:"retentionJobName"`
	RetentionSchedule   string   `json:"retentionSchedule"`
	RetentionCommand    string   `json:"retentionCommand"`
	RetentionOwner      string   `json:"retentionOwner"`
	RetentionDatabase   string   `json:"retentionDatabase"`
	FirstSenderID       string   `json:"firstSenderId"`
	FirstAccountID      string   `json:"firstAccountId"`
	SecondAccountID     string   `json:"secondAccountId"`
	ScopeSenderID       string   `json:"scopeSenderId"`
	ScopeParentDigest   string   `json:"scopeParentDigest"`
	RefreshLifetime     string   `json:"refreshLifetime"`
	PermanentSenderID   string   `json:"permanentSenderId"`
	SupersededSenderID  string   `json:"supersededSenderId"`
	SenderCount         int      `json:"senderCount"`
	CredentialCount     int      `json:"credentialCount"`
}

func TestFixtureSQLBootstrapsAndCleansRunOwnedRows(t *testing.T) {
	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")
	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open disposable database: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	sql := generateSmokeSQL(t)
	execSQL(t, pool, sql.Bootstrap)
	assertCount(t, pool, 2, `SELECT count(*) FROM pin_glide.identity_accounts
		WHERE id IN ($1, $2)`, sql.FirstAccountID, sql.SecondAccountID)
	assertCount(t, pool, sql.SenderCount, `SELECT count(*) FROM pin_glide.identity_sender_registrations
		WHERE id LIKE 'smoke-integration-fixture-sender-%' AND state = 'active'`)
	assertCount(t, pool, sql.CredentialCount, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE sender_id LIKE 'smoke-integration-fixture-sender-%'
		  AND token_digest ~ '^[0-9a-f]{64}$'`)

	unaffectedBefore := stateFingerprint(t, pool, sql.ScopeSenderID, sql.SupersededSenderID, sql.PermanentSenderID)
	execSQL(t, pool, `
INSERT INTO pin_glide.identity_refresh_operations
    (sender_id, operation_id, attempt, parent_digest, expires_at, committed_at)
VALUES ($1, 'integration-operation', 0, $2, now() + $3::interval, now())`,
		sql.ScopeSenderID, sql.ScopeParentDigest, sql.RefreshLifetime)
	execSQL(t, pool, sql.ExpireAccess)
	assertCount(t, pool, 2, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE sender_id = $1
		  AND scope IN ('user_session', 'uploader')
		  AND expires_at <= now()`, sql.ScopeSenderID)
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE sender_id = $1
		  AND scope = 'refresh'
		  AND expires_at > now()`, sql.ScopeSenderID)
	execSQL(t, pool, sql.ExpireRefresh)
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE sender_id = $1
		  AND scope = 'refresh'
		  AND expires_at <= now()`, sql.ScopeSenderID)
	execSQL(t, pool, sql.AgeOperation)
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_refresh_operations
		WHERE sender_id = $1
		  AND operation_id = 'integration-operation'
		  AND committed_at <= now() - interval '30 seconds'`, sql.ScopeSenderID)
	execSQL(t, pool, sql.ExpireParent)
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_credentials AS credential
		JOIN pin_glide.identity_refresh_operations AS operation
		  ON operation.sender_id = credential.sender_id
		 AND operation.parent_digest = credential.token_digest
		WHERE operation.sender_id = $1
		  AND operation.operation_id = 'integration-operation'
		  AND credential.expires_at <= now()`, sql.ScopeSenderID)
	execSQL(t, pool, sql.SupersedeFamily)
	assertCount(t, pool, 3, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE sender_id = $1 AND revoked_at IS NOT NULL`, sql.SupersededSenderID)
	assertCount(t, pool, 3, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE sender_id = $1 AND revoked_at IS NULL`, sql.SupersededSenderID)
	execSQL(t, pool, sql.RevokeRegistration)
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_sender_registrations
		WHERE id = $1 AND state = 'revoked' AND revoked_at IS NOT NULL`, sql.PermanentSenderID)
	assertCount(t, pool, 0, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE sender_id = $1 AND revoked_at IS NULL`, sql.PermanentSenderID)
	if unaffectedAfter := stateFingerprint(
		t, pool, sql.ScopeSenderID, sql.SupersededSenderID, sql.PermanentSenderID,
	); unaffectedAfter != unaffectedBefore {
		t.Fatal("fixture state SQL changed an unrelated Sender or credential")
	}

	execSQL(t, pool, `
INSERT INTO pin_glide.identity_receiver_registrations (id, account_id, state)
VALUES ('smoke-integration-fixture-receiver', $1, 'active')`, sql.FirstAccountID)
	execSQL(t, pool, `
INSERT INTO pin_glide.identity_receiver_credentials
    (receiver_id, generation, token_digest)
VALUES ('smoke-integration-fixture-receiver', 1, repeat('a', 64))`)
	execSQL(t, pool, `
INSERT INTO pin_glide.identity_receiver_registrations (id, account_id, state)
VALUES ('smoke-integration-unaffected-receiver', $1, 'active')`, sql.FirstAccountID)
	execSQL(t, pool, `
INSERT INTO pin_glide.identity_receiver_credentials
    (receiver_id, generation, token_digest)
VALUES ('smoke-integration-unaffected-receiver', 1, repeat('c', 64))`)
	execSQL(t, pool, sql.InvalidateReceiver)
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_receiver_credentials
		WHERE receiver_id = 'smoke-integration-fixture-receiver'
		  AND revoked_at IS NOT NULL`)
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_receiver_credentials
		WHERE receiver_id = 'smoke-integration-unaffected-receiver'
		  AND revoked_at IS NULL`)
	execSQL(t, pool, `
INSERT INTO pin_glide.pairing_pairings
    (id, sender_id, receiver_id, alias, state, activated_at)
VALUES ('smoke-integration-fixture-pairing', $1,
        'smoke-integration-fixture-receiver', '', 'active', now())`, sql.FirstSenderID)
	execSQL(t, pool, `
INSERT INTO pin_glide.pairing_sessions
    (id, mode, receiver_id, authorized_sender_id, state, token_digest, expires_at)
VALUES ('smoke-integration-fixture-session', 'new_receiver',
        'smoke-integration-fixture-receiver', $1, 'awaiting_confirmation',
        repeat('b', 64), now() + interval '5 minutes')`, sql.FirstSenderID)
	execSQL(t, pool, `
INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, completed_at)
VALUES ('smoke-integration-fixture-delivery', $1, 'smoke-message',
        'smoke-integration-fixture-receiver', 'smoke-integration-fixture-pairing',
        'offline', now())`, sql.FirstSenderID)
	execSQL(t, pool, `
INSERT INTO pin_glide.service_diagnostics
    (request_id, kind, method, code, duration_ms)
VALUES ('smoke-integration-fixture-request', 'http', 'GET', 200, 1)`)
	execSQL(t, pool, `
INSERT INTO pin_glide.identity_accounts (id, provider, app_id, open_id)
VALUES ('unrelated-integration-account', 'qq', 'unrelated', 'unrelated')`)
	execSQL(t, pool, `
INSERT INTO pin_glide.identity_sender_registrations (id, account_id, state)
VALUES ('unrelated-integration-sender', 'unrelated-integration-account', 'active')`)

	execSQL(t, pool, sql.Cleanup)
	for _, query := range []string{
		`SELECT count(*) FROM pin_glide.identity_accounts
		 WHERE id IN ('smoke-integration-fixture-account-a', 'smoke-integration-fixture-account-b')`,
		`SELECT count(*) FROM pin_glide.identity_sender_registrations
		 WHERE id LIKE 'smoke-integration-fixture-sender-%'`,
		`SELECT count(*) FROM pin_glide.identity_credentials
		 WHERE sender_id LIKE 'smoke-integration-fixture-sender-%'`,
		`SELECT count(*) FROM pin_glide.identity_receiver_registrations
		 WHERE id = 'smoke-integration-fixture-receiver'`,
		`SELECT count(*) FROM pin_glide.identity_receiver_credentials
		 WHERE receiver_id = 'smoke-integration-fixture-receiver'`,
		`SELECT count(*) FROM pin_glide.identity_receiver_registrations
		 WHERE id = 'smoke-integration-unaffected-receiver'`,
		`SELECT count(*) FROM pin_glide.identity_receiver_credentials
		 WHERE receiver_id = 'smoke-integration-unaffected-receiver'`,
		`SELECT count(*) FROM pin_glide.pairing_pairings
		 WHERE id = 'smoke-integration-fixture-pairing'`,
		`SELECT count(*) FROM pin_glide.pairing_sessions
		 WHERE id = 'smoke-integration-fixture-session'`,
		`SELECT count(*) FROM pin_glide.delivery_deliveries
		 WHERE id = 'smoke-integration-fixture-delivery'`,
		`SELECT count(*) FROM pin_glide.service_diagnostics
		 WHERE request_id = 'smoke-integration-fixture-request'`,
	} {
		assertCount(t, pool, 0, query)
	}
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_accounts
		WHERE id = 'unrelated-integration-account'`)
	assertCount(t, pool, 1, `SELECT count(*) FROM pin_glide.identity_sender_registrations
		WHERE id = 'unrelated-integration-sender'`)
}

func generateSmokeSQL(t *testing.T) generatedSmokeSQL {
	t.Helper()
	root := repositoryRoot(t)
	script := `
import { pathToFileURL } from 'node:url';
const fixtures = await import(pathToFileURL(process.env.FIXTURES).href);
const receiverFixtures = await import(pathToFileURL(process.env.RECEIVER_FIXTURES).href);
const retentionFixtures = await import(pathToFileURL(process.env.RETENTION_FIXTURES).href);
const retentionChecks = await import(pathToFileURL(process.env.RETENTION_CHECKS).href);
const retentionSession = await import(pathToFileURL(process.env.RETENTION_SESSION).href);
const database = await import(pathToFileURL(process.env.DATABASE).href);
const remote = await import(pathToFileURL(process.env.REMOTE).href);
const plan = fixtures.createFixturePlan({
  runId: 'integration-fixture',
  now: new Date('2026-10-20T00:00:00Z'),
});
const retention = {
  referenceTime: '2026-10-21T04:00:00.000Z',
  senderID: plan.senders[0].id,
  receiverID: 'smoke-integration-retention-receiver',
  pairingID: 'smoke-integration-retention-pairing',
  jobID: 1,
  liveTokenDigest: 'a'.repeat(64),
  liveGuardDigest: 'e'.repeat(64),
};
const scope = plan.senders.find(sender => sender.id.endsWith('-scope'));
const permanent = plan.senders.find(sender => sender.id.endsWith('-permanent'));
const superseded = plan.senders.find(sender => sender.id.endsWith('-superseded'));
const retentionJob = database.cronJobs.get(database.retentionJobName);
process.stdout.write(JSON.stringify({
  bootstrap: fixtures.renderBootstrapSQL(plan),
  cleanup: fixtures.renderCleanupSQL(
    plan,
    ['smoke-integration-fixture-receiver'],
    ['smoke-integration-fixture-request'],
  ),
  externalWriterSql: remote.externalWriterQuery,
  terminateWriterSql: retentionSession.terminateApplicationBackendsSQL(
    'smoke-integration-owned-writer'),
  expireAccess: fixtures.renderExpireAccessSQL(plan, 'scope'),
  expireRefresh: fixtures.renderExpireRefreshSQL(plan, 'scope'),
  ageOperation: fixtures.renderAgeRefreshOperationSQL(plan, 'scope', 'integration-operation'),
  expireParent: fixtures.renderExpireRefreshParentSQL(plan, 'scope', 'integration-operation'),
  revokeRegistration: fixtures.renderRevokeSenderRegistrationSQL(plan, 'permanent'),
  supersedeFamily: fixtures.renderSupersedeFamilySQL(plan, 'superseded'),
  invalidateReceiver: receiverFixtures.renderInvalidateReceiverCredentialSQL(
    plan, 'smoke-integration-fixture-receiver'),
  retentionAging: retentionFixtures.renderRetentionAgingSQL(plan, retention),
  retentionBoundary: retentionFixtures.renderRetentionBoundarySQL(plan, retention),
  retentionState: retentionChecks.retentionStateQuery(plan, retention),
  retentionCall: retentionChecks.cleanupCallSQL(retention.referenceTime),
  retentionPrivacy: retentionChecks.runOwnedPrivacyQuery(plan, {
    receiverIDs: [retention.receiverID],
    requestIDs: [
      'smoke-integration-fixture-request',
      'smoke-integration-fixture-retention-diagnostic-old',
      'smoke-integration-fixture-retention-diagnostic-fresh',
    ],
    subjects: [retention.pairingID],
  }),
  retentionCleanup: retentionChecks.renderRetentionCleanupSQL(plan),
  retentionProbeCommand: retentionSession.retentionProbeCommand(
    'smoke-integration-retention-probe'),
  probeCleanup: retentionSession.retentionProbeCleanupSQL(
    'smoke-integration-retention-probe'),
  probeHistoryCleanup: retentionSession.retentionProbeHistoryCleanupSQL(
    'smoke-integration-retention-probe'),
  cleanupVerification: fixtures.renderCleanupVerificationSQL(
    plan,
    ['smoke-integration-retention-receiver'],
    ['smoke-integration-fixture-request'],
    ['smoke-integration-retention-pairing'],
  ),
  cleanupCategories: fixtures.cleanupVerificationCategories,
  configurationProbe: retentionChecks.renderConfigurationDeleteProbeSQL(plan, retention),
  retentionJobName: database.retentionJobName,
  retentionSchedule: retentionJob.schedule,
  retentionCommand: retentionJob.command,
  retentionOwner: retentionJob.username,
  retentionDatabase: retentionJob.database,
  firstSenderId: plan.senders[0].id,
  firstAccountId: plan.accounts[0].id,
  secondAccountId: plan.accounts[1].id,
  scopeSenderId: scope.id,
  scopeParentDigest: scope.credentials.find(credential => credential.scope === 'refresh').digest,
  refreshLifetime: fixtures.credentialLifetimeInterval('refresh'),
  permanentSenderId: permanent.id,
  supersededSenderId: superseded.id,
  senderCount: plan.senders.length,
  credentialCount: plan.senders.length * fixtures.credentialScopes.length,
}));
`
	command := exec.Command("node", "--input-type=module", "--eval", script)
	command.Dir = root
	command.Env = append(os.Environ(),
		"FIXTURES="+filepath.Join(root, "test/smoke/lib/fixtures.mjs"),
		"RECEIVER_FIXTURES="+filepath.Join(root, "test/smoke/lib/receiver_fixtures.mjs"),
		"RETENTION_FIXTURES="+filepath.Join(root, "test/smoke/lib/retention_fixtures.mjs"),
		"RETENTION_CHECKS="+filepath.Join(root, "test/smoke/lib/retention_checks.mjs"),
		"RETENTION_SESSION="+filepath.Join(root, "test/smoke/lib/session_retention.mjs"),
		"DATABASE="+filepath.Join(root, "test/smoke/lib/database.mjs"),
		"REMOTE="+filepath.Join(root, "test/smoke/lib/remote.mjs"),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("generate smoke SQL: %v: %s", err, output)
	}
	var generated generatedSmokeSQL
	if err := json.Unmarshal(output, &generated); err != nil {
		t.Fatalf("decode generated smoke SQL: %v: %s", err, output)
	}
	return generated
}

func heldConnection(t *testing.T, dsn, applicationName string) (*sql.DB, *sql.Conn) {
	t.Helper()
	pool, err := db.Open(context.Background(), dsn+"&application_name="+applicationName)
	if err != nil {
		t.Fatalf("open %s connection: %v", applicationName, err)
	}
	connection, err := pool.Conn(context.Background())
	if err != nil {
		_ = pool.Close()
		t.Fatalf("hold %s connection: %v", applicationName, err)
	}
	if _, err := connection.ExecContext(context.Background(), "SELECT 1"); err != nil {
		_ = connection.Close()
		_ = pool.Close()
		t.Fatalf("use %s connection: %v", applicationName, err)
	}
	return pool, connection
}

func waitForConnections(t *testing.T, pool *sql.DB, statement string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := connections(t, pool, statement)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("external writer count is %d, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertConnections(t *testing.T, pool *sql.DB, statement string, want int) {
	t.Helper()
	if got := connections(t, pool, statement); got != want {
		t.Fatalf("external writer count is %d, want %d", got, want)
	}
}

func connections(t *testing.T, pool *sql.DB, statement string) int {
	t.Helper()
	var raw string
	if err := pool.QueryRowContext(context.Background(), statement).Scan(&raw); err != nil {
		t.Fatalf("query external writers: %v", err)
	}
	var value struct {
		Connections int `json:"connections"`
	}
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode external writer count: %v: %s", err, raw)
	}
	return value.Connections
}

func execSQL(t *testing.T, pool *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("execute smoke SQL: %v", err)
	}
}

func assertCount(t *testing.T, pool *sql.DB, want int, statement string, args ...any) {
	t.Helper()
	var got int
	if err := pool.QueryRowContext(context.Background(), statement, args...).Scan(&got); err != nil {
		t.Fatalf("count smoke rows: %v", err)
	}
	if got != want {
		t.Fatalf("row count is %d, want %d for %s", got, want, strings.TrimSpace(statement))
	}
}

func retentionState(t *testing.T, pool *sql.DB, statement string) map[string]any {
	t.Helper()
	var raw string
	if err := pool.QueryRowContext(context.Background(), statement).Scan(&raw); err != nil {
		t.Fatalf("read retention state: %v", err)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatalf("decode retention state: %v: %s", err, raw)
	}
	for _, section := range []string{"aging", "boundary", "live", "durable", "owned"} {
		if _, ok := state[section].(map[string]any); !ok {
			t.Fatalf("retention state has no %s object: %s", section, raw)
		}
	}
	return state
}

func assertNestedCount(t *testing.T, state map[string]any, section, field string, want float64) {
	t.Helper()
	values := state[section].(map[string]any)
	if got, ok := values[field]; !ok || got != want {
		t.Fatalf("retention state %s.%s = %v, want %v", section, field, got, want)
	}
}

func stateFingerprint(t *testing.T, pool *sql.DB, excludedSenderIDs ...string) string {
	t.Helper()
	const statement = `
SELECT json_build_object(
  'senders', (
    SELECT coalesce(json_agg(json_build_object(
      'id', id,
      'state', state,
      'revokedAt', revoked_at
    ) ORDER BY id), '[]'::json)
    FROM pin_glide.identity_sender_registrations
    WHERE id NOT IN ($1, $2, $3)
  ),
  'credentials', (
    SELECT coalesce(json_agg(json_build_object(
      'id', id,
      'senderId', sender_id,
      'familyId', family_id,
      'scope', scope,
      'tokenDigest', token_digest,
      'issuedAt', issued_at,
      'expiresAt', expires_at,
      'revokedAt', revoked_at
    ) ORDER BY id), '[]'::json)
    FROM pin_glide.identity_credentials
    WHERE sender_id NOT IN ($1, $2, $3)
  ),
  'operations', (
    SELECT coalesce(json_agg(json_build_object(
      'senderId', sender_id,
      'operationId', operation_id,
      'attempt', attempt,
      'parentDigest', parent_digest,
      'expiresAt', expires_at,
      'committedAt', committed_at
    ) ORDER BY sender_id, operation_id), '[]'::json)
    FROM pin_glide.identity_refresh_operations
    WHERE sender_id NOT IN ($1, $2, $3)
  )
);`
	var fingerprint string
	if err := pool.QueryRowContext(
		context.Background(), statement,
		excludedSenderIDs[0], excludedSenderIDs[1], excludedSenderIDs[2],
	).Scan(&fingerprint); err != nil {
		t.Fatalf("read unaffected fixture state: %v", err)
	}
	return fingerprint
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository root")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	return root
}
