package wss

import (
	"net/http"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

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
