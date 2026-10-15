package wss

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// ReceiptSink accepts one Receipt frame a committed delivery connection sent.
// The delivery domain decides whether the Receipt belongs to live work; the
// transport only checks the frame's shape and attributes it to the Receiver
// the connection authenticated.
type ReceiptSink interface {
	Receipt(senderID, messageID, receiverID string)
}

// SetReceiptSink installs the sink of Receipt frames. A process composed
// without one keeps serving; only Receipt handling stops.
func (s *ReceiverSocket) SetReceiptSink(receipts ReceiptSink) {
	s.receipts = receipts
}

// Deliver writes one delivery frame to the Receiver's current committed
// connection within timeout. A Receiver that holds no committed connection
// reports delivered=false and no error, which is the offline outcome; a write
// that failed reports an error, because arrival is then not guaranteed absent.
// The frame is the exact v1 delivery message and carries the Sender's bytes
// unchanged.
func (s *ReceiverSocket) Deliver(receiverID string, frame protocol.Delivery, timeout time.Duration) (bool, error) {
	socket := s.committedSocket(receiverID)
	if socket == nil {
		return false, nil
	}

	encoded, err := json.Marshal(frame)
	if err != nil {
		// Every field of a delivery message is a plain value, so this cannot
		// happen for a frame the service builds itself.
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := socket.Write(ctx, websocket.MessageText, encoded); err != nil {
		return false, err
	}
	return true, nil
}

// committedSocket reports the accepted socket that currently serves one
// Receiver. A Receiver's previous generation is removed when its replacement
// commits, and a reservation that has not committed holds no socket, so this
// is the connection a delivery of this moment may use.
func (s *ReceiverSocket) committedSocket(receiverID string) *websocket.Conn {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for key, slot := range s.slots {
		if key.receiverID == receiverID && slot.socket != nil {
			return slot.socket
		}
	}
	return nil
}

// accept reads one application frame of a committed delivery connection. Only
// the Receipt message of this version is acceptable: text the service cannot
// read is an invalid payload, and any other frame is a protocol error rather
// than traffic the service silently ignores. Deriving the Receiver from the
// connection is what stops a client from acknowledging another Receiver's
// Delivery.
func (s *ReceiverSocket) accept(receiverID string, messageType websocket.MessageType, data []byte) (closeIntent, bool) {
	if messageType != websocket.MessageText {
		return closeProtocolError, false
	}
	if !utf8.Valid(data) {
		return closeInvalidPayload, false
	}
	receipt, err := decodeReceipt(data)
	if err != nil {
		return closeProtocolError, false
	}
	if s.receipts != nil {
		s.receipts.Receipt(receipt.SenderID, receipt.MessageID, receiverID)
	}
	return closeNothing, true
}

// errNotReceipt marks a frame that is not a Receipt of this protocol version.
// Without readable correlation the service has no arrival to acknowledge, so
// such a frame is a protocol error rather than a fabricated Receipt.
var errNotReceipt = errors.New("frame is not a receipt")

// decodeReceipt validates one Receipt frame. Both correlation identifiers are
// required: a Receipt without the Sender and Message it acknowledges is not
// readable correlation.
func decodeReceipt(data []byte) (protocol.Receipt, error) {
	var receipt protocol.Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return protocol.Receipt{}, errNotReceipt
	}
	if receipt.Version != protocol.WSSVersion || receipt.Type != "receipt" ||
		receipt.SenderID == "" || receipt.MessageID == "" {
		return protocol.Receipt{}, errNotReceipt
	}
	return receipt, nil
}
