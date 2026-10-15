package delivery

import (
	"encoding/base64"
	"errors"
	"testing"
)

// version returns one readable cryptoVersion field.
func version(value int) *int { return &value }

// bytes returns the padded Base64 of n synthetic bytes with the given first
// value, so a test can build profile values of an exact decoded length without
// carrying a real ciphertext.
func bytes(n int, first byte) string {
	raw := make([]byte, n)
	if n > 0 {
		raw[0] = first
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// The service validates the accepted encryption profile without decrypting
// content: the crypto version, the padded-Base64 encoding, the 96-bit nonce and
// the ciphertext envelope a 128-bit tag needs are what decide an entry, and a
// value that only looks like ciphertext is accepted.
func TestItemValidationAcceptsTheProfileAndRejectsItsViolations(t *testing.T) {
	tests := []struct {
		name string
		item Item
		want error
	}{
		{
			name: "the accepted profile syntax",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(12, 0), Ciphertext: bytes(17, 0x01)},
			want: nil,
		},
		{
			name: "a longer ciphertext of the same profile",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(12, 0xff), Ciphertext: bytes(65, 0x02)},
			want: nil,
		},
		{
			name: "a readable version this service does not implement",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(2),
				Nonce: bytes(12, 0), Ciphertext: bytes(17, 0x01)},
			want: ErrUnsupportedCryptoVersion,
		},
		{
			name: "no crypto version at all",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a",
				Nonce: bytes(12, 0), Ciphertext: bytes(17, 0x01)},
			want: ErrInvalidItem,
		},
		{
			name: "no receiver",
			item: Item{PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(12, 0), Ciphertext: bytes(17, 0x01)},
			want: ErrInvalidItem,
		},
		{
			name: "no pairing",
			item: Item{ReceiverID: "receiver-a", CryptoVersion: version(1),
				Nonce: bytes(12, 0), Ciphertext: bytes(17, 0x01)},
			want: ErrInvalidItem,
		},
		{
			name: "a nonce shorter than 96 bits",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(11, 0), Ciphertext: bytes(17, 0x01)},
			want: ErrInvalidItem,
		},
		{
			name: "a nonce longer than 96 bits",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(13, 0), Ciphertext: bytes(17, 0x01)},
			want: ErrInvalidItem,
		},
		{
			name: "an unpadded ciphertext",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(12, 0), Ciphertext: base64.RawStdEncoding.EncodeToString(make([]byte, 17))},
			want: ErrInvalidItem,
		},
		{
			name: "a value that is not Base64 at all",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(12, 0), Ciphertext: "not-base64!!"},
			want: ErrInvalidItem,
		},
		{
			name: "a ciphertext that can hold no payload beside its tag",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(12, 0), Ciphertext: bytes(16, 0x01)},
			want: ErrInvalidItem,
		},
		{
			name: "an empty ciphertext",
			item: Item{ReceiverID: "receiver-a", PairingID: "pairing-a", CryptoVersion: version(1),
				Nonce: bytes(12, 0)},
			want: ErrInvalidItem,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.item.validate(); !errors.Is(err, test.want) {
				t.Errorf("validation = %v, want %v", err, test.want)
			}
		})
	}
}

// Only a readable Receiver can be duplicated: an entry the transport could not
// read names no Receiver to collide on, and naming one twice is what makes the
// service refuse to choose a ciphertext for it.
func TestDuplicatedReceiversCountsOnlyReadableReceivers(t *testing.T) {
	items := []Item{
		{ReceiverID: "receiver-a"},
		{ReceiverID: "receiver-b"},
		{ReceiverID: "receiver-a"},
		{},
		{},
		{ReceiverID: "receiver-c"},
	}

	duplicated := duplicatedReceivers(items)
	if !duplicated["receiver-a"] {
		t.Error("two entries naming receiver-a were not reported as duplicated")
	}
	for _, receiverID := range []string{"receiver-b", "receiver-c", ""} {
		if duplicated[receiverID] {
			t.Errorf("receiver %q was reported as duplicated by a single readable entry", receiverID)
		}
	}
}
