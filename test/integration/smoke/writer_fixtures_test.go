//go:build integration

package smoke_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

func TestApplicationTerminationTargetsOnlyTheRunOwnedWriter(t *testing.T) {
	postgres := harness.StartPostgres(t)
	sql := generateSmokeSQL(t)
	primary, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open primary connection: %v", err)
	}
	t.Cleanup(func() { _ = primary.Close() })

	owned, ownedConnection := heldConnection(t, postgres.DSN, "smoke-integration-owned-writer")
	t.Cleanup(func() { _ = owned.Close() })
	unrelated, unrelatedConnection := heldConnection(t, postgres.DSN, "smoke-integration-unrelated-writer")
	t.Cleanup(func() { _ = unrelated.Close() })

	execSQL(t, primary, sql.TerminateWriterSQL)
	waitForApplicationConnections(t, primary, "smoke-integration-owned-writer", 0)
	assertApplicationConnections(t, primary, "smoke-integration-unrelated-writer", 1)
	_ = ownedConnection.Close()
	_ = unrelatedConnection.Close()
}

func waitForApplicationConnections(t *testing.T, pool *sql.DB, application string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := applicationConnections(t, pool, application); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s connections did not reach %d", application, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertApplicationConnections(t *testing.T, pool *sql.DB, application string, want int) {
	t.Helper()
	if got := applicationConnections(t, pool, application); got != want {
		t.Fatalf("%s connections is %d, want %d", application, got, want)
	}
}

func applicationConnections(t *testing.T, pool *sql.DB, application string) int {
	t.Helper()
	var count int
	if err := pool.QueryRowContext(context.Background(), `
SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, application).
		Scan(&count); err != nil {
		t.Fatalf("query %s connections: %v", application, err)
	}
	return count
}
