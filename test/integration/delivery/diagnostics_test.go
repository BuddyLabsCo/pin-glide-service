//go:build integration

package delivery_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// connectionStatuses reads the retained WSS statuses of one Receiver
// connection, in the order the service stored them.
func (e *environment) connectionStatuses(t *testing.T, receiverID string) []int {
	t.Helper()

	rows, err := e.pool.QueryContext(context.Background(), `
SELECT code FROM pin_glide.service_diagnostics
WHERE kind = 'wss' AND subject = $1
ORDER BY id`, receiverID)
	if err != nil {
		t.Fatalf("read retained connection records: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var statuses []int
	for rows.Next() {
		var code sql.NullInt64
		if err := rows.Scan(&code); err != nil {
			t.Fatalf("read one retained connection status: %v", err)
		}
		if !code.Valid {
			t.Fatalf("a retained connection record carries no status")
		}
		statuses = append(statuses, int(code.Int64))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read retained connection records: %v", err)
	}
	return statuses
}

// expectConnectionStatuses requires the retained connection records of one
// Receiver to carry exactly the given statuses, in order.
func (e *environment) expectConnectionStatuses(t *testing.T, receiverID string, want ...int) {
	t.Helper()

	if statuses := e.connectionStatuses(t, receiverID); !slices.Equal(statuses, want) {
		t.Errorf("the connection records of %s retained %v, want %v", receiverID, statuses, want)
	}
}
