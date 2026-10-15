package pairing

import (
	"context"
	"database/sql"
	"time"
)

// newerIssuanceSQL reports whether a session of this Receiver that is not older
// than this one already issued a credential, which is what makes this session's
// own grant stale: the generation that already works is the one to keep, and a
// delayed first issuance never revokes it. Creation time orders the sessions of
// one Receiver, and two sessions created in the same instant resolve to the one
// that issued first, so the ordering never lets a later redemption replace a
// credential an issuance already handed out.
const newerIssuanceSQL = `
SELECT EXISTS (
    SELECT 1
    FROM pin_glide.pairing_sessions
    WHERE receiver_id = $1
      AND id <> $2
      AND issued_generation IS NOT NULL
      AND created_at >= $3
)`

// replacedGrantSQL closes the Receiver's issuance grants that one issuance
// replaced: the sessions that are not newer than the issuing one and still hold
// a grant of their own. It is the other half of the ordering newerIssuanceSQL
// decides, so a grant that can no longer be redeemed reports its own outcome
// instead of leaving its holder to discover it by redemption. Only sessions
// that hold a grant are closed, and their Pairing is already active, so no
// unfinished pairing work and no association ends here.
const replacedGrantSQL = `
UPDATE pin_glide.pairing_sessions
SET state = 'superseded', closed_at = $4
WHERE receiver_id = $1
  AND id <> $2
  AND created_at <= $3
  AND state IN ('credential_ready', 'credential_issued')
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

// newerIssuance reports whether a session of this Receiver that is not older
// than this one already issued the credential that works.
func newerIssuance(ctx context.Context, tx *sql.Tx, session Session) (bool, error) {
	var newer bool
	err := tx.QueryRowContext(ctx, newerIssuanceSQL,
		session.ReceiverID, session.ID, session.CreatedAt).Scan(&newer)
	return newer, err
}

// closeReplacedGrants closes and reports the Receiver's issuance grants that
// the issuing session replaced, so their participants read the outcome of the
// grant that can no longer be redeemed.
func closeReplacedGrants(ctx context.Context, tx *sql.Tx, session Session, now time.Time) ([]string, error) {
	rows, err := tx.QueryContext(ctx, replacedGrantSQL,
		session.ReceiverID, session.ID, session.CreatedAt, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var replaced []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, err
		}
		replaced = append(replaced, sessionID)
	}
	return replaced, rows.Err()
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
