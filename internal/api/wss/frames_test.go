package wss

import (
	"encoding/base64"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// A relay frame must be a padded-Base64 pairing_relay of this version, so a
// client cannot send anything else through the opaque relay.
func TestRelayFrameCodecRejectsMalformedFrames(t *testing.T) {
	valid := `{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":"` +
		base64.StdEncoding.EncodeToString([]byte("opaque")) + `"}`
	message, err := decodeRelay([]byte(valid))
	if err != nil {
		t.Fatalf("a valid frame was rejected: %v", err)
	}
	if message.SessionID != "session-a" {
		t.Errorf("decoded session = %q, want session-a", message.SessionID)
	}

	// An unknown optional field is compatible with the published definition, so
	// a client that sent one still relays.
	extended := `{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":"AAAA","futureField":true}`
	if _, err := decodeRelay([]byte(extended)); err != nil {
		t.Errorf("a frame carrying an unknown optional field was rejected: %v", err)
	}

	for name, frame := range map[string][]byte{
		"not JSON":        []byte(`{`),
		"wrong version":   []byte(`{"version":2,"type":"pairing_relay","sessionId":"session-a","payload":"AAAA"}`),
		"wrong type":      []byte(`{"version":1,"type":"pairing_state","sessionId":"session-a","payload":"AAAA"}`),
		"unknown type":    []byte(`{"version":1,"type":"delivery","sessionId":"session-a","payload":"AAAA"}`),
		"missing session": []byte(`{"version":1,"type":"pairing_relay","payload":"AAAA"}`),
		"missing payload": []byte(`{"version":1,"type":"pairing_relay","sessionId":"session-a"}`),
		"empty payload":   []byte(`{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":""}`),
		"invalid base64":  []byte(`{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":"not base64!"}`),
		"unpadded base64": []byte(`{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":"AAA"}`),
	} {
		if _, err := decodeRelay(frame); err == nil {
			t.Errorf("%s was accepted", name)
		} else if code := frameErrorCodeOf(err); code != protocol.InvalidRequest {
			t.Errorf("%s reported %q, want %q", name, code, protocol.InvalidRequest)
		}
	}
}
