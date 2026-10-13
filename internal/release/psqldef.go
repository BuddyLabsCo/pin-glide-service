package release

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type psqlDef struct {
	command string
	args    []string
	env     []string
}

func newPSQLDef(databaseURL string) (psqlDef, error) {
	var tool psqlDef
	uri, err := url.Parse(databaseURL)
	if err != nil || (uri.Scheme != "postgres" && uri.Scheme != "postgresql") {
		return tool, fmt.Errorf("psqldef requires a postgres:// or postgresql:// PIN_GLIDE_DATABASE_URL")
	}
	// Do not silently send psqldef to a different database or weaken TLS by losing
	// connection options. The pinned CLI only exposes these libpq parameters.
	supported := map[string]string{
		"sslmode": "PGSSLMODE", "sslrootcert": "PGSSLROOTCERT",
		"sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY", "connect_timeout": "PGCONNECT_TIMEOUT",
	}
	for key := range uri.Query() {
		if _, ok := supported[key]; !ok && key != "host" && key != "port" && key != "user" && key != "password" && key != "dbname" {
			return tool, fmt.Errorf("psqldef does not support PIN_GLIDE_DATABASE_URL option %q", key)
		}
	}
	connection, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		// pgx's parse error can include the password-bearing input URL.
		return tool, fmt.Errorf("invalid PIN_GLIDE_DATABASE_URL for psqldef")
	}
	if strings.Contains(uri.Host, ",") || strings.Contains(uri.Query().Get("host"), ",") {
		return tool, fmt.Errorf("psqldef requires a single PostgreSQL host")
	}
	if strings.HasSuffix(connection.Database, ".sql") || strings.HasPrefix(connection.Database, "-") {
		return tool, fmt.Errorf("database name is ambiguous to psqldef")
	}
	tool.command, err = exec.LookPath("psqldef")
	if err != nil {
		// Local development already installs psqldef as a pinned Go tool.
		tool.command, err = exec.LookPath("go")
		if err != nil {
			return tool, fmt.Errorf("psqldef is required (bundled in the migrate image): %w", err)
		}
		tool.args = []string{"tool", "psqldef"}
	}
	tool.args = append(tool.args, "-h", connection.Host, "-p", strconv.Itoa(int(connection.Port)), "-U", connection.User,
		connection.Database)
	// PIN_GLIDE_DATABASE_URL is authoritative; inherited libpq settings must not
	// alter the target or the TLS policy. Passwords never appear in command arguments.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PG") {
			tool.env = append(tool.env, value)
		}
	}
	tool.env = append(tool.env, "PGPASSWORD="+connection.Password, "PGSSLMODE=prefer")
	for key, env := range supported {
		if value := uri.Query().Get(key); value != "" {
			tool.env = append(tool.env, env+"="+value)
		}
	}
	return tool, nil
}

// schemaAudit describes a single real child invocation, never an inferred check.
type schemaAudit struct {
	Schema    string `json:"target_schema"`
	Operation string `json:"operation"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  *int   `json:"exit_code"`
	Duration  int64  `json:"duration_ms"`
	Executor  string `json:"executed_by"`
	Error     string `json:"error,omitempty"`
}

func (tool psqlDef) run(ctx context.Context, schema, mode, desired string) (schemaAudit, error) {
	fmt.Printf("pin-glide-release migrate: phase psqldef %s --%s\n", schema, mode)
	args := append(append([]string(nil), tool.args...), "--config-inline", "target_schema: "+schema, "--"+mode)
	command := exec.CommandContext(ctx, tool.command, args...)
	command.Env = tool.env
	command.Stdin = strings.NewReader(desired)
	var stdout, stderr bytes.Buffer
	command.Stdout = io.MultiWriter(os.Stdout, &stdout)
	command.Stderr = io.MultiWriter(os.Stderr, &stderr)
	started := time.Now()
	err := command.Run()
	audit := schemaAudit{Schema: schema, Operation: mode, Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(started).Milliseconds(), Executor: "system"}
	if command.ProcessState != nil && command.ProcessState.ExitCode() >= 0 {
		code := command.ProcessState.ExitCode()
		audit.ExitCode = &code
	}
	if err != nil {
		audit.Error = err.Error()
	}
	return audit, err
}

func (tool psqlDef) sync(ctx context.Context, conn *sql.Conn, schema, desired string) error {
	for _, mode := range []string{"apply", "check"} {
		audit, runErr := tool.run(ctx, schema, mode, desired)
		// Cancellation still deserves an audit if the database remains reachable.
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, auditErr := conn.ExecContext(auditCtx, `INSERT INTO release.release_schema_audits
   (target_schema,operation,stdout,stderr,exit_code,duration_ms,executed_by)
   VALUES ($1,$2,$3,$4,$5,$6,$7)`, audit.Schema, audit.Operation, audit.Stdout, audit.Stderr, audit.ExitCode, audit.Duration, audit.Executor)
		cancel()
		if auditErr != nil {
			// JSON preserves both complete streams and unavailable exit status explicitly.
			_ = json.NewEncoder(os.Stderr).Encode(audit)
			return fmt.Errorf("persist schema audit: %w", errors.Join(auditErr, runErr))
		}
		if runErr != nil {
			if mode == "check" {
				return fmt.Errorf("%s schema did not converge: %w", schema, runErr)
			}
			return fmt.Errorf("psqldef %s --%s: %w", schema, mode, runErr)
		}
	}
	return nil
}
