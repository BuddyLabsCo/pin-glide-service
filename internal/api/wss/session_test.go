package wss

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// A session state that no longer authorizes its channel ends it with the policy
// meaning, while a state change that leaves the channel serving does not.
func TestSessionStateEndsTheChannelWithTheMeaningItCarries(t *testing.T) {
	tests := []struct {
		name string
		view pairing.View
		want websocket.StatusCode
	}{
		{
			name: "a state change that keeps the channel",
			view: pairing.View{SessionID: "session-a", State: pairing.StateAwaitingConfirmation},
			want: websocket.StatusNormalClosure,
		},
		{
			name: "a canceled session",
			view: pairing.View{SessionID: "session-a", State: pairing.StateCanceled},
			want: websocket.StatusPolicyViolation,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hub := NewHub()
			accepted := make(chan struct{})
			served := make(chan struct{})
			serve := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				defer close(served)
				socket, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
					Subprotocols: []string{protocol.Subprotocol},
				})
				if err != nil {
					return
				}
				socket.SetReadLimit(maxFrameBytes)
				connection := NewConnection(socket, test.view.SessionID, pairing.SideReceiver, nil)
				hub.Subscribe(connection)
				close(accepted)
				connection.Serve(request.Context(), func([]byte) error { return nil })
				hub.Unsubscribe(connection)
			}))
			defer serve.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(serve.URL, "http"), &websocket.DialOptions{
				Subprotocols: []string{protocol.Subprotocol},
			})
			if err != nil {
				t.Fatalf("dial hub connection: %v", err)
			}
			<-accepted

			// A state change first queues the snapshot every participant sees,
			// which is also what a change that keeps the channel sends.
			hub.SendState(test.view.SessionID, protocol.PairingState{Version: protocol.WSSVersion, Type: "pairing_state"})
			readCtx, cancelRead := context.WithTimeout(context.Background(), 10*time.Second)
			_, payload, err := client.Read(readCtx)
			cancelRead()
			if err != nil {
				t.Fatalf("read the state frame: %v", err)
			}

			// The socket the handler is serving ends with the test, so the
			// handler returns instead of outliving it.
			defer func() { <-served }()
			defer func() { _ = client.CloseNow() }()
			var state struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(payload, &state); err != nil {
				t.Fatalf("decode the state frame: %v: %s", err, payload)
			}
			if state.Type != "pairing_state" {
				t.Errorf("state frame type = %q, want pairing_state", state.Type)
			}
			if test.want == websocket.StatusNormalClosure {
				// Nothing ended the channel, so it serves until the client
				// closes it.
				return
			}

			NewPairingSocket(nil, hub, nil).SessionChanged(test.view.SessionID, test.view)

			// The state frame a change publishes arrives before the close it
			// carries, so the test reads until the connection ends.
			for {
				closing, cancelClosing := context.WithTimeout(context.Background(), 10*time.Second)
				_, _, err := client.Read(closing)
				cancelClosing()
				if err == nil {
					continue
				}
				if code := websocket.CloseStatus(err); code != test.want {
					t.Errorf("closed session ended with %v, want %v", err, test.want)
				}
				break
			}
		})
	}
}
