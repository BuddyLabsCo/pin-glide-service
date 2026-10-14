// Package harness composes the real service for integration tests: a disposable
// PostgreSQL instance, the actual service over a loopback HTTP listener, and
// refusal of non-disposable databases. Only external provider boundaries are
// faked in tests that use it.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api"
	"github.com/BuddyLabsCo/pin-glide-service/internal/api/wss"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
)

const (
	postgresUser   = "pin_glide"
	postgresDBName = "pin_glide"
)

var (
	postgresImageOnce sync.Once
	postgresImageRef  string
	postgresImageErr  error
)

// postgresImage returns the derived server distribution from the build script
// every database consumer shares, so integration tests run the same
// PostgreSQL/pg_cron image as Compose, lint/codegen and local development.
func postgresImage(t *testing.T) string {
	t.Helper()

	postgresImageOnce.Do(func() {
		root, ok := repositoryRoot()
		if !ok {
			postgresImageErr = errors.New("cannot locate the repository root")
			return
		}
		command := exec.Command("bash", filepath.Join(root, "scripts", "db", "postgres-image.sh"))
		command.Stderr = os.Stderr
		output, err := command.Output()
		if err != nil {
			postgresImageErr = fmt.Errorf("build the derived PostgreSQL image: %w", err)
			return
		}
		postgresImageRef = strings.TrimSpace(string(output))
	})
	if postgresImageErr != nil {
		t.Fatal(postgresImageErr)
	}
	return postgresImageRef
}

// repositoryRoot returns the checkout root this harness file lives in.
func repositoryRoot() (string, bool) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..")), true
}

// Postgres is a disposable PostgreSQL instance owned by one test.
type Postgres struct {
	DSN string

	container string
	stopped   bool
}

// StartPostgres starts a disposable PostgreSQL container on a random loopback
// port with synthetic credentials and waits until it accepts connections.
func StartPostgres(t *testing.T) *Postgres {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker is required to start a disposable PostgreSQL instance: %v", err)
	}

	password := rand.Text()
	container := dockerOutput(t, "run", "--rm", "--detach",
		"--publish", "127.0.0.1::5432",
		"--env", "POSTGRES_USER="+postgresUser,
		"--env", "POSTGRES_PASSWORD="+password,
		"--env", "POSTGRES_DB="+postgresDBName,
		postgresImage(t),
	)
	postgres := &Postgres{container: container}
	t.Cleanup(func() {
		if err := postgres.Stop(); err != nil {
			t.Errorf("stop disposable PostgreSQL: %v", err)
		}
	})

	postgres.DSN = fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable",
		postgresUser, password, dockerPort(t, container), postgresDBName)
	waitForPostgres(t, postgres.DSN)
	return postgres
}

