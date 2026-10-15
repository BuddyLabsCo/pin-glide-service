package pairing

import (
	"context"
	"database/sql"
	"time"
)

// senderReservationsSQL locks every reservation one Sender's unfinished
// authorized work holds. Locking the rows in a stable order before the
// Sender's own lock is what a claim and a completion do as well, so a
// credential change serializes with them instead of deadlocking.
const senderReservationsSQL = `
SELECT id, pending_pairing_id
FROM pin_glide.pairing_sessions
WHERE authorized_sender_id = $1
  AND state = 'awaiting_confirmation'
  AND pending_pairing_id IS NOT NULL
  AND expires_at > $2
ORDER BY id
FOR UPDATE`

// removeSenderAssociationsSQL removes every association one Sender still owns,
// active or reserved. Matching only that Sender is what keeps another Sender's
// association with the same Receiver out of this change. Rows another request
// already removed are not matched, so a concurrent ordinary removal of the
// same Pairing is reported once.
const removeSenderAssociationsSQL = `
UPDATE pin_glide.pairing_pairings
SET state = 'removed', removed_at = $2
WHERE sender_id = $1 AND state IN ('active', 'pending')
RETURNING id, receiver_id, alias`

// lockSenderReservations locks the reservations one Sender's unfinished
// authorized work holds, so the cleanup that follows cannot leave a
// reservation that would activate an association its Sender no longer owns.
func lockSenderReservations(ctx context.Context, tx *sql.Tx, senderID string, now time.Time) ([]lockedReservation, error) {
	rows, err := tx.QueryContext(ctx, senderReservationsSQL, senderID, now)
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

// removeSenderAssociations marks every association one Sender still holds
// removed and reports them as they were, which is the identity each Receiver's
// notification states.
func removeSenderAssociations(ctx context.Context, tx *sql.Tx, senderID string, now time.Time) ([]Association, error) {
	rows, err := tx.QueryContext(ctx, removeSenderAssociationsSQL, senderID, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var removed []Association
	for rows.Next() {
		var association Association
		if err := rows.Scan(&association.PairingID, &association.ReceiverID, &association.Alias); err != nil {
			return nil, err
		}
		removed = append(removed, association)
	}
	return removed, rows.Err()
}
