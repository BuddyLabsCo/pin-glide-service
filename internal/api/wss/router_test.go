package wss

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// pairingChannelStub records what the router handed to the pairing channel and
// reports the failure of a subscription the service refused before upgrading.
type pairingChannelStub struct {
	served    int
	sessionID string
	release   func()
	refusal   error
}

func (s *pairingChannelStub) Serve(_ http.ResponseWriter, _ *http.Request, sessionID string, release func()) error {
	s.served++
	s.sessionID = sessionID
	s.release = release
	return s.refusal
}

// receiverChannelStub is the delivery channel as the router hands it work.
type receiverChannelStub struct {
	served  int
	release func()
	refusal error
}

func (s *receiverChannelStub) Serve(_ http.ResponseWriter, _ *http.Request, release func()) error {
	s.served++
	s.release = release
	return s.refusal
}

// The WSS surface owns its own routes: each channel answers the path the
// published description names, the pairing channel receives the session
// identifier of that path, the connection's admission release reaches the
// channel that holds it, and a refused subscription is answered and logged
// through the shared problem document this surface was composed with.
func TestRouterRoutesChannelsAndReportsRefusedSubscriptions(t *testing.T) {
	pairingChannel := &pairingChannelStub{refusal: ErrSubprotocolRequired}
	receiverChannel := &receiverChannelStub{refusal: ErrReceiverUnavailable}

	released := 0
	rejected := map[string]error{}
	var logged bytes.Buffer
	router := Router{
		Pairing:  pairingChannel,
		Receiver: receiverChannel,
		Release:  func(context.Context) func() { return func() { released++ } },
		Reject: func(writer http.ResponseWriter, request *http.Request, reason error) protocol.ErrorCode {
			rejected[request.URL.Path] = reason
			writer.WriteHeader(http.StatusBadRequest)
			return protocol.InvalidRequest
		},
		Logger: slog.New(slog.NewTextHandler(&logged, nil)),
	}

	mux := http.NewServeMux()
	Register(mux, router)

	paths := []string{"/v1/pairing-sessions/session-a/events", "/v1/receiver/events"}
	for _, path := range paths {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want %d", path, response.Code, http.StatusBadRequest)
		}
	}

	if pairingChannel.served != 1 || receiverChannel.served != 1 {
		t.Errorf("served = %d pairing, %d receiver, want one each", pairingChannel.served, receiverChannel.served)
	}
	if pairingChannel.sessionID != "session-a" {
		t.Errorf("pairing session = %q, want session-a", pairingChannel.sessionID)
	}
	// The real sockets release the admission they were given when their channel
	// ends, so the release each stub received is the one the router resolved.
	pairingChannel.release()
	receiverChannel.release()
	if released != len(paths) {
		t.Errorf("admission releases = %d, want %d", released, len(paths))
	}

	if !errors.Is(rejected[paths[0]], ErrSubprotocolRequired) {
		t.Errorf("pairing refusal = %v, want %v", rejected[paths[0]], ErrSubprotocolRequired)
	}
	if !errors.Is(rejected[paths[1]], ErrReceiverUnavailable) {
		t.Errorf("receiver refusal = %v, want %v", rejected[paths[1]], ErrReceiverUnavailable)
	}

	log := logged.String()
	for _, reported := range []string{
		"pairing subscription rejected",
		"session_id=session-a",
		"receiver subscription rejected",
		"code=invalid_request",
	} {
		if !strings.Contains(log, reported) {
			t.Errorf("log does not report %q: %s", reported, log)
		}
	}
}
