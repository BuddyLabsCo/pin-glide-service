//go:build integration

package release_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression: an outer CLI error must not replace the child's raw exit status,
// truncate/merge output, create imaginary checks, or continue without an audit.
// Seam: the migrate executable and an external psqldef boundary; PostgreSQL and
// normal schema synchronization are real, only selected child failures are injected.
func TestSchemaAuditFailures(t *testing.T) {
	_, work, bin, pool, dsn := releaseDatabase(t)
	process := releaseProcess{work: work, bin: bin, dsn: dsn, version: "2.0.0", initial: "true", retry: "false"}
	writeWorkFile(t, work, "db/schema/pin_glide/20_tables/release_probe.sql", probeSchema)
	writeWorkFile(t, work, "db/scripts/issue_11_1_release_probe.sql", probeSeed)
	writeWorkFile(t, work, "db/migrations/order.txt", "db/scripts/issue_11_1_release_probe.sql\n")
	process.migrate(t, true, "complete")
	process.initial = "false"
	must(t, os.Rename(filepath.Join(bin, "psqldef"), filepath.Join(bin, "psqldef-real")))
	inject := func(schema, mode string, code int) {
		t.Helper()
		body := fmt.Sprintf(`#!/bin/sh
case "$*" in
 *"target_schema: %s"*"--%s"*)
  head -c 131072 /dev/zero | tr '\000' O
  head -c 98304 /dev/zero | tr '\000' E >&2
  exit %d;;
esac
exec "$(dirname "$0")/psqldef-real" "$@"
`, schema, mode, code)
		must(t, os.WriteFile(filepath.Join(bin, "psqldef"), []byte(body), 0755))
	}
	for i, c := range []struct {
		schema, mode string
		code         int
	}{{"release", "apply", 23}, {"release", "check", 19}, {"pin_glide", "apply", 17}, {"pin_glide", "check", 13}} {
		t.Run(c.schema+"_"+c.mode, func(t *testing.T) {
			var before int
			must(t, pool.QueryRow("SELECT count(*) FROM release.release_schema_audits").Scan(&before))
			inject(c.schema, c.mode, c.code)
			output := process.migrate(t, false, fmt.Sprintf("exit status %d", c.code))
			var after, exit, duration int
			var schema, mode, stdout, stderr, executor string
			must(t, pool.QueryRow("SELECT count(*) FROM release.release_schema_audits").Scan(&after))
			if after-before != i+1 {
				t.Fatalf("expected %d actual invocations, got %d", i+1, after-before)
			}
			must(t, pool.QueryRow("SELECT target_schema,operation,stdout,stderr,exit_code,duration_ms,executed_by FROM release.release_schema_audits ORDER BY id DESC LIMIT 1").Scan(&schema, &mode, &stdout, &stderr, &exit, &duration, &executor))
			if schema != c.schema || mode != c.mode || exit != c.code || stdout != strings.Repeat("O", 131072) || stderr != strings.Repeat("E", 98304) || duration < 0 || executor != "system" {
				t.Fatalf("audit changed child result: %s %s exit=%d stdout=%d stderr=%d duration=%d executor=%s", schema, mode, exit, len(stdout), len(stderr), duration, executor)
			}
			if strings.Contains(output, "phase data scripts") {
				t.Fatal("failed schema sync proceeded to data")
			}
			var history int
			must(t, pool.QueryRow("SELECT count(*) FROM release.release_deploy_history").Scan(&history))
			if history != 1 {
				t.Fatal("failed sync wrote a milestone")
			}
		})
	}
	// Even an exit-zero apply must stop if its real audit cannot be persisted.
	_, err := pool.Exec("CREATE SCHEMA audit_test; ALTER TABLE release.release_schema_audits SET SCHEMA audit_test")
	must(t, err)
	inject("release", "apply", 0)
	output := process.migrate(t, false, "persist schema audit")
	if strings.Contains(output, "phase psqldef release --check") {
		t.Fatal("audit loss did not stop the pipeline")
	}
	var audit struct {
		Schema    string `json:"target_schema"`
		Operation string `json:"operation"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		Exit      *int   `json:"exit_code"`
	}
	// Raw stdout has no final newline; the structured fallback starts at this key.
	start := strings.Index(output, `{"target_schema":`)
	if start < 0 {
		t.Fatal("missing structured audit fallback")
	}
	must(t, json.NewDecoder(strings.NewReader(output[start:])).Decode(&audit))
	if audit.Schema != "release" || audit.Operation != "apply" || audit.Exit == nil || *audit.Exit != 0 || len(audit.Stdout) != 131072 || len(audit.Stderr) != 98304 {
		t.Fatalf("incomplete fallback audit: schema=%s operation=%s stdout=%d stderr=%d", audit.Schema, audit.Operation, len(audit.Stdout), len(audit.Stderr))
	}
}
