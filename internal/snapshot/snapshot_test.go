package snapshot_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/snapshot"
)

// Regression: a usable snapshot must preserve pg_dump's safe export settings and
// appear only as a completed archive. No existing test covers this public seam.
func TestSnapshotPublishesCustomArchive(t *testing.T) {
	tools := t.TempDir()
	writeExecutable(t, tools, "pg_dump", `
test "$PGPASSWORD" = 'secret' || exit 10
test "$#" = 4 || exit 11
test "$4" = '--dbname=postgres://pin_glide@db/pin_glide' || exit 17
test "$1" = '--format=custom' || exit 12
test "$2" = '--no-security-labels' || exit 13
test "$3" = '--exclude-extension=anon' || exit 14
printf 'PGDMP-archive'
`)
	writeExecutable(t, tools, "pg_restore", `
test "$1" = '--list' || exit 15
test -s "$2" || exit 16
`)
	t.Setenv("PATH", tools)
	directory := t.TempDir()
	name, err := snapshot.Snapshot(context.Background(), snapshot.Options{
		DatabaseURL: "postgres://pin_glide:secret@db/pin_glide", Path: directory, Keep: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(name) != directory {
		t.Fatalf("snapshot path = %q", name)
	}
	pattern := regexp.MustCompile(`^pin-glide-snapshot-(\d{8}T\d{6}\.\d{9}Z)-[0-9]+\.dump$`)
	parts := pattern.FindStringSubmatch(filepath.Base(name))
	if parts == nil {
		t.Fatalf("snapshot name = %q", name)
	}
	if _, err := time.Parse("20060102T150405.000000000Z", parts[1]); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(name)
	if err != nil || string(contents) != "PGDMP-archive" {
		t.Fatalf("archive = %q, error = %v", contents, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot directory = %v, error = %v", entries, err)
	}
}

// Regression: failed, empty, invalid, and cancelled exports must not publish an
// archive or prune the last usable backup, and errors must not expose credentials.
func TestSnapshotFailurePreservesBackupsAndRedactsErrors(t *testing.T) {
	for _, test := range []struct {
		name, dump, restore string
		cancelled           bool
	}{
		{name: "dump failure", dump: "printf 'postgres://pin_glide:secret@db/pin_glide' >&2; printf partial; exit 1", restore: "exit 0"},
		{name: "empty dump", dump: "exit 0", restore: "exit 0"},
		{name: "invalid dump", dump: "printf invalid", restore: "printf secret >&2; exit 1"},
		{name: "cancelled", dump: "printf archive", restore: "exit 0", cancelled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools := t.TempDir()
			writeExecutable(t, tools, "pg_dump", test.dump)
			writeExecutable(t, tools, "pg_restore", test.restore)
			t.Setenv("PATH", tools)
			directory := t.TempDir()
			previous := "pin-glide-snapshot-20200101T000000.000000000Z-123.dump"
			if err := os.WriteFile(filepath.Join(directory, previous), []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancelled {
				cancel()
			}
			name, err := snapshot.Snapshot(ctx, snapshot.Options{DatabaseURL: "postgres://pin_glide:secret@db/pin_glide", Path: directory, Keep: 1})
			if err == nil || name != "" {
				t.Fatalf("snapshot = %q, error = %v; expected failure", name, err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "postgres://") {
				t.Fatalf("credentials leaked in error: %v", err)
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil || len(entries) != 1 || entries[0].Name() != previous {
				t.Fatalf("backup directory = %v, error = %v", entries, readErr)
			}
		})
	}
}

// Regression: retention must count only this tool's completed snapshots, never
// unrelated backups, temporary files, directories, empty files, or symlinks.
func TestSnapshotRetainsNewestCompletedArchivesOnly(t *testing.T) {
	tools := t.TempDir()
	writeExecutable(t, tools, "pg_dump", "printf archive")
	writeExecutable(t, tools, "pg_restore", "exit 0")
	t.Setenv("PATH", tools)
	directory := t.TempDir()
	files := []string{
		"pin-glide-snapshot-20200101T000000.000000000Z-1.dump",
		"pin-glide-snapshot-20200102T000000.000000000Z-2.dump",
		"pin-glide-snapshot-20200103T000000.000000000Z-3.dump",
		"customer-backup.dump",
		"pin-glide-snapshot-20209999T000000.000000000Z-4.dump",
		".pin-glide-snapshot-123.tmp",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	empty := "pin-glide-snapshot-20200104T000000.000000000Z-4.dump"
	if err := os.WriteFile(filepath.Join(directory, empty), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	child := "pin-glide-snapshot-20200105T000000.000000000Z-5.dump"
	if err := os.Mkdir(filepath.Join(directory, child), 0o700); err != nil {
		t.Fatal(err)
	}
	link := "pin-glide-snapshot-20200106T000000.000000000Z-6.dump"
	if err := os.Symlink(filepath.Join(directory, files[3]), filepath.Join(directory, link)); err != nil {
		t.Fatal(err)
	}
	name, err := snapshot.Snapshot(context.Background(), snapshot.Options{DatabaseURL: "postgres://db/pin_glide", Path: directory, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, deleted := range files[:2] {
		if _, err := os.Lstat(filepath.Join(directory, deleted)); !os.IsNotExist(err) {
			t.Errorf("old archive %q not removed: %v", deleted, err)
		}
	}
	for _, retained := range append(files[2:], empty, child, link, filepath.Base(name)) {
		if _, err := os.Lstat(filepath.Join(directory, retained)); err != nil {
			t.Errorf("retained file %q: %v", retained, err)
		}
	}
}

// Regression: a missing destination must not silently export into the system
// temporary directory, and invalid retention must never delete all backups.
func TestSnapshotRejectsInvalidOptions(t *testing.T) {
	tools := t.TempDir()
	writeExecutable(t, tools, "pg_dump", "printf archive")
	writeExecutable(t, tools, "pg_restore", "exit 0")
	t.Setenv("PATH", tools)
	directory := t.TempDir()
	for _, test := range []struct {
		name    string
		options snapshot.Options
	}{
		{"missing database", snapshot.Options{Path: directory, Keep: 1}},
		{"missing path", snapshot.Options{DatabaseURL: "postgres://db/pin_glide", Keep: 1}},
		{"missing directory", snapshot.Options{DatabaseURL: "postgres://db/pin_glide", Path: filepath.Join(directory, "missing"), Keep: 1}},
		{"zero retention", snapshot.Options{DatabaseURL: "postgres://db/pin_glide", Path: directory, Keep: 0}},
		{"negative retention", snapshot.Options{DatabaseURL: "postgres://db/pin_glide", Path: directory, Keep: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			name, err := snapshot.Snapshot(context.Background(), test.options)
			if err == nil || name != "" {
				t.Fatalf("snapshot = %q, error = %v; expected validation failure", name, err)
			}
		})
	}
}

func writeExecutable(t *testing.T, directory, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\nset -eu\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// Regression: URI-only PGDATABASE falls back to a local socket; command-line
// connection URLs must also keep passwords out of the process arguments.
func TestSnapshotPassesConnectionURLWithoutPassword(t *testing.T) {
	for _, tc := range []struct{ name, input, connection, password string }{
		{"userinfo", "postgres://pin_glide:pa%24%24@db:5433/helm?sslmode=require", "--dbname=postgres://pin_glide@db:5433/helm?sslmode=require", "pa$$"},
		{"query", "postgres://db/pin_glide?user=helm&password=query%26secret&sslmode=verify-full", "--dbname=postgres://db/pin_glide?sslmode=verify-full&user=helm", "query&secret"},
		{"no password", "postgres://db/pin_glide?sslmode=disable", "--dbname=postgres://db/pin_glide?sslmode=disable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := t.TempDir()
			writeExecutable(t, tools, "pg_dump", `
test "$4" = "$EXPECTED_CONNECTION" || exit 21
test "$PGPASSWORD" = "$EXPECTED_PASSWORD" || exit 22
test -z "${PGHOST:-}" && test -z "${PGDATABASE:-}" || exit 23
printf archive
`)
			writeExecutable(t, tools, "pg_restore", "exit 0")
			t.Setenv("PATH", tools)
			t.Setenv("PGHOST", "inherited.invalid")
			t.Setenv("PGDATABASE", "other-database")
			t.Setenv("PGPASSWORD", "wrong-password")
			t.Setenv("EXPECTED_CONNECTION", tc.connection)
			t.Setenv("EXPECTED_PASSWORD", tc.password)
			if _, err := snapshot.Snapshot(context.Background(), snapshot.Options{
				DatabaseURL: tc.input, Path: t.TempDir(), Keep: 1,
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
