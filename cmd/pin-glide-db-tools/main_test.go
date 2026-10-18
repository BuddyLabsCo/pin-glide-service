package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Regression: the CLI must reject unsupported modes before database access and
// report invalid snapshot configuration without exposing connection credentials.
func TestCLI(t *testing.T) {
	if os.Getenv("PIN_GLIDE_DB_TOOLS_SUBPROCESS") == "1" {
		os.Args = append([]string{"pin-glide-db-tools"}, strings.Split(os.Getenv("PIN_GLIDE_DB_TOOLS_ARGS"), " ")...)
		main()
		os.Exit(0)
	}
	for _, test := range []struct {
		name, args, keep, path, database, want string
		status                                 int
	}{
		{name: "unknown mode", args: "unknown", want: "usage: pin-glide-db-tools <migrate|snapshot>", status: 2},
		{name: "extra argument", args: "snapshot extra", want: "usage:", status: 2},
		{name: "missing database", args: "snapshot", keep: "1", path: t.TempDir(), want: "PIN_GLIDE_DATABASE_URL", status: 1},
		{name: "invalid retention", args: "snapshot", keep: "invalid", path: t.TempDir(), database: "postgres://pin_glide:secret@db/pin_glide", want: "PIN_GLIDE_SNAPSHOT_KEEP", status: 1},
		{name: "missing retention", args: "snapshot", path: t.TempDir(), database: "postgres://pin_glide:secret@db/pin_glide", want: "PIN_GLIDE_SNAPSHOT_KEEP", status: 1},
		{name: "zero retention", args: "snapshot", keep: "0", path: t.TempDir(), database: "postgres://pin_glide:secret@db/pin_glide", want: "PIN_GLIDE_SNAPSHOT_KEEP", status: 1},
		{name: "missing path", args: "snapshot", keep: "1", database: "postgres://pin_glide:secret@db/pin_glide", want: "PIN_GLIDE_SNAPSHOT_PATH", status: 1},
		{name: "migration configuration", args: "migrate", want: "load configuration: PIN_GLIDE_DATABASE_URL is required in production", status: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestCLI$")
			command.Env = []string{
				"PIN_GLIDE_DB_TOOLS_SUBPROCESS=1", "PIN_GLIDE_DB_TOOLS_ARGS=" + test.args,
				"PIN_GLIDE_ENV=production",
				"PIN_GLIDE_DATABASE_URL=" + test.database, "PIN_GLIDE_SNAPSHOT_PATH=" + test.path, "PIN_GLIDE_SNAPSHOT_KEEP=" + test.keep,
			}
			output, err := command.CombinedOutput()
			if err == nil || command.ProcessState.ExitCode() != test.status {
				t.Fatalf("exit = %v, wanted %d; output: %s", err, test.status, output)
			}
			if !strings.Contains(string(output), test.want) || strings.Contains(string(output), "secret") {
				t.Fatalf("unexpected CLI output: %s", output)
			}
		})
	}
}
