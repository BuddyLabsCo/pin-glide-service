// Package snapshot creates and retains PostgreSQL custom-format archives.
package snapshot

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Options struct {
	DatabaseURL string
	Path        string
	Keep        int
}

const timestampLayout = "20060102T150405.000000000Z"

var completedName = regexp.MustCompile(`^pin-glide-snapshot-(\d{8}T\d{6}\.\d{9}Z)-[0-9]+\.dump$`)

// Snapshot publishes a validated archive and returns its path.
func Snapshot(ctx context.Context, options Options) (string, error) {
	if strings.TrimSpace(options.DatabaseURL) == "" {
		return "", fmt.Errorf("PIN_GLIDE_DATABASE_URL is required")
	}
	connection, err := url.Parse(options.DatabaseURL)
	if err != nil || (connection.Scheme != "postgres" && connection.Scheme != "postgresql") {
		return "", fmt.Errorf("PIN_GLIDE_DATABASE_URL must be a PostgreSQL URL")
	}
	query, err := url.ParseQuery(connection.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid PIN_GLIDE_DATABASE_URL options")
	}
	password, _ := connection.User.Password()
	if connection.User != nil {
		connection.User = url.User(connection.User.Username())
	}
	if query.Has("password") {
		password = query.Get("password")
		query.Del("password")
	}
	connection.RawQuery = query.Encode()
	if strings.TrimSpace(options.Path) == "" {
		return "", fmt.Errorf("PIN_GLIDE_SNAPSHOT_PATH is required")
	}
	if options.Keep <= 0 {
		return "", fmt.Errorf("PIN_GLIDE_SNAPSHOT_KEEP must be a positive integer")
	}
	directory, err := os.Stat(options.Path)
	if err != nil {
		return "", fmt.Errorf("open snapshot directory: %w", err)
	}
	if !directory.IsDir() {
		return "", fmt.Errorf("PIN_GLIDE_SNAPSHOT_PATH must be an existing directory")
	}
	file, err := os.CreateTemp(options.Path, ".pin-glide-snapshot-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create snapshot temporary file: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()

	// libpq expands a URI passed as dbname, but not one inherited in PGDATABASE.
	dump := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-security-labels", "--exclude-extension=anon", "--dbname="+connection.String())
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "PG") {
			dump.Env = append(dump.Env, variable)
		}
	}
	dump.Env = append(dump.Env, "PGPASSWORD="+password)
	dump.Stdout = file
	if err := dump.Run(); err != nil {
		return "", fmt.Errorf("pg_dump failed")
	}
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect snapshot: %w", err)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("pg_dump produced an empty archive")
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync snapshot: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close snapshot: %w", err)
	}
	validate := exec.CommandContext(ctx, "pg_restore", "--list", file.Name())
	if err := validate.Run(); err != nil {
		return "", fmt.Errorf("snapshot archive validation failed")
	}

	suffix := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file.Name()), ".pin-glide-snapshot-"), ".tmp")
	name := "pin-glide-snapshot-" + time.Now().UTC().Format(timestampLayout) + "-" + suffix + ".dump"
	path := filepath.Join(options.Path, name)
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("snapshot filename already exists")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("check snapshot filename: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return "", fmt.Errorf("publish snapshot: %w", err)
	}
	if err := retain(options.Path, options.Keep); err != nil {
		return path, err
	}
	return path, nil
}

func retain(directory string, keep int) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("list snapshots for retention: %w", err)
	}
	var completed []string
	for _, entry := range entries {
		parts := completedName.FindStringSubmatch(entry.Name())
		if parts == nil {
			continue
		}
		if _, err := time.Parse(timestampLayout, parts[1]); err != nil {
			continue
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect snapshot for retention: %w", err)
		}
		if info.Mode().IsRegular() && info.Size() > 0 {
			completed = append(completed, entry.Name())
		}
	}
	slices.Sort(completed)
	for _, name := range completed[:max(0, len(completed)-keep)] {
		if err := os.Remove(filepath.Join(directory, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove expired snapshot: %w", err)
		}
	}
	return nil
}
