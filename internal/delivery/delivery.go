// Package delivery owns Delivery admission, deduplication, Receipt handling
// and terminal results. It coordinates live work in this process and keeps the
// durable identity each Delivery is deduplicated by; the ciphertext itself is
// never persisted and never logged.
package delivery

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

const (
	// WriteTimeout bounds one delivery write to the Receiver's connection.
	WriteTimeout = 2 * time.Second
	// ReceiptTimeout bounds how long the service waits for the Receiver's
	// Receipt after a successful write.
	ReceiptTimeout = 5 * time.Second
	// BatchBudget bounds one upload as a whole. It is an upper bound, not a
	// fixed delay, and never a promise about an end-to-end operation.
	BatchBudget = 15 * time.Second
)

// Request is one upload after the transport read its envelope: the Message ID
// the Sender generated once for one SMS operation, and one entry per selected
// Receiver in input order.
type Request struct {
	MessageID string
	Items     []Item
}

// Item is one input entry as the transport could read it. A field the
// transport could not read as its expected type stays at its zero value, and
// CryptoVersion is nil when it was absent or was not a readable integer, so
// the entry's own validation decides its outcome.
type Item struct {
	ReceiverID    string
	PairingID     string
	CryptoVersion *int
	Nonce         string
	Ciphertext    string
}

// Result is the terminal outcome of one input entry: its original index, the
// Receiver when the entry carried a readable one, the status clients see and
// the typed failure behind a failed status.
type Result struct {
	Index      int
	ReceiverID string
	Status     protocol.DeliveryStatus
	Err        error
}

// Delivery is the retained metadata of one Delivery. It never carries a
// ciphertext, a key or a Receipt.
type Delivery struct {
	ReceiverID  string
	PairingID   string
	Status      protocol.DeliveryStatus
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// Channel is the live transport seam of the delivery domain: it writes one
// frame to the Receiver's current committed delivery connection. A Receiver
// without a usable connection reports delivered=false and no error, which is
// the offline outcome; a write that failed reports an error, because arrival
// is then not guaranteed absent.
type Channel interface {
	Deliver(receiverID string, frame protocol.Delivery, timeout time.Duration) (delivered bool, err error)
}

// Authenticator resolves the upload authority one request presents. The
// identity service owns those rules, so this domain reuses them instead of
// restating them, and reports the credential outcomes its transports map.
type Authenticator interface {
	Authenticate(ctx context.Context, rawCredential string) (string, error)
}

// Errors the delivery operations report. Callers distinguish outcomes by
// these typed causes; the HTTP surface maps them to the stable business codes
// the published contract defines.
var (
	// ErrReceiverNotAuthorized marks an entry naming a Receiver that is unknown
	// to this Sender, is not its own active association, or is revoked. The
	// four reasons stay distinct internally and share this one external
	// outcome, so no caller can confirm another Account's registrations.
	ErrReceiverNotAuthorized = errors.New("receiver not authorized")
	// errReceiverUnknown marks a Receiver identifier no registration holds.
	errReceiverUnknown = errors.New("receiver is unknown")
	// errReceiverRevoked marks a Receiver whose registration is permanently
	// closed.
	errReceiverRevoked = errors.New("receiver is revoked")
	// errReceiverUnpaired marks a Receiver that is not the Sender's own active
	// association under the Pairing the entry named.
	errReceiverUnpaired = errors.New("receiver is not the sender's active association")
	// errReceiverForeign marks a Receiver whose registration belongs to another
	// Account. It stays distinct from an unknown Receiver for diagnosis while
	// sharing the same external outcome, so no caller learns whether the
	// registration exists.
	errReceiverForeign = errors.New("receiver belongs to another account")
	// ErrDuplicateReceiver marks an entry whose Receiver another entry of the
	// same request also named.
	ErrDuplicateReceiver = errors.New("duplicate receiver entry")
	// ErrInvalidItem marks an entry that is not a readable, well-formed
	// ciphertext envelope of this profile.
	ErrInvalidItem = errors.New("invalid delivery item")
	// ErrUnsupportedCryptoVersion marks an entry whose readable cryptoVersion
	// this service version does not implement.
	ErrUnsupportedCryptoVersion = errors.New("unsupported crypto version")
	// ErrOffline marks an entry whose Receiver holds no usable connection.
	ErrOffline = errors.New("receiver offline")
	// ErrSendFailed marks a delivery write that failed or exceeded its bound.
	ErrSendFailed = errors.New("delivery send failed")
	// ErrAckTimeout marks the absence of a Receipt within the adopted bound.
	ErrAckTimeout = errors.New("receipt not observed")
	// ErrInterrupted marks registered work without a confirmable outcome.
	ErrInterrupted = errors.New("delivery interrupted")
	// ErrStatusUnavailable marks a reliable terminal outcome or retained
	// metadata that could not be obtained, which is an uncertain result
	// because delivery may already have occurred.
	ErrStatusUnavailable = errors.New("delivery status unavailable")
	// ErrMessageNotFound marks a Message without retained admitted Deliveries
	// of this Sender, including one whose rows cleanup already deleted.
	ErrMessageNotFound = errors.New("message metadata not found")
)

// terminalErrors maps every stored terminal status to the typed failure it
// reports. A status without an entry is success or ongoing work.
var terminalErrors = map[protocol.DeliveryStatus]error{
	protocol.DeliveryOffline:     ErrOffline,
	protocol.DeliverySendFailed:  ErrSendFailed,
	protocol.DeliveryAckTimeout:  ErrAckTimeout,
	protocol.DeliveryInterrupted: ErrInterrupted,
}

// TerminalError returns the typed failure of one stored status, or nil when
// that status reports arrival or ongoing work.
func TerminalError(status protocol.DeliveryStatus) error {
	return terminalErrors[status]
}

// validate reports the item-level failure of one entry. The service validates
// the envelope of the adopted profile and never decrypts content: another
// readable cryptoVersion is unsupported, and a missing field, an unpadded or
// non-Base64 value, a nonce that is not 96 bits or a ciphertext that cannot
// hold a 128-bit tag and a payload is an invalid item.
func (item Item) validate() error {
	if item.ReceiverID == "" || item.PairingID == "" {
		return ErrInvalidItem
	}
	if item.CryptoVersion == nil {
		return ErrInvalidItem
	}
	if *item.CryptoVersion != protocol.CryptoVersion {
		return ErrUnsupportedCryptoVersion
	}
	if !decodedLength(item.Nonce, protocol.NonceBytes, true) || !decodedLength(item.Ciphertext, protocol.TagBytes+1, false) {
		return ErrInvalidItem
	}
	return nil
}

// decodedLength reports whether value is padded Base64 of an acceptable
// length: exactly want when fixed, or at least want when it is a lower bound.
func decodedLength(value string, want int, fixed bool) bool {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return false
	}
	if fixed {
		return len(decoded) == want
	}
	return len(decoded) >= want
}
