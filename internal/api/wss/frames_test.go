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

	for name, frame := range map[string]string{
		"not JSON":        `{`,
		"wrong version":   `{"version":2,"type":"pairing_relay","sessionId":"session-a","payload":"AAAA"}`,
		"wrong type":      `{"version":1,"type":"pairing_state","sessionId":"session-a","payload":"AAAA"}`,
		"missing session": `{"version":1,"type":"pairing_relay","payload":"AAAA"}`,
		"missing payload": `{"version":1,"type":"pairing_relay","sessionId":"session-a"}`,
		"empty payload":   `{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":""}`,
		"invalid base64":  `{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":"not base64!"}`,
		"unpadded base64": `{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":"AAA"}`,
	} {
		if _, err := decodeRelay([]byte(frame)); err == nil {
			t.Errorf("%s was accepted", name)
		} else if code := frameErrorCodeOf(err); code != protocol.InvalidRequest {
			t.Errorf("%s reported %q, want %q", name, code, protocol.InvalidRequest)
		}
	}
}
