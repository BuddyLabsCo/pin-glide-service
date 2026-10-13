//go:build integration

package release_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The application schema these release tests converge against. The repository
// ships no business tables yet, so each test supplies its own probe.
const probeSchema = `CREATE TABLE IF NOT EXISTS pin_glide.release_probe (
    id integer,
    message text NOT NULL,
    legacy_amount text NOT NULL DEFAULT '0',
    CONSTRAINT pk_release_probe PRIMARY KEY (id)
);
`

const probeSeed = "INSERT INTO pin_glide.release_probe (id, message) VALUES (1, 'initial value') ON CONFLICT (id) DO NOTHING;"

// Regression: upgrading previously treated CREATE TABLE IF NOT EXISTS as a schema
// migration, and retries had no durable application-migration ledger.
// Seam: the deployed pin-glide-release migrate process, its bundled SQL, and an
// isolated PostgreSQL. Observe exit status, phases, preserved data, and
// convergence; no existing release test exercises a real database or this
// public command.
func TestMigrateReleasePipeline(t *testing.T) {
	_, work, bin, pool, dsn := releaseDatabase(t)
	process := releaseProcess{work: work, bin: bin, dsn: dsn, version: "0.1.0", initial: "true", retry: "false"}

	runSQL := func(query string) { t.Helper(); _, err := pool.Exec(query); must(t, err) }
	text := func(query string) string {
		t.Helper()
		var value string
		must(t, pool.QueryRow(query).Scan(&value))
		return value
	}

	// The first deployment establishes both schemas and the seed, and the four
	// psqldef invocations stay independently visible in fixed phase order.
	writeWorkFile(t, work, "db/schema/pin_glide/20_tables/release_probe.sql", probeSchema)
	writeWorkFile(t, work, "db/scripts/issue_11_1_release_probe.sql", probeSeed)
	writeWorkFile(t, work, "db/migrations/order.txt", "db/scripts/issue_11_1_release_probe.sql\n")
	output := process.migrate(t, true, "complete")
	process.initial = "false"
	if got := text("SELECT count(*) FROM release.release_schema_audits"); got != "4" {
		t.Fatalf("expected four independent schema audits, got %s", got)
	}
	last := -1
	for _, phase := range []string{"phase release migrations", "phase psqldef release --apply", "phase psqldef release --check", "phase pin_glide migrations", "phase psqldef pin_glide --apply", "phase psqldef pin_glide --check", "phase data scripts", "pin-glide-release migrate: complete"} {
		index := strings.Index(output, phase)
		if index <= last {
			t.Fatalf("missing or out-of-order phase %q: %s", phase, output)
		}
		last = index
	}
	if got := text("SELECT message FROM pin_glide.release_probe WHERE id=1"); got != "initial value" {
		t.Fatal(got)
	}
	runSQL("CREATE SCHEMA neighbor; CREATE TABLE neighbor.keep (id integer); INSERT INTO neighbor.keep VALUES (7)")
	runSQL("UPDATE pin_glide.release_probe SET message='operator value'")
	databaseIdentity := text("SELECT oid FROM pg_database WHERE datname = current_database()")
	process.migrate(t, true, "complete")
	if got := text("SELECT message FROM pin_glide.release_probe WHERE id=1"); got != "operator value" {
		t.Fatalf("seed overwrote data: %s", got)
	}

	// Commit order is not global filename order. A schema extension must be
	// applied after explicit migrations, and neither may touch neighbor schemas.
	schemaPath := "db/schema/pin_glide/20_tables/release_probe.sql"
	upgraded := strings.Replace(probeSchema, "message text NOT NULL,", "message text NOT NULL,\n    memo text,", 1)
	writeWorkFile(t, work, schemaPath, upgraded)
	writeWorkFile(t, work, "db/scripts/issue_11_11_first.sql", "UPDATE pin_glide.release_probe SET message = message || '-first' WHERE message NOT LIKE '%-first%';")
	writeWorkFile(t, work, "db/scripts/issue_11_12_second.sql", "UPDATE pin_glide.release_probe SET message = message || '-second' WHERE message NOT LIKE '%-second%';")
	order := "db/scripts/issue_11_1_release_probe.sql\ndb/scripts/issue_11_11_first.sql\ndb/scripts/issue_11_12_second.sql\n"
	writeWorkFile(t, work, "db/migrations/order.txt", order)
	process.migrate(t, true, "complete")
	runSQL("UPDATE pin_glide.release_probe SET memo='upgrade applied'")
	process.migrate(t, true, "already processed")
	if got := text("SELECT message FROM pin_glide.release_probe WHERE id=1"); got != "operator value-first-second" {
		t.Fatalf("wrong order or duplicate execution: %s", got)
	}
	if got := text("SELECT count(*) FROM release.release_data_scripts"); got != "3" {
		t.Fatalf("ledger lost: %s", got)
	}
	if got := text("SELECT id FROM neighbor.keep"); got != "7" {
		t.Fatal("neighbor schema modified")
	}

	// A later migration failing retains earlier commits, rolls back the failing
	// file and resumes safely after correction without replaying committed work.
	writeWorkFile(t, work, "db/scripts/issue_11_4_saved.sql", "UPDATE pin_glide.release_probe SET message = message || '-saved' WHERE message NOT LIKE '%-saved%';")
	writeWorkFile(t, work, "db/scripts/issue_11_5_retry.sql", "UPDATE pin_glide.release_probe SET message = message || '-bad' WHERE message NOT LIKE '%-bad%'; SELECT 1/0;")
	order += "db/scripts/issue_11_4_saved.sql\ndb/scripts/issue_11_5_retry.sql\n"
	writeWorkFile(t, work, "db/migrations/order.txt", order)
	output = process.migrate(t, false, "issue_11_5_retry.sql")
	if strings.Contains(output, "migrate: complete") {
		t.Fatal("script failure did not stop pipeline")
	}
	if got := text("SELECT message FROM pin_glide.release_probe WHERE id=1"); got != "operator value-first-second-saved" {
		t.Fatalf("incorrect partial commit: %s", got)
	}
	// The enum-looking string literal is not an AST node type; the guard must
	// read the parsed tree rather than the script text.
	writeWorkFile(t, work, "db/scripts/issue_11_5_retry.sql", "UPDATE pin_glide.release_probe SET message = message || '-retried' WHERE message NOT LIKE '%-retried%'; SELECT 'OBJECT_DATABASE';")
	process.migrate(t, true, "complete")
	process.migrate(t, true, "already processed")
	if got := text("SELECT message FROM pin_glide.release_probe WHERE id=1"); got != "operator value-first-second-saved-retried" {
		t.Fatal(got)
	}

	writeWorkFile(t, work, "db/scripts/issue_11_11_first.sql", "SELECT 1;")
	process.migrate(t, false, "changed or is missing")
	writeWorkFile(t, work, "db/scripts/issue_11_11_first.sql", "UPDATE pin_glide.release_probe SET message = message || '-first' WHERE message NOT LIKE '%-first%';")
	must(t, os.Remove(filepath.Join(work, "db/scripts/issue_11_11_first.sql")))
	writeWorkFile(t, work, "db/migrations/order.txt", strings.ReplaceAll(order, "db/scripts/issue_11_11_first.sql\n", ""))
	process.migrate(t, false, "changed or is missing")
	writeWorkFile(t, work, "db/scripts/issue_11_11_first.sql", "UPDATE pin_glide.release_probe SET message = message || '-first' WHERE message NOT LIKE '%-first%';")
	writeWorkFile(t, work, "db/migrations/order.txt", order)

	// Explicit external effects and transaction control are rejected rather than
	// committing data without its ledger entry or changing shared cluster state.
	for _, statement := range []string{"COMMIT;", "CREATE ROLE release_forbidden;", "ALTER DATABASE pin_glide RENAME TO broken;", "ALTER SYSTEM SET max_connections = '999';", "ALTER TABLE pin_glide.release_probe SET TABLESPACE pg_default;"} {
		t.Logf("reject unsafe release SQL: %s", statement)
		writeWorkFile(t, work, "db/scripts/issue_11_6_forbidden.sql", statement)
		writeWorkFile(t, work, "db/migrations/order.txt", order+"db/scripts/issue_11_6_forbidden.sql\n")
		process.migrate(t, false, "release SQL")
	}
	must(t, os.Remove(filepath.Join(work, "db/scripts/issue_11_6_forbidden.sql")))
	writeWorkFile(t, work, "db/migrations/order.txt", order)
	if got := text("SELECT count(*) FROM pg_roles WHERE rolname='release_forbidden'"); got != "0" {
		t.Fatal("created a global role")
	}

	// The process refuses overlapping migrations for the same database.
	connection, err := pool.Conn(context.Background())
	must(t, err)
	_, err = connection.ExecContext(context.Background(), "SELECT pg_advisory_lock(719417, 63)")
	must(t, err)
	process.migrate(t, false, "another Pin Glide migration is running")
	_, err = connection.ExecContext(context.Background(), "SELECT pg_advisory_unlock(719417, 63)")
	must(t, err)
	must(t, connection.Close())

	// A real psqldef apply failure must not proceed to seeds or claim convergence.
	writeWorkFile(t, work, schemaPath, strings.Replace(upgraded, "memo text,", "memo text,\n    required_value text NOT NULL,", 1))
	output = process.migrate(t, false, "psqldef pin_glide --apply")
	if strings.Contains(output, "phase data scripts") {
		t.Fatal("apply failure continued to seeds")
	}
	writeWorkFile(t, work, schemaPath, upgraded)
	process.migrate(t, true, "complete")
	// psqldef deliberately does not enable destructive drops. Its check must
	// reject a skipped change instead of reporting a falsely converged database.
	runSQL("CREATE TABLE pin_glide.unmanaged_test (id integer)")
	process.migrate(t, false, "schema did not converge")
	runSQL("DROP TABLE pin_glide.unmanaged_test")
	process.migrate(t, true, "complete")
	// The migration ledger is an application-owned schema object too. Unsafe
	// drift must fail the public convergence check without losing its records.
	runSQL("ALTER TABLE release.release_data_scripts ADD COLUMN unmanaged_test text")
	process.migrate(t, false, "schema did not converge")
	runSQL("ALTER TABLE release.release_data_scripts DROP COLUMN unmanaged_test")
	process.migrate(t, true, "already processed")
	if got := text("SELECT count(*) FROM release.release_data_scripts"); got != "5" {
		t.Fatalf("ledger lost during convergence: %s", got)
	}
	if got := text("SELECT oid FROM pg_database WHERE datname = current_database()"); got != databaseIdentity {
		t.Fatal("database identity changed")
	}

	// A destructive representation change is prepared by an immutable structural
	// Migration Script, because psqldef issues no destructive change itself.
	runSQL("UPDATE pin_glide.release_probe SET legacy_amount='42'")
	writeWorkFile(t, work, schemaPath, strings.Replace(upgraded, "legacy_amount text NOT NULL DEFAULT '0',", "amount_cents integer NOT NULL,", 1))
	writeWorkFile(t, work, "db/migrations/pin_glide/issue_11_7_cutover.sql", `ALTER TABLE pin_glide.release_probe ADD COLUMN IF NOT EXISTS amount_cents integer;
UPDATE pin_glide.release_probe SET amount_cents = legacy_amount::integer WHERE amount_cents IS NULL;
ALTER TABLE pin_glide.release_probe ALTER COLUMN amount_cents SET NOT NULL;
ALTER TABLE pin_glide.release_probe DROP COLUMN legacy_amount;`)
	order += "db/migrations/pin_glide/issue_11_7_cutover.sql\n"
	writeWorkFile(t, work, "db/migrations/order.txt", order)
	process.migrate(t, true, "complete")
	if got := text("SELECT amount_cents FROM pin_glide.release_probe WHERE id=1"); got != "42" {
		t.Fatalf("representation cutover reset retained data: %s", got)
	}
	if got := text("SELECT count(*) FROM information_schema.columns WHERE table_schema='pin_glide' AND table_name='release_probe' AND column_name='legacy_amount'"); got != "0" {
		t.Fatal("superseded representation survived convergence")
	}
	process.migrate(t, true, "already processed")
	if got := text("SELECT count(*) FROM neighbor.keep"); got != "1" {
		t.Fatal("neighbor schema modified by cutover")
	}

	// Local Git must use the same within-commit filename order as published
	// manifests, even when the operator configures a reversed diff.orderFile.
	t.Run("GitOrderIgnoresUserDiffOrder", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(work, "git-config"))
		t.Setenv("GIT_CONFIG_COUNT", "0")
		writeWorkFile(t, work, "git-diff-order", "db/scripts/issue_11_9_git_second.sql\ndb/scripts/issue_11_8_git_first.sql\n")
		run(t, work, "git", "config", "--global", "diff.orderFile", filepath.Join(work, "git-diff-order"))
		run(t, work, "git", "init", "--initial-branch=main")
		run(t, work, "git", "config", "user.name", "Release Test")
		run(t, work, "git", "config", "user.email", "release-test@example.invalid")
		must(t, os.Remove(filepath.Join(work, "db/migrations/order.txt")))
		run(t, work, "git", "add", "db/scripts", "db/migrations")
		run(t, work, "git", "commit", "-m", "Record previously applied migrations")
		writeWorkFile(t, work, "db/scripts/issue_11_8_git_first.sql", "UPDATE pin_glide.release_probe SET message = message || '-git-first' WHERE message NOT LIKE '%-git-first%';")
		writeWorkFile(t, work, "db/scripts/issue_11_9_git_second.sql", "UPDATE pin_glide.release_probe SET message = message || '-git-second' WHERE message NOT LIKE '%-git-second%';")
		run(t, work, "git", "add", "db/scripts")
		run(t, work, "git", "commit", "-m", "Introduce ordered migrations together")
		process.migrate(t, true, "complete")
		process.migrate(t, true, "already processed")
		if got := text("SELECT message FROM pin_glide.release_probe WHERE id=1"); got != "operator value-first-second-saved-retried-git-first-git-second" {
			t.Fatalf("user Git configuration changed migration order or replayed a file: %s", got)
		}
	})
}

