//go:build integration

// The scheduled cleanup job itself: one stable owner and name through create,
// update, active state and retirement, verified as database metadata because
// Schema Sync reconciles neither cron.job rows nor extension versions, plus the
// privileges that keep the configuration and the routine to the release owner.
package privacy_test

import (
	"context"
	"database/sql"
	"testing"
)

// The adopted declaration of the one scheduled cleanup job.
const (
	retentionJobName     = "pin-glide-retention-cleanup"
	retentionJobSchedule = "0 19 * * *"
	retentionJobCommand  = "SELECT pin_glide.pin_glide_fn_cleanup_expired_rows(now())"
)

// scheduledJob is one cron.job row as this test reads it.
type scheduledJob struct {
	jobID    int64
	schedule string
	command  string
	database string
	username string
	active   bool
}

// job reads the named scheduled job and reports whether it exists.
func (e *environment) job(t *testing.T, name string) (scheduledJob, bool) {
	t.Helper()

	var found scheduledJob
	err := e.pool.QueryRowContext(context.Background(), `
SELECT jobid, schedule, command, database, username, active
FROM cron.job
WHERE jobname = $1`, name).
		Scan(&found.jobID, &found.schedule, &found.command, &found.database,
			&found.username, &found.active)
	if err == sql.ErrNoRows {
		return scheduledJob{}, false
	}
	if err != nil {
		t.Fatalf("read the %s job: %v", name, err)
	}
	return found, true
}

// extensionVersion reads one installed extension version, which Schema Sync
// compares by name alone and never upgrades.
func (e *environment) extensionVersion(t *testing.T, name string) string {
	t.Helper()

	var version string
	if err := e.pool.QueryRowContext(context.Background(),
		`SELECT extversion FROM pg_extension WHERE extname = $1`, name).Scan(&version); err != nil {
		t.Fatalf("read the %s extension version: %v", name, err)
	}
	return version
}

// TestScheduledCleanupJobConvergesThroughCreateUpdateActiveAndRetirement shows
// the shipped data scripts converging one stable owner and name whenever a
// release applies them: a rerun keeps the job identity, a mutated or missing
// job row is converged rather than duplicated, and a job under the service's
// own name prefix that the release no longer declares is retired while an
// unrelated operator job is left alone. The release records each script and
// never applies it a second time, so these applications stand in for the
// release that ships the script.
func TestScheduledCleanupJobConvergesThroughCreateUpdateActiveAndRetirement(t *testing.T) {
	environment := start(t)
	environment.applyDataScripts(t)

	created, found := environment.job(t, retentionJobName)
	if !found {
		t.Fatalf("the shipped data scripts scheduled no %s job", retentionJobName)
	}
	var currentDatabase, currentUser string
	if err := environment.pool.QueryRowContext(context.Background(),
		`SELECT current_database(), current_user`).Scan(&currentDatabase, &currentUser); err != nil {
		t.Fatalf("read the owning database and user: %v", err)
	}
	adopted := scheduledJob{
		jobID:    created.jobID,
		schedule: retentionJobSchedule,
		command:  retentionJobCommand,
		database: currentDatabase,
		username: currentUser,
		active:   true,
	}
	if created != adopted {
		t.Errorf("the scheduled job = %+v, want %+v", created, adopted)
	}

	// A second application of the same immutable scripts is the same release
	// state: the job keeps its identity, so pg_cron never carries two cleanup
	// jobs for one name and owner.
	environment.applyDataScripts(t)
	rerun, found := environment.job(t, retentionJobName)
	if !found || rerun != adopted {
		t.Errorf("the rerun left %+v (found %t), want the adopted job unchanged", rerun, found)
	}

	// The extension version is deployed infrastructure metadata: the schema
	// check compares the extension by name and never upgrades it.
	if version := environment.extensionVersion(t, "pg_cron"); version != "1.6" {
		t.Errorf("pg_cron version = %q, want the 1.6 the image preloads", version)
	}

	// Mutation: a job row edited outside the release is converged by the next
	// application of the script that declares it.
	environment.exec(t, `
UPDATE cron.job
SET schedule = '0 0 1 1 *', command = 'SELECT 1', active = false
WHERE jobname = $1`, retentionJobName)
	environment.applyDataScripts(t)
	if mutated, found := environment.job(t, retentionJobName); !found || mutated != adopted {
		t.Errorf("the converged job = %+v (found %t), want %+v", mutated, found, adopted)
	}

	// Deletion: a job removed outside the release is declared again by the
	// script that owns it, because deleting a declaration never removes a row
	// by itself.
	environment.exec(t, `SELECT cron.unschedule($1)`, retentionJobName)
	if _, found := environment.job(t, retentionJobName); found {
		t.Fatal("the unscheduled cleanup job still has a row")
	}
	environment.applyDataScripts(t)
	// A job created again necessarily has a new row identity; every declared
	// field is the adopted one.
	redeclared := adopted
	if recreated, found := environment.job(t, retentionJobName); found {
		redeclared.jobID = recreated.jobID
		if recreated != redeclared {
			t.Errorf("the declared job = %+v, want %+v", recreated, redeclared)
		}
	} else {
		t.Error("the script that declares the cleanup job scheduled nothing")
	}

	// Retirement: the service owns the pin-glide- job-name prefix, so a job
	// under it that no shipped script declares is removed, while a job an
	// operator owns keeps running.
	environment.exec(t, `SELECT cron.schedule('pin-glide-retired-probe', '0 0 1 1 *', 'SELECT 1')`)
	environment.exec(t, `SELECT cron.schedule('retention_probe_operator', '0 0 1 1 *', 'SELECT 1')`)
	environment.applyDataScripts(t)
	if _, found := environment.job(t, "pin-glide-retired-probe"); found {
		t.Error("a retired service job survived the release that no longer declares it")
	}
	if _, found := environment.job(t, retentionJobName); !found {
		t.Error("retiring the stale job also removed the declared one")
	}
	if _, found := environment.job(t, "retention_probe_operator"); !found {
		t.Error("retirement removed a job outside the service's own name prefix")
	}
}

// TestScheduledCleanupPrivilegesAreConstrainedToTheReleaseOwner shows the
// configured target list and the dynamic routine as release-owned: a role that
// is not the owner can neither write the cleanup configuration nor invoke the
// DELETE routine, so it cannot point cleanup at another table or run it.
func TestScheduledCleanupPrivilegesAreConstrainedToTheReleaseOwner(t *testing.T) {
	environment := start(t)
	environment.applyDataScripts(t)

	environment.exec(t, `CREATE ROLE retention_probe`)
	t.Cleanup(func() {
		if _, err := environment.pool.ExecContext(context.Background(),
			`DROP ROLE retention_probe`); err != nil {
			t.Errorf("drop the privilege probe role: %v", err)
		}
	})

	for name, query := range map[string]string{
		"write the cleanup configuration": `
SELECT has_table_privilege('retention_probe', 'pin_glide.retention_policies',
    'INSERT, UPDATE, DELETE')`,
		"run the cleanup routine": `
SELECT has_function_privilege('retention_probe',
    'pin_glide.pin_glide_fn_cleanup_expired_rows(timestamptz)', 'EXECUTE')`,
	} {
		var allowed bool
		if err := environment.pool.QueryRowContext(context.Background(), query).Scan(&allowed); err != nil {
			t.Fatalf("read whether the probe role may %s: %v", name, err)
		}
		if allowed {
			t.Errorf("a role outside the release owner may %s", name)
		}
	}
}