// Stop removes the disposable container. It is safe to call more than once, so
// a test can stop the database deliberately and still let cleanup run.
func (p *Postgres) Stop() error {
	if p.stopped {
		return nil
	}
	p.stopped = true

	command := exec.Command("docker", "rm", "--force", p.container)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("remove container %s: %w: %s", p.container, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// ApplySchema loads one desired schema from db/schema/<target> with the pinned
// psqldef the release process uses, so composed tests run against the shipped
// structure instead of test-only objects.
func (p *Postgres) ApplySchema(t *testing.T, target string) {
	t.Helper()

	root, ok := repositoryRoot()
	if !ok {
		t.Fatal("cannot locate the repository root")
	}

	var files []string
	err := filepath.WalkDir(filepath.Join(root, "db", "schema", target),
		func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".sql") {
				files = append(files, path)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("locate the %s desired schema: %v", target, err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("the %s desired schema is empty", target)
	}

	var desired strings.Builder
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read schema file %s: %v", file, err)
		}
		desired.Write(body)
		desired.WriteString("\n")
	}

	// psqldef orders the desired statements, which is what the release image
	// relies on: filename order would create a foreign key before the table it
	// references.
	config := filepath.Join(t.TempDir(), "psqldef.yml")
	if err := os.WriteFile(config, []byte("target_schema: "+target+"\n"), 0o600); err != nil {
		t.Fatalf("write psqldef configuration: %v", err)
	}
	database, err := url.Parse(p.DSN)
	if err != nil {
		t.Fatalf("parse the disposable database URL: %v", err)
	}
	password, _ := database.User.Password()

	command := exec.Command("go", "tool", "psqldef",
		"-h", database.Hostname(), "-p", database.Port(), "-U", database.User.Username(),
		strings.TrimPrefix(database.Path, "/"), "--config", config, "--apply")
	command.Dir = root
	command.Env = append(os.Environ(), "PGPASSWORD="+password)
	command.Stdin = strings.NewReader(desired.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("apply the %s desired schema: %v: %s", target, err, output)
	}
}

// Service is the composed service running on a real loopback HTTP listener.
type Service struct {
	BaseURL string
	Client  *http.Client
	// Presence is the liveness of Receiver delivery connections this process
	// run publishes, which is what a Sender's Receiver list reports.
	Presence *wss.Presence

	cancel  context.CancelFunc
	served  <-chan error
	pool    *sql.DB
	stopErr error
	stopped bool
}

// StartService composes the service against databaseURL, starts it on a real
// listener and returns once health answers. Tests cannot select a shared or
// production database or origin: the harness only starts disposable loopback
// databases and binds one loopback port. provider is the faked external WeChat
// boundary of that service.
func StartService(t *testing.T, databaseURL string, provider identity.Provider) *Service {
	t.Helper()
	return StartServiceLogging(t, databaseURL, provider, observability.NewLogger(os.Stderr, "integration"))
}

// StartServiceLogging composes the service like StartService with the supplied
// logger, so a test can assert what reaches service diagnostics.
func StartServiceLogging(t *testing.T, databaseURL string, provider identity.Provider, logger *slog.Logger) *Service {
	t.Helper()
	requireDisposableDatabase(t, databaseURL)

	ctx, cancel := context.WithCancel(context.Background())
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		cancel()
		t.Fatalf("open database: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		_ = pool.Close()
		t.Fatalf("listen: %v", err)
	}

	served := make(chan error, 1)
	presence := wss.NewPresence(time.Now().UTC())
	go func() {
		served <- api.Serve(ctx, listener, api.Dependencies{
			Database: pool,
			Logger:   logger,
			WeChat:   provider,
			Presence: presence,
		})
	}()

	service := &Service{
		BaseURL:  "http://" + listener.Addr().String(),
		Client:   &http.Client{Timeout: 10 * time.Second},
		Presence: presence,
		cancel:   cancel,
		served:   served,
		pool:     pool,
	}
	t.Cleanup(func() {
		if err := service.Stop(); err != nil {
			t.Errorf("stop composed service: %v", err)
		}
	})
	service.waitReady(t)
	return service
}

// Stop joins the shutdown path of the composed service and closes its pool. It
// is safe to call more than once.
func (s *Service) Stop() error {
	if s.stopped {
		return s.stopErr
	}
	s.stopped = true
	s.cancel()

	select {
	case s.stopErr = <-s.served:
	case <-time.After(10 * time.Second):
		s.stopErr = errors.New("composed service did not stop within its shutdown window")
	}
	s.stopErr = errors.Join(s.stopErr, s.pool.Close())
	return s.stopErr
}

// Get issues a real HTTP GET against the composed service.
func (s *Service) Get(t *testing.T, path string) *http.Response {
	t.Helper()

	response, err := s.Client.Get(s.BaseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return response
}

// waitReady blocks until health answers successfully, so a test only continues
// against a started service.
func (s *Service) waitReady(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := s.Client.Get(s.BaseURL + "/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("composed service did not become healthy")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requireDisposableDatabase refuses any database that is not on the loopback
// interface, so tests cannot point at shared or production storage.
func requireDisposableDatabase(t *testing.T, databaseURL string) {
	t.Helper()

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return
	}
	t.Fatalf("database host %q is not a disposable loopback target", parsed.Hostname())
}

func waitForPostgres(t *testing.T, databaseURL string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pool, err := db.Open(ctx, databaseURL)
		cancel()
		if err == nil {
			_ = pool.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("disposable PostgreSQL did not become ready: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func dockerPort(t *testing.T, container string) string {
	t.Helper()

	_, port, found := strings.Cut(dockerOutput(t, "port", container, "5432/tcp"), ":")
	if !found || port == "" {
		t.Fatalf("container %s has no published PostgreSQL port", container)
	}
	return port
}

func dockerOutput(t *testing.T, args ...string) string {
	t.Helper()

	command := exec.Command("docker", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(output))
}
