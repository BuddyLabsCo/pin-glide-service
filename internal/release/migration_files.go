package release

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type script struct {
	schema, name, checksum, body string
	duration                     int64
	executor                     string
}

// The manifest carries repository-relative identities in introduction-commit
// order. Local Git uses the same order, then appends uncommitted filenames.
func migrationFiles() ([]string, error) {
	available := make(map[string]bool)
	for _, dir := range []string{"db/migrations/release", "db/migrations/pin_glide", "db/scripts"} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
				available[filepath.Join(dir, entry.Name())] = true
			}
		}
	}
	order, err := os.ReadFile("db/migrations/order.txt")
	if errors.Is(err, os.ErrNotExist) && len(available) > 0 {
		// Share one ordering implementation with packaging, including
		// shallow-history rejection. Published images always carry the manifest.
		order, err = exec.Command("bash", "scripts/release/migration-order.sh").Output()
	}
	if err != nil && (!errors.Is(err, os.ErrNotExist) || len(available) > 0) {
		return nil, fmt.Errorf("script order requires db/migrations/order.txt or Git history: %w", err)
	}
	var files []string
	for _, line := range strings.Split(string(order), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if !available[name] {
			return nil, fmt.Errorf("script order contains missing or duplicate file %q", name)
		}
		files = append(files, name)
		delete(available, name)
	}
	for name := range available {
		return nil, fmt.Errorf("script %s is absent from order.txt", name)
	}
	return files, nil
}

func releaseScripts() ([]script, error) {
	files, err := migrationFiles()
	if err != nil {
		return nil, err
	}
	var scripts []script
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := validateSQL(string(data)); err != nil {
			return nil, fmt.Errorf("script %s: %w", path, err)
		}
		schema := filepath.Base(filepath.Dir(path))
		scripts = append(scripts, script{schema: schema, name: filepath.Base(path), checksum: fmt.Sprintf("%x", sha256.Sum256(data)), body: string(data), executor: "system"})
	}
	return scripts, nil
}
