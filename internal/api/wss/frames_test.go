package wss

import (
	"encoding/base64"
	"errors"
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
		} else if intent := relayCloseIntent(err); intent != closeProtocolError {
			t.Errorf("%s closes with intent %d, want the protocol error", name, intent)
		}
	}
}

// Text the service cannot read as UTF-8 is an invalid payload rather than a
// version, type or shape violation, so it has its own Close meaning.
func TestRelayFrameCodecClassifiesInvalidTextEncoding(t *testing.T) {
	frame := append([]byte(`{"version":1,"type":"pairing_relay","sessionId":"session-a","payload":"`), 0xff)
	frame = append(frame, []byte(`"}`)...)

	_, err := decodeRelay(frame)
	if err == nil {
		t.Fatal("a frame that is not valid UTF-8 was accepted")
	}
	if intent := relayCloseIntent(err); intent != closeInvalidPayload {
		t.Errorf("invalid text encoding closes with intent %d, want the invalid payload", intent)
	}
	if code := frameErrorCodeOf(err); code != protocol.InvalidRequest {
		t.Errorf("invalid text encoding reports %q, want %q", code, protocol.InvalidRequest)
	}
}

// The classification stays total: an unexpected failure is a protocol error
// rather than an invalid payload, so a bug does not become a client-visible
// encoding outcome.
func TestRelayCloseIntentDefaultsToAProtocolError(t *testing.T) {
	if intent := relayCloseIntent(errors.New("unexpected")); intent != closeProtocolError {
		t.Errorf("an unexpected frame failure closes with intent %d, want the protocol error", intent)
	}
}