func run(t *testing.T, directory, program string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", program, args, err, output)
	}
	return string(output)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(fmt.Errorf("integration setup/assertion: %w", err))
	}
}

// writeWorkFile stages a file in the exported release tree the deployment image
// carries, which is what a published release commit delivers.
func writeWorkFile(t *testing.T, work, path, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(filepath.Join(work, path)), 0755))
	must(t, os.WriteFile(filepath.Join(work, path), []byte(content), 0644))
}

// releaseProcess invokes the deployed release command with the environment an
// operator deployment provides. These tests only vary version and install mode.
type releaseProcess struct {
	work    string
	bin     string
	dsn     string
	version string
	initial string
	retry   string
}

func (p releaseProcess) migrate(t *testing.T, success bool, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(p.bin, "pin-glide-release"), "migrate")
	command.Dir = p.work
	command.Env = append(os.Environ(), "PATH="+p.bin+":"+os.Getenv("PATH"), "PIN_GLIDE_ENV=production", "PIN_GLIDE_DATABASE_URL="+p.dsn, "PIN_GLIDE_RELEASE_VERSION="+p.version, "PIN_GLIDE_RELEASE_INITIAL="+p.initial, "PIN_GLIDE_RELEASE_INITIAL_RETRY="+p.retry)
	output, err := command.CombinedOutput()
	if (err == nil) != success || !strings.Contains(string(output), want) {
		t.Fatalf("migrate success=%v want=%q error=%v\n%s", success, want, err, output)
	}
	return string(output)
}

func releaseDatabase(t *testing.T) (string, string, string, *sql.DB, string) {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	bin := filepath.Join(work, "bin")
	must(t, os.MkdirAll(bin, 0755))
	run(t, root, "go", "build", "-o", filepath.Join(bin, "pin-glide-release"), "./cmd/pin-glide-release")
	run(t, root, "go", "build", "-o", filepath.Join(bin, "psqldef"), "github.com/sqldef/sqldef/v3/cmd/psqldef")
	// A published image carries the exported release SQL; the test stands in for
	// it by copying the repository's SQL next to a Git-derived manifest.
	must(t, filepath.WalkDir(filepath.Join(root, "db"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(work, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0644)
	}))
	must(t, os.MkdirAll(filepath.Join(work, "db/migrations"), 0755))
	must(t, os.MkdirAll(filepath.Join(work, "scripts/release"), 0755))
	orderTool, err := os.ReadFile(filepath.Join(root, "scripts/release/migration-order.sh"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(work, "scripts/release/migration-order.sh"), orderTool, 0755))

	postgres := harness.StartPostgres(t)
	pool, err := sql.Open("pgx", postgres.DSN)
	must(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	return root, work, bin, pool, postgres.DSN
}
