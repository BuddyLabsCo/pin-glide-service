package wss

import (
	"net/http"
	"testing"
	"unicode"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// Every close the service can send is one of the documented meanings, with the
// documented code and a reason that is a short machine code rather than
// anything that could carry a credential, ciphertext or another secret value.
func TestCloseMeaningsMatchThePublishedDefinitions(t *testing.T) {
	documented := map[int]protocol.CloseCode{}
	for _, meaning := range protocol.CloseCodes {
		documented[meaning.Code] = meaning
	}

	for _, means := range closeCodes {
		definition, ok := documented[int(means.code)]
		if !ok {
			t.Errorf("intent %d closes with %d, which protocol.CloseCodes does not document", means.intent, means.code)
			continue
		}
		code, reason := closeMeaning(means.intent)
		if code != websocket.StatusCode(definition.Code) || reason != definition.Reason {
			t.Errorf("closeMeaning(%d) = (%d, %q), want the documented (%d, %q)",
				means.intent, code, reason, definition.Code, definition.Reason)
		}
		if len(reason) > 123 {
			t.Errorf("intent %d reason is %d bytes, over the 123-byte limit", means.intent, len(reason))
		}
		for _, character := range reason {
			if unicode.IsLetter(character) && !unicode.IsLower(character) {
				t.Errorf("intent %d reason %q is not a machine-readable code", means.intent, reason)
				break
			}
		}
	}
}

// An upgrade only reaches this surface when it offered the subprotocol the
// service publishes; a client that offered nothing else must be refused rather
// than served without it.
func TestUpgradeRequiresThePublishedSubprotocol(t *testing.T) {
	tests := []struct {
		name    string
		offered []string
		want    bool
	}{
		{name: "absent", offered: nil, want: false},
		{name: "another protocol", offered: []string{"pin-glide.v2"}, want: false},
		{name: "exact", offered: []string{protocol.Subprotocol}, want: true},
		{name: "case-insensitive", offered: []string{"PIN-GLIDE.V1"}, want: true},
		{name: "listed with others", offered: []string{"chat.v1", " pin-glide.v1 "}, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, "/v1/receiver/events", nil)
			if err != nil {
				t.Fatalf("build upgrade request: %v", err)
			}
			for _, offered := range test.offered {
				request.Header.Add("Sec-WebSocket-Protocol", offered)
			}
			if got := subprotocolAccepted(request); got != test.want {
				t.Errorf("subprotocolAccepted(%v) = %v, want %v", test.offered, got, test.want)
			}
		})
	}
}
