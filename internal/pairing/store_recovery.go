package pairing

import (
	"context"
	"database/sql"
	"time"
)

// receiverPairingsSQL reads every Pairing of one Receiver, whatever its state.
// A Receiver may still hold the key of a Pairing it no longer forwards under,
// so its own key inventory is checked against these identities rather than
// against its active associations alone.
const receiverPairingsSQL = `
SELECT id
FROM pin_glide.pairing_pairings
WHERE receiver_id = $1
ORDER BY id`

// receiverAssociationsSQL locks every active association of one Receiver
// together with the Sender that owns it, which is what a recovery removal
// states. The rows are locked in a stable order, so two recoveries of the same
// Receiver serialize instead of deadlocking.
const receiverAssociationsSQL = `
SELECT id, sender_id
FROM pin_glide.pairing_pairings
WHERE receiver_id = $1 AND state = 'active'
ORDER BY id
FOR UPDATE`

// receiverReservationsSQL locks the unfinished pairing work of one Receiver:
// the claimed sessions that still hold a pending reservation, which a recovery
// keeps only where the Receiver still holds the reserved key. An expired
// session releases its reservation without any cleanup, so it is not locked.
const receiverReservationsSQL = `
SELECT id, pending_pairing_id
FROM pin_glide.pairing_sessions
WHERE receiver_id = $1
  AND state = 'awaiting_confirmation'
  AND pending_pairing_id IS NOT NULL
  AND expires_at > $2
ORDER BY id
FOR UPDATE`

// requireReceiverPairings checks that every reported Pairing ID belongs to the
// Receiver that reported its key inventory. A set that names anything else is
// rejected instead of acting on another Receiver's Pairings.
func requireReceiverPairings(ctx context.Context, tx *sql.Tx, receiverID string, kept map[string]bool) error {
	if len(kept) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, receiverPairingsSQL, receiverID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	owned := map[string]bool{}
	for rows.Next() {
		var pairingID string
		if err := rows.Scan(&pairingID); err != nil {
			return err
		}
		owned[pairingID] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for pairingID := range kept {
		if !owned[pairingID] {
			return ErrRetainedPairingsInvalid
		}
	}
	return nil
}

// lockReceiverAssociations reads every active association of one Receiver for
// the rest of the transaction.
func lockReceiverAssociations(ctx context.Context, tx *sql.Tx, receiverID string) ([]lockedAssociation, error) {
	rows, err := tx.QueryContext(ctx, receiverAssociationsSQL, receiverID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var associations []lockedAssociation
	for rows.Next() {
		var association lockedAssociation
		if err := rows.Scan(&association.pairingID, &association.senderID); err != nil {
			return nil, err
		}
		associations = append(associations, association)
	}
	return associations, rows.Err()
}

// lockedAssociation is one of a Receiver's active associations together with
// the Sender that owns it.
type lockedAssociation struct {
	pairingID string
	senderID  string
}

// lockReceiverReservations locks the unfinished pairing work of one Receiver,
// so a recovery decides about it before anything can activate it.
func lockReceiverReservations(ctx context.Context, tx *sql.Tx, receiverID string, now time.Time) ([]lockedReservation, error) {
	rows, err := tx.QueryContext(ctx, receiverReservationsSQL, receiverID, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var reservations []lockedReservation
	for rows.Next() {
		var reservation lockedReservation
		if err := rows.Scan(&reservation.sessionID, &reservation.pairingID); err != nil {
			return nil, err
		}
		reservations = append(reservations, reservation)
	}
	return reservations, rows.Err()
}
