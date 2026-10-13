//go:build integration

package release_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression: fresh installs must record skipped structural scripts, release
// bootstrap may replay, and the Database Release must survive data failure.
// Seam: the public migrate process against disposable PostgreSQL. Ledger rows,
// preserved data and process results are durable release facts, not private calls.
func TestReleaseMetadataBootstrap(t *testing.T) {
	_, work, bin, pool, dsn := releaseDatabase(t)
	write := func(path, body string) {
		t.Helper()
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(work, path)), 0755))
		must(t, os.WriteFile(filepath.Join(work, path), []byte(body), 0644))
	}
	sql := func(query string) { t.Helper(); _, err := pool.Exec(query); must(t, err) }
	text := func(query string) string { t.Helper(); var s string; must(t, pool.QueryRow(query).Scan(&s)); return s }
	initial, retry, version := "true", "false", "1.0.0"
	migrate := func(ok bool, want string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(bin, "pin-glide-release"), "migrate")
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PIN_GLIDE_ENV=production", "PIN_GLIDE_DATABASE_URL="+dsn, "PIN_GLIDE_RELEASE_VERSION="+version, "PIN_GLIDE_RELEASE_INITIAL="+initial, "PIN_GLIDE_RELEASE_INITIAL_RETRY="+retry)
		out, err := cmd.CombinedOutput()
		if (err == nil) != ok || !strings.Contains(string(out), want) {
			t.Fatalf("success=%v want=%q error=%v\n%s", ok, want, err, out)
		}
		return string(out)
	}
	manifest := ""
	add := func(path, body string) {
		t.Helper()
		write(path, body)
		manifest += path + "\n"
		write("db/migrations/order.txt", manifest)
	}
	releasePath := "db/migrations/release/issue_11_12_release.sql"
	pinGlidePath := "db/migrations/pin_glide/issue_11_12_release.sql"
	// These would fail if initial-install code executed them. They remain valid,
	// immutable historical files whose fingerprints must be checked on every run.
	add(releasePath, "SELECT 1/0;")
	add(pinGlidePath, "SELECT 2/0;")
	write("db/schema/pin_glide/20_tables/release_probe.sql", probeSchema)
	add("db/migrations/pin_glide/issue_11_13_prepare.sql", "CREATE TABLE IF NOT EXISTS pin_glide.release_probe (id integer, message text NOT NULL, legacy_amount text NOT NULL DEFAULT '0', CONSTRAINT pk_release_probe PRIMARY KEY (id)); INSERT INTO pin_glide.release_probe (id, message) VALUES (1,'required') ON CONFLICT (id) DO NOTHING;")
	add("db/scripts/issue_11_13_prepare.sql", "INSERT INTO pin_glide.release_probe (id, message) VALUES (1,'required') ON CONFLICT (id) DO NOTHING;")
	initial = "false"
	migrate(false, "upgrade requires an existing Database Release")
	initial = "true"
	migrate(true, "complete")
	if got := text("SELECT count(*) FROM release.release_migration_scripts WHERE executed_by='skipped' AND duration_ms=0 AND sql_body IS NOT NULL"); got != "3" {
		t.Fatal(got)
	}
	if got := text("SELECT count(DISTINCT target_schema) FROM release.release_migration_scripts WHERE filename='issue_11_12_release.sql'"); got != "2" {
		t.Fatal("schema identity collision:", got)
	}
	if got := text("SELECT message FROM pin_glide.release_probe WHERE id=1"); got != "required" {
		t.Fatal(got)
	}
	migrate(false, "database already deployed")
	retry = "true"
	migrate(true, "complete")
	if got := text("SELECT count(*) FROM release.release_deploy_history"); got != "2" {
		t.Fatal("same-version milestone did not append:", got)
	}
	version = "1.0.1"
	migrate(false, "database already deployed")
	initial, retry = "false", "false"
	write(releasePath, "SELECT 3/0;")
	migrate(false, "changed or is missing")
	write(releasePath, "SELECT 1/0;")
	must(t, os.Remove(filepath.Join(work, pinGlidePath)))
	write("db/migrations/order.txt", strings.ReplaceAll(manifest, pinGlidePath+"\n", ""))
	migrate(false, "changed or is missing")
	write(pinGlidePath, "SELECT 2/0;")
	write("db/migrations/order.txt", manifest)

	// A committed release script has no ledger row until both sync calls succeed.
	sql("CREATE SCHEMA neighbor; CREATE TABLE neighbor.effects(id integer PRIMARY KEY); CREATE TABLE release.unmanaged_failure(id integer)")
	add("db/migrations/release/issue_11_16_replay.sql", "INSERT INTO neighbor.effects VALUES (6) ON CONFLICT (id) DO NOTHING;")
	migrate(false, "release schema did not converge")
	if got := text("SELECT count(*) FROM neighbor.effects"); got != "1" {
		t.Fatal(got)
	}
	if got := text("SELECT count(*) FROM release.release_migration_scripts WHERE filename='issue_11_16_replay.sql'"); got != "0" {
		t.Fatal("recorded before sync", got)
	}
	sql("DROP TABLE release.unmanaged_failure")
	migrate(true, "complete")
	if got := text("SELECT count(*) FROM neighbor.effects"); got != "1" {
		t.Fatal("release replay duplicated effects", got)
	}

	// The second accepted replay window is AFTER sync but BEFORE ledger commit.
	ledgerPath := "db/schema/release/20_tables/release_migration_scripts.sql"
	ledger, err := os.ReadFile(filepath.Join(work, ledgerPath))
	must(t, err)
	constrained := strings.Replace(string(ledger), "PRIMARY KEY (target_schema, filename)", "PRIMARY KEY (target_schema, filename),\n CONSTRAINT chk_backfill CHECK (filename <> 'issue_11_17_backfill.sql')", 1)
	write(ledgerPath, constrained)
	add("db/migrations/release/issue_11_17_backfill.sql", "INSERT INTO neighbor.effects VALUES (7) ON CONFLICT (id) DO NOTHING;")
	migrate(false, "record release scripts")
	if got := text("SELECT count(*) FROM neighbor.effects"); got != "2" {
		t.Fatal(got)
	}
	sql("ALTER TABLE release.release_migration_scripts DROP CONSTRAINT chk_backfill")
	write(ledgerPath, string(ledger))
	migrate(true, "complete")
	if got := text("SELECT count(*) FROM neighbor.effects"); got != "2" {
		t.Fatal("backfill retry duplicated effects", got)
	}

	// Application SQL and its ledger share one transaction; prior successful
	// files remain, and the Database Release milestone is not written early.
	add("db/migrations/pin_glide/issue_11_18_saved.sql", "INSERT INTO neighbor.effects VALUES (8) ON CONFLICT (id) DO NOTHING;")
	add("db/migrations/pin_glide/issue_11_19_failed.sql", "INSERT INTO neighbor.effects VALUES (9) ON CONFLICT (id) DO NOTHING; SELECT 1/0;")
	before := text("SELECT count(*) FROM release.release_deploy_history")
	migrate(false, "issue_11_19_failed.sql")
	if got := text("SELECT count(*) FROM neighbor.effects WHERE id IN (8,9)"); got != "1" {
		t.Fatal("script transaction broken", got)
	}
	if got := text("SELECT count(*) FROM release.release_deploy_history"); got != before {
		t.Fatal("premature history", got)
	}
	write("db/migrations/pin_glide/issue_11_19_failed.sql", "INSERT INTO neighbor.effects VALUES (9) ON CONFLICT (id) DO NOTHING;")
	add("db/scripts/issue_11_20_failed.sql", "INSERT INTO neighbor.effects VALUES (10) ON CONFLICT (id) DO NOTHING; SELECT 1/0;")
	migrate(false, "issue_11_20_failed.sql")
	if got := text("SELECT release_version FROM release.release_deploy_history ORDER BY id DESC LIMIT 1"); got != "1.0.1" {
		t.Fatal("milestone lost on data failure", got)
	}
	if got := text("SELECT count(*) FROM neighbor.effects WHERE id=10"); got != "0" {
		t.Fatal("failed data persisted", got)
	}
	if got := text("SELECT count(*) FROM release.release_data_scripts WHERE filename='issue_11_20_failed.sql'"); got != "0" {
		t.Fatal("failed data recorded", got)
	}
	write("db/scripts/issue_11_20_failed.sql", "INSERT INTO neighbor.effects VALUES (10) ON CONFLICT (id) DO NOTHING;")
	migrate(true, "complete")
	// Attribution is intentionally not an enum; skipped/manual records stay immutable.
	sql("UPDATE release.release_migration_scripts SET executed_by='operator verified' WHERE filename='issue_11_18_saved.sql'")
	migrate(true, "complete")
	if got := text("SELECT count(*) FROM release.release_schema_audits WHERE duration_ms IS NULL OR executed_by <> 'system'"); got != "0" {
		t.Fatal(got)
	}
	if got := text("SELECT count(*) FROM release.release_migration_scripts WHERE duration_ms IS NULL"); got != "0" {
		t.Fatal(got)
	}
	if got := text("SELECT count(*) FROM release.release_data_scripts WHERE duration_ms IS NULL"); got != "0" {
		t.Fatal(got)
	}
}
