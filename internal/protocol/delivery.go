package protocol

import "time"

// CryptoVersion is the only message-encryption profile this version accepts.
// The service validates the envelope it receives and never decrypts content.
const CryptoVersion = 1

const (
	// NonceBytes is the 96-bit AES-GCM nonce the profile requires.
	NonceBytes = 12
	// TagBytes is the full 128-bit authentication tag the profile appends to
	// the ciphertext.
	TagBytes = 16
)

// DeliveryStatus is one state of one Delivery. Processing holds the admitted
// work of one Sender, Message and Receiver; every other state is terminal.
type DeliveryStatus string

const (
	// DeliveryProcessing marks admitted work whose outcome is not stored yet.
	// Only a metadata query reports it.
	DeliveryProcessing DeliveryStatus = "processing"
	// DeliveryReceived marks an authenticated Receiver that reported arrival;
	// it is not proof of decryption, display or copying.
	DeliveryReceived DeliveryStatus = "received"
	// DeliveryRejected marks an entry this invocation did not forward.
	DeliveryRejected DeliveryStatus = "rejected"
	// DeliveryOffline marks an entry without a usable delivery connection.
	DeliveryOffline DeliveryStatus = "offline"
	// DeliverySendFailed marks a delivery write that failed or timed out.
	DeliverySendFailed DeliveryStatus = "send_failed"
	// DeliveryAckTimeout marks the absence of a Receipt within the bound.
	DeliveryAckTimeout DeliveryStatus = "ack_timeout"
	// DeliveryInterrupted marks registered work without a confirmable outcome.
	DeliveryInterrupted DeliveryStatus = "interrupted"
)

// UploadRequest is one Sender's upload: the Message ID it generated once for
// one SMS operation and the independent ciphertext of every Receiver it
// selected. The Sender identity comes from the upload credential rather than
// the payload, and the first three item fields name the Pairings that carry
// the forwarding authority.
type UploadRequest struct {
	MessageID string       `json:"messageId" minLength:"1" maxLength:"128" example:"message-example" doc:"Random Message ID the Sender generated once for this SMS operation; all of its Receiver ciphertexts carry it"`
	Items     []UploadItem `json:"items" nullable:"false" doc:"One entry per selected Receiver; entries are validated and processed independently and answered in input order"`
}

// UploadItem is one Receiver-selected ciphertext. The service validates the
// envelope of this profile and forwards the bytes unchanged, so it never
// decrypts content and never sees the Sender's key material.
type UploadItem struct {
	ReceiverID    string `json:"receiverId" minLength:"1" maxLength:"128" example:"receiver-example" doc:"Receiver this ciphertext is for"`
	PairingID     string `json:"pairingId" minLength:"1" maxLength:"128" example:"pairing-example" doc:"Pairing that authorizes forwarding this ciphertext to that Receiver"`
	CryptoVersion int    `json:"cryptoVersion" example:"1" doc:"Encryption profile version; this version implements 1, and another readable version is rejected with unsupported_crypto_version"`
	Nonce         string `json:"nonce" example:"AAAAAAAAAAAAAAAA" doc:"Padded-Base64 96-bit (12-byte) AES-GCM nonce; the example is synthetic and is never a nonce to reuse"`
	Ciphertext    string `json:"ciphertext" example:"AQIDBAUGBwgJCgsMDQ4PEBESExQ=" doc:"Padded-Base64 ciphertext followed by its 16-byte authentication tag; the example is synthetic bytes, not a working vector"`
}

// UploadResult is the terminal outcome of one input entry. Index is the
// entry's position in the request, and ReceiverID is present only when the
// entry carried a readable Receiver identifier.
type UploadResult struct {
	Index      int            `json:"index" example:"0" doc:"Position of this entry in the request's items array"`
	ReceiverID string         `json:"receiverId,omitempty" example:"receiver-example" doc:"Receiver of this entry; absent when the entry did not carry a readable identifier"`
	Status     DeliveryStatus `json:"status" enum:"received,rejected,offline,send_failed,ack_timeout,interrupted" doc:"Terminal outcome of this entry: received carries no error, every other status carries one"`
	Error      *BusinessError `json:"error,omitempty" doc:"Stable business error of a failed entry; absent for received"`
}

// UploadResponse is the 207 answer of one processable upload: the request's
// correlation once, and one terminal result per input entry.
type UploadResponse struct {
	MessageID string         `json:"messageId" example:"message-example" doc:"Message ID of the request these results belong to"`
	RequestID string         `json:"requestId" example:"req-example" doc:"Service-generated operation identifier for log correlation"`
	Results   []UploadResult `json:"results" nullable:"false" doc:"One result per input entry, in input order"`
}

// DeliveryView is the retained metadata of one Delivery: its Receiver and
// Pairing, its status, when it was admitted and, once terminal, when it
// finished and why it failed. It carries no ciphertext, key or Receipt.
type DeliveryView struct {
	ReceiverID  string         `json:"receiverId" doc:"Receiver this Delivery was admitted for"`
	PairingID   string         `json:"pairingId" doc:"Pairing that authorized this Delivery when it was admitted"`
	Status      DeliveryStatus `json:"status" enum:"processing,received,offline,send_failed,ack_timeout,interrupted" doc:"Current status; processing appears only in a metadata query, never in a synchronous 207 result"`
	CreatedAt   time.Time      `json:"createdAt" doc:"When this Delivery was admitted, in RFC 3339 UTC"`
	CompletedAt *time.Time     `json:"completedAt,omitempty" doc:"When the terminal outcome was stored; absent while processing"`
	Error       *BusinessError `json:"error,omitempty" doc:"Stable business error of a terminal failure; absent for received and processing"`
}

// MessageDeliveries is the retained metadata of one Sender's Message. It lists
// only this Sender's admitted Deliveries; rejected-only input creates none.
type MessageDeliveries struct {
	MessageID  string         `json:"messageId" doc:"Message ID these Deliveries belong to"`
	Deliveries []DeliveryView `json:"deliveries" nullable:"false" doc:"Retained admitted Deliveries of this Message"`
}
