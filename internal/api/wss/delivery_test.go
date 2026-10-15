package wss

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// deliveryFrame is one synthetic delivery message of the accepted envelope.
var deliveryFrame = protocol.Delivery{
	Version:       protocol.WSSVersion,
	Type:          "delivery",
	SenderID:      "sender-1",
	ReceiverID:    "receiver-1",
	PairingID:     "pairing-1",
	MessageID:     "message-1",
	CryptoVersion: protocol.CryptoVersion,
	Nonce:         "AAAAAAAAAAAAAAAA",
	Ciphertext:    "AQIDBAUGBwgJCgsMDQ4PEBESExQ=",
}

// receiptSink records the Receipts one channel accepted, with the Receiver each
// one was attributed to.
type receiptSink struct {
	mutex     sync.Mutex
	senderID  string
	messageID string
	receiver  string
	accepted  int
}

// Receipt records one accepted Receipt frame.
func (s *receiptSink) Receipt(senderID, messageID, receiverID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.senderID, s.messageID, s.receiver = senderID, messageID, receiverID
	s.accepted++
}

// reading reports how many Receipts this sink accepted.
func (s *receiptSink) reading() (int, string, string, string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.accepted, s.senderID, s.messageID, s.receiver
}

// A delivery goes to the Receiver's current committed connection with its
// envelope unchanged, and a Receiver that holds no connection reports offline
// rather than a write the service could not make.
func TestDeliveryUsesTheCommittedConnectionOnly(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	channel := newReceiverChannel(t, newReceiverStub("receiver-1"), presence)
	sink := &receiptSink{}
	channel.socket.SetReceiptSink(sink)

	if delivered, err := channel.socket.Deliver("receiver-1", deliveryFrame, time.Second); delivered || err != nil {
		t.Fatalf("delivery without a connection = %v, %v; want not delivered and no error", delivered, err)
	}

	socket := channel.connected(t, "receiver-1:1")
	awaitPresence(t, presence, "receiver-1", true)

	delivered, err := channel.socket.Deliver("receiver-1", deliveryFrame, time.Second)
	if !delivered || err != nil {
		t.Fatalf("delivery to a committed connection = %v, %v; want delivered", delivered, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messageType, data, err := socket.Read(ctx)
	if err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("delivery arrived as message type %v, want text", messageType)
	}
	var received protocol.Delivery
	if err := json.Unmarshal(data, &received); err != nil {
		t.Fatalf("decode delivery: %v: %s", err, data)
	}
	if received != deliveryFrame {
		t.Errorf("delivery = %+v, want the frame unchanged %+v", received, deliveryFrame)
	}

	// A Receipt of this version reaches the sink attributed to the Receiver the
	// connection authenticated, never to a Receiver the frame names.
	if err := socket.Write(ctx, websocket.MessageText,
		[]byte(`{"version":1,"type":"receipt","senderId":"sender-1","messageId":"message-1"}`)); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		accepted, senderID, messageID, receiverID := sink.reading()
		if accepted > 0 {
			if senderID != "sender-1" || messageID != "message-1" || receiverID != "receiver-1" {
				t.Errorf("accepted Receipt = %q %q %q, want the frame and the authenticated Receiver", senderID, messageID, receiverID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the committed connection's Receipt never reached the sink")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A frame this direction does not accept carries no usable correlation, so it is
// a protocol error: the connection closes with 1002 instead of the service
// fabricating a Receipt or silently ignoring traffic.
func TestFrameWithoutReceiptCorrelationClosesTheConnection(t *testing.T) {
	presence := NewPresence(time.Now().UTC())
	channel := newReceiverChannel(t, newReceiverStub("receiver-1"), presence)
	sink := &receiptSink{}
	channel.socket.SetReceiptSink(sink)
	socket := channel.connected(t, "receiver-1:1")
	awaitPresence(t, presence, "receiver-1", true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := socket.Write(ctx, websocket.MessageText, []byte(`{"version":1,"type":"receipt"}`)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if _, _, err := socket.Read(ctx); websocket.CloseStatus(err) != websocket.StatusProtocolError {
		t.Fatalf("close after an uncorrelated frame = %v, want 1002", err)
	}
	if accepted, _, _, _ := sink.reading(); accepted != 0 {
		t.Errorf("the channel accepted %d Receipts without correlation", accepted)
	}
}
