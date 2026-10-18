//go:build integration

package identity_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

// The real entrypoint starts with every supported identity-provider
// configuration state. A startup check added after the database opens would
// still fail here instead of being hidden behind an unreachable test database.
func TestAPIServiceStartsWithIdentityProviderMatrix(t *testing.T) {
	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")
	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(cwd, "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "pin-glide-api")
	build := exec.Command("go", "build", "-o", binary, "./cmd/pin-glide-api")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build api entrypoint: %v\n%s", err, output)
	}

	tests := []struct {
		name              string
		wechatAppID       string
		wechatSecret      string
		qqAppID           string
		qqAppKey          string
		unavailableProofs []string
	}{
		{name: "neither configured", unavailableProofs: []string{"wechat", "qq"}},
		{name: "only wechat", wechatAppID: "wx-app", wechatSecret: "wx-secret", unavailableProofs: []string{"qq"}},
		{name: "only qq", qqAppID: "qq-app", qqAppKey: "qq-key", unavailableProofs: []string{"wechat"}},
		{name: "both configured", wechatAppID: "wx-app", wechatSecret: "wx-secret", qqAppID: "qq-app", qqAppKey: "qq-key"},
		{name: "wechat partial", wechatAppID: "wx-app", unavailableProofs: []string{"wechat"}},
		{name: "qq partial", qqAppKey: "qq-key", unavailableProofs: []string{"qq"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PIN_GLIDE_ENV", "production")
			t.Setenv("PIN_GLIDE_DATABASE_URL", postgres.DSN)
			t.Setenv("PIN_GLIDE_API_ADDRESS", "127.0.0.1:0")
			t.Setenv("PIN_GLIDE_WECHAT_APP_ID", test.wechatAppID)
			t.Setenv("PIN_GLIDE_WECHAT_APP_SECRET", test.wechatSecret)
			t.Setenv("PIN_GLIDE_QQ_APP_ID", test.qqAppID)
			t.Setenv("PIN_GLIDE_QQ_APP_KEY", test.qqAppKey)
			t.Setenv("PIN_GLIDE_BAILIAN_API_KEY", "test-parent-key")

			command := exec.Command(binary)
			command.Dir = root
			logs := &lockedBuffer{}
			command.Stdout = logs
			command.Stderr = logs
			if err := command.Start(); err != nil {
				t.Fatalf("start api: %v", err)
			}
			t.Cleanup(func() {
				if command.Process != nil {
					_ = command.Process.Kill()
				}
			})
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()

			address := waitForListeningAddress(t, exited, logs)
			waitForHealth(t, address, exited, logs)
			for _, proof := range test.unavailableProofs {
				assertUnavailableProof(t, address, pool, proof)
			}
			if err := command.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("stop api: %v", err)
			}
			select {
			case err := <-exited:
				if err != nil {
					t.Fatalf("api exit: %v\n%s", err, logs.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("api did not stop after SIGTERM\n%s", logs.String())
			}
		})
	}
}

func waitForListeningAddress(t *testing.T, exited <-chan error, logs *lockedBuffer) string {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-exited:
			t.Fatalf("api exited before listening: %v\n%s", err, logs.String())
		default:
		}

		text := logs.String()
		const marker = `"address":"`
		if start := strings.Index(text, marker); start >= 0 {
			start += len(marker)
			if end := strings.IndexByte(text[start:], '"'); end >= 0 {
				return text[start : start+end]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("api did not report a listening address\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertUnavailableProof(t *testing.T, address string, pool *sql.DB, provider string) {
	t.Helper()

	var path, body string
	switch provider {
	case "wechat":
		path, body = "/v1/auth/wechat", `{"wechatCode":"unconfigured-provider"}`
	case "qq":
		path, body = "/v1/auth/qq", `{"qqCode":"unconfigured-provider"}`
	default:
		t.Fatalf("unknown unavailable provider %q", provider)
	}

	response, err := http.Post("http://"+address+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s login through unconfigured provider: %v", provider, err)
	}
	payload, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read %s login response: %v", provider, err)
	}
	if response.StatusCode != http.StatusBadGateway ||
		!strings.Contains(string(payload), "identity_provider_error") {
		t.Fatalf("%s login through unconfigured provider = %d %s, want 502 identity_provider_error",
			provider, response.StatusCode, payload)
	}

	var accounts, senders, credentials int
	if err := pool.QueryRow(`SELECT
		(SELECT count(*) FROM pin_glide.identity_accounts),
		(SELECT count(*) FROM pin_glide.identity_sender_registrations),
		(SELECT count(*) FROM pin_glide.identity_credentials)`).Scan(
		&accounts, &senders, &credentials); err != nil {
		t.Fatalf("count identity state: %v", err)
	}
	if accounts != 0 || senders != 0 || credentials != 0 {
		t.Fatalf("failed QQ login wrote identity state: accounts=%d senders=%d credentials=%d",
			accounts, senders, credentials)
	}
}

func waitForHealth(t *testing.T, address string, exited <-chan error, logs *lockedBuffer) {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-exited:
			t.Fatalf("api exited before health: %v\n%s", err, logs.String())
		default:
		}

		response, err := client.Get("http://" + address + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("api did not become healthy at %s: %v\n%s", address, err, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
