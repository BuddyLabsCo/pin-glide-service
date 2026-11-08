// This file checks the published HTTP description against the operation
// inventory a released v1 client generates from.
package http_test

import (
	"slices"
	"strings"
	"testing"
)

// v1Inventory is every operation of the adopted v1 contract, as its method and
// published path. Clients generate from one released description and call it
// against a later service, so the description has to carry each of these and
// nothing the specification never adopted.
var v1Inventory = []string{
	"GET /health",
	"GET /v1/messages/{messageId}",
	"GET /v1/self/account",
	"GET /v1/self/receivers",
	"GET /v1/self/senders",
	"PATCH /v1/self/pairings/{id}",
	"DELETE /v1/self/pairings/{id}",
	"DELETE /v1/pairing-sessions/{id}",
	"POST /v1/auth/logout",
	"POST /v1/auth/qq",
	"POST /v1/auth/refresh",
	"POST /v1/auth/wechat",
	"POST /v1/devices/{id}/revoke",
	"POST /v1/messages",
	"POST /v1/model/credentials",
	"POST /v1/pairing-sessions",
	"POST /v1/pairing-sessions/{id}/authorize",
	"POST /v1/pairing-sessions/{id}/complete",
	"POST /v1/pairing-sessions/{id}/receiver-token",
}

// The published description must carry exactly the adopted v1 inventory: an
// operation a released client calls cannot disappear from the contract, and a
// path the specification does not define cannot appear in it.
func TestPublishedHTTPDescriptionCarriesTheAdoptedV1Inventory(t *testing.T) {
	description := published(t)
	paths, _ := description["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatalf("published description has no operations: %v", description)
	}

	served := []string{}
	for path, value := range paths {
		operations, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("path %s is not an operation item: %v", path, value)
		}
		for method := range operations {
			served = append(served, strings.ToUpper(method)+" "+path)
		}
	}

	want := slices.Clone(v1Inventory)
	slices.Sort(served)
	slices.Sort(want)
	if !slices.Equal(served, want) {
		t.Errorf("the published description carries %v, want the adopted v1 inventory %v", served, want)
	}
}
