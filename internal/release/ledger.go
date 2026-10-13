package release

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func relationExists(ctx context.Context, conn *sql.Conn, name string) (bool, error) {
	var exists bool
	err := conn.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists)
	return exists, err
}

// This is the stable pre-sync read contract. Do not read columns whose layout
// the current release may be about to change (SQL bodies, timing, attribution).
func readLedger(ctx context.Context, conn *sql.Conn) (map[string]string, error) {
	applied := make(map[string]string)
	for _, ledger := range []struct{ table, query string }{
		{"release.release_migration_scripts", "SELECT target_schema, filename, checksum FROM release.release_migration_scripts"},
		{"release.release_data_scripts", "SELECT 'scripts', filename, checksum FROM release.release_data_scripts"},
	} {
		exists, err := relationExists(ctx, conn, ledger.table)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		if err := readLedgerRows(ctx, conn, ledger.query, applied); err != nil {
			return nil, err
		}
	}
	return applied, nil
}

func readLedgerRows(ctx context.Context, conn *sql.Conn, query string, applied map[string]string) error {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var schema, name, checksum string
		if err := rows.Scan(&schema, &name, &checksum); err != nil {
			return err
		}
		key := schema + "/" + name
		if previous, ok := applied[key]; ok && previous != checksum {
			return fmt.Errorf("conflicting script history for %s", key)
		}
		applied[key] = checksum
	}
	return rows.Err()
}

func validateLedger(scripts []script, applied map[string]string) error {
	available := make(map[string]string, len(scripts))
	for _, s := range scripts {
		available[s.schema+"/"+s.name] = s.checksum
	}
	for key, checksum := range applied {
		if available[key] != checksum {
			return fmt.Errorf("previously processed script %s changed or is missing", key)
		}
	}
	return nil
}

func recordScript(ctx context.Context, tx *sql.Tx, s script) error {
	if s.schema == "scripts" {
		_, err := tx.ExecContext(ctx, `INSERT INTO release.release_data_scripts
   (filename,checksum,sql_body,duration_ms,executed_by) VALUES ($1,$2,$3,$4,$5)`, s.name, s.checksum, s.body, s.duration, s.executor)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO release.release_migration_scripts
 (target_schema,filename,checksum,sql_body,duration_ms,executed_by) VALUES ($1,$2,$3,$4,$5,$6)`, s.schema, s.name, s.checksum, s.body, s.duration, s.executor)
	return err
}

func applyScript(ctx context.Context, conn *sql.Conn, s script, initial bool) (script, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return s, err
	}
	defer func() { _ = tx.Rollback() }()
	if initial && s.schema != "scripts" {
		s.executor = "skipped"
	} else {
		started := time.Now()
		if _, err = tx.ExecContext(ctx, s.body); err != nil {
			return s, err
		}
		s.duration = time.Since(started).Milliseconds()
	}
	if s.schema != "release" {
		if err := recordScript(ctx, tx, s); err != nil {
			return s, err
		}
	}
	return s, tx.Commit()
}

func runScripts(ctx context.Context, conn *sql.Conn, scripts []script, applied map[string]string, schema string, initial bool) ([]script, error) {
	var processed []script
	for _, s := range scripts {
		if s.schema != schema {
			continue
		}
		key := s.schema + "/" + s.name
		if _, ok := applied[key]; ok {
			fmt.Printf("pin-glide-release migrate: skip %s (already processed)\n", key)
			continue
		}
		fmt.Printf("pin-glide-release migrate: process %s\n", key)
		done, err := applyScript(ctx, conn, s, initial)
		if err != nil {
			return nil, fmt.Errorf("script %s: %w", key, err)
		}
		processed = append(processed, done)
	}
	return processed, nil
}

func recordReleaseScripts(ctx context.Context, conn *sql.Conn, scripts []script) error {
	if len(scripts) == 0 {
		return nil
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range scripts {
		if err := recordScript(ctx, tx, s); err != nil {
			return err
		}
	}
	return tx.Commit()
}
