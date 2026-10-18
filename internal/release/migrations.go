package release

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Options identifies the target and the operator's persisted install mode.
// InitialRetry is permitted only after this attempt has verified a fresh database.
type Options struct {
	Version      string
	Initial      bool
	InitialRetry bool
}

// Migrate owns one database-wide lock across scripts and independent psqldef
// processes. Each script commits separately; release ledger writes are deferred
// until release Schema Sync succeeds, deliberately allowing safe script replay.
func Migrate(ctx context.Context, pool *sql.DB, databaseURL string, options Options) error {
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(options.Version) {
		return fmt.Errorf("PIN_GLIDE_RELEASE_VERSION must be X.Y.Z (without v)")
	}
	if options.InitialRetry && !options.Initial {
		return fmt.Errorf("initial retry requires initial install mode")
	}
	desired := make(map[string]string)
	for _, schema := range []string{"release", "pin_glide"} {
		source, err := sourceSchema(schema)
		if err != nil {
			return err
		}
		if err := validateSQL(source); err != nil {
			return err
		}
		desired[schema] = source
	}
	scripts, err := releaseScripts()
	if err != nil {
		return err
	}
	tool, err := newPSQLDef(databaseURL)
	if err != nil {
		return err
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(719417, 63)").Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("another Pin Glide migration is running")
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(719417, 63)") }()
	if err := checkInstallMode(ctx, conn, options); err != nil {
		return err
	}
	applied, err := readLedger(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateLedger(scripts, applied); err != nil {
		return err
	}

	fmt.Println("pin-glide-db-tools migrate: phase release migrations")
	processed, err := runScripts(ctx, conn, scripts, applied, "release", options.Initial)
	if err != nil {
		return err
	}
	if err := tool.sync(ctx, conn, "release", desired["release"]); err != nil {
		return err
	}
	if err := recordReleaseScripts(ctx, conn, processed); err != nil {
		return fmt.Errorf("record release scripts: %w", err)
	}

	fmt.Println("pin-glide-db-tools migrate: phase pin_glide migrations")
	if _, err := runScripts(ctx, conn, scripts, applied, "pin_glide", options.Initial); err != nil {
		return err
	}
	if err := tool.sync(ctx, conn, "pin_glide", desired["pin_glide"]); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO release.release_deploy_history (release_version) VALUES ($1)", options.Version); err != nil {
		return fmt.Errorf("record Database Release: %w", err)
	}
	fmt.Println("pin-glide-db-tools migrate: phase data scripts")
	if _, err := runScripts(ctx, conn, scripts, applied, "scripts", false); err != nil {
		return err
	}
	fmt.Println("pin-glide-db-tools migrate: complete")
	return nil
}

func checkInstallMode(ctx context.Context, conn *sql.Conn, options Options) error {
	exists, err := relationExists(ctx, conn, "release.release_deploy_history")
	if err != nil {
		return err
	}
	var version string
	if exists {
		err = conn.QueryRowContext(ctx, "SELECT release_version FROM release.release_deploy_history ORDER BY id DESC LIMIT 1").Scan(&version)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("read Database Release: %w", err)
		}
	}
	if options.Initial {
		if version != "" && (!options.InitialRetry || version != options.Version) {
			return fmt.Errorf("initial install refused: database already deployed at %s", version)
		}
		fmt.Println("pin-glide-db-tools migrate: initial database accepted")
	} else if version == "" {
		return fmt.Errorf("upgrade requires an existing Database Release; use explicit initial install only for a fresh database")
	}
	return nil
}

func sourceSchema(schema string) (string, error) {
	var source strings.Builder
	err := filepath.WalkDir(filepath.Join("db/schema", schema), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".sql" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source.Write(data)
		source.WriteByte('\n')
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("read %s schema: %w", schema, err)
	}
	if strings.TrimSpace(source.String()) == "" {
		return "", fmt.Errorf("empty %s desired schema", schema)
	}
	return source.String(), nil
}
