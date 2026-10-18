//go:build integration

package identity_test

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

// The real entrypoint starts with every supported identity-provider
// configuration state. A startup check added after the database opens would
// still fail here instead of being hidden behind an unreachable test database.
func TestAPIServiceStartsWithIdentityProviderMatrix(t *testing.T) {
	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")

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
		name         string
		wechatAppID  string
		wechatSecret string
		qqAppID      string
		qqAppKey     string
	}{
		{name: "neither configured"},
		{name: "only wechat", wechatAppID: "wx-app", wechatSecret: "wx-secret"},
		{name: "only qq", qqAppID: "qq-app", qqAppKey: "qq-key"},
		{name: "both configured", wechatAppID: "wx-app", wechatSecret: "wx-secret", qqAppID: "qq-app", qqAppKey: "qq-key"},
		{name: "wechat partial", wechatAppID: "wx-app"},
		{name: "qq partial", qqAppKey: "qq-key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address := freeAddress(t)
			t.Setenv("PIN_GLIDE_ENV", "production")
			t.Setenv("PIN_GLIDE_DATABASE_URL", postgres.DSN)
			t.Setenv("PIN_GLIDE_API_ADDRESS", address)
			t.Setenv("PIN_GLIDE_WECHAT_APP_ID", test.wechatAppID)
			t.Setenv("PIN_GLIDE_WECHAT_APP_SECRET", test.wechatSecret)
			t.Setenv("PIN_GLIDE_QQ_APP_ID", test.qqAppID)
			t.Setenv("PIN_GLIDE_QQ_APP_KEY", test.qqAppKey)
			t.Setenv("PIN_GLIDE_BAILIAN_API_KEY", "test-parent-key")

			command := exec.Command(binary)
			command.Dir = root
			var logs bytes.Buffer
			command.Stdout = &logs
			command.Stderr = &logs
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

			waitForHealth(t, address, exited, &logs)
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

func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func waitForHealth(t *testing.T, address string, exited <-chan error, logs *bytes.Buffer) {
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
