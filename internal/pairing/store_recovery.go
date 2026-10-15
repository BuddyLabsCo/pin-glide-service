package pairing

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// newerIssuanceSQL reports whether a session after this one already issued a
// credential for the Receiver, which is what makes this session's own grant
// stale: the generation a newer session issued is the one that works. Creation
// time and then the identifier order every session of one Receiver, so two
// sessions created in the same instant still have one order and an older grant
// cannot slip through.
const newerIssuanceSQL = `
SELECT EXISTS (
    SELECT 1
    FROM pin_glide.pairing_sessions
    WHERE receiver_id = $1
      AND id <> $2
      AND issued_generation IS NOT NULL
      AND (created_at > $3 OR (created_at = $3 AND id > $2))
)`

// removeCurrentAssociationSQL withdraws one Sender's current association with
// one Receiver, whatever Pairing ID it has. Matching the Sender, the Receiver
// and the active state together is what makes a concurrent replacement or
// removal visible, so a stale report cannot remove whatever is current.
const removeCurrentAssociationSQL = `
UPDATE pin_glide.pairing_pairings
SET state = 'removed', removed_at = $3
WHERE sender_id = $1 AND receiver_id = $2 AND state = 'active'
RETURNING id`

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

// supersedeIfStale closes this session as superseded, and reports that it did,
// when a newer session of the same Receiver already issued the credential that
// works. A delayed first issuance therefore never replaces a working newer
// generation.
func supersedeIfStale(ctx context.Context, tx *sql.Tx, session Session, now time.Time) (bool, error) {
	var newer bool
	if err := tx.QueryRowContext(ctx, newerIssuanceSQL,
		session.ReceiverID, session.ID, session.CreatedAt).Scan(&newer); err != nil {
		return false, err
	}
	if !newer {
		return false, nil
	}
	return true, closeSession(ctx, tx, session, StateSuperseded, now)
}

// removeCurrentAssociation withdraws one Sender's current association with one
// Receiver and reports the Pairing ID it withdrew. A Sender that holds no
// association with that Receiver has nothing to withdraw, which its own
// missing-key report states.
func removeCurrentAssociation(ctx context.Context, tx *sql.Tx, senderID, receiverID string, now time.Time) (string, error) {
	var pairingID string
	err := tx.QueryRowContext(ctx, removeCurrentAssociationSQL, senderID, receiverID, now).Scan(&pairingID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return pairingID, err
}

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
