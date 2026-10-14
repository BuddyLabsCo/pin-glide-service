package pairing

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/enum"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/table"
	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"
)

// activeAssociationSQL reads one Sender's own active association by its
// Pairing ID, returning the Receiver a removal has to act on. It matches the
// owning Sender and the active state together, so a Pairing that another
// Sender owns or that was already removed is not an association this caller
// may act on.
const activeAssociationSQL = `
SELECT id, receiver_id, alias
FROM pin_glide.pairing_pairings
WHERE id = $1 AND sender_id = $2 AND state = 'active'`

// liveReservationsSQL locks the Sender's reservations that may still activate
// an association with one Receiver. Only a claimed session that has not
// activated holds one, and an expired session releases it without any cleanup,
// so this is exactly the Sender's unfinished work for that Receiver. The rows
// are locked in a stable order before the Sender's own lock is taken, which is
// the same order a claim and a completion take them in, so concurrent work
// waits instead of deadlocking.
const liveReservationsSQL = `
SELECT id, pending_pairing_id
FROM pin_glide.pairing_sessions
WHERE authorized_sender_id = $1
  AND receiver_id = $2
  AND state = 'awaiting_confirmation'
  AND pending_pairing_id IS NOT NULL
  AND expires_at > $3
ORDER BY id
FOR UPDATE`

// removeAssociationSQL removes one Sender's own active association. Matching
// the identity and the active state together is what makes a concurrent
// replacement visible: an association another request already replaced or
// removed leaves this removal with no effect instead of removing whatever is
// current under a stale identity. The association row itself stays, so the
// removed identity keeps its history and no cascade reaches retained records.
const removeAssociationSQL = `
UPDATE pin_glide.pairing_pairings
SET state = 'removed', removed_at = $3
WHERE id = $1 AND sender_id = $2 AND state = 'active'
RETURNING receiver_id, alias`

// renameAssociationSQL renames one Sender's own active association. Only the
// owning Sender's own alias changes; the Pairing identity, its Receiver and
// every other Sender's association are untouched.
const renameAssociationSQL = `
UPDATE pin_glide.pairing_pairings
SET alias = $3
WHERE id = $1 AND sender_id = $2 AND state = 'active'`

// activeAssociations lists one Sender's own active associations in the order
// they were established. A pending reservation is not listed: it is not an
// association yet.
func activeAssociations(ctx context.Context, db qrm.Queryable, senderID string) ([]Association, error) {
	var rows []model.PairingPairings
	err := table.PairingPairings.
		SELECT(table.PairingPairings.ID, table.PairingPairings.ReceiverID, table.PairingPairings.Alias_).
		FROM(table.PairingPairings).
		WHERE(table.PairingPairings.SenderID.EQ(postgres.String(senderID)).
			AND(table.PairingPairings.State.EQ(enum.PairingState.Active))).
		ORDER_BY(table.PairingPairings.CreatedAt.ASC(), table.PairingPairings.ID.ASC()).
		QueryContext(ctx, db, &rows)
	if err != nil {
		return nil, err
	}

	associations := make([]Association, 0, len(rows))
	for _, row := range rows {
		associations = append(associations, Association{
			ReceiverID: row.ReceiverID,
			PairingID:  row.ID,
			Alias:      row.Alias,
		})
	}
	return associations, nil
}

// activeAssociation reads one Sender's own active association inside the
// caller's transaction. Everything the caller may not act on reports the same
// not-found outcome.
func activeAssociation(ctx context.Context, tx *sql.Tx, senderID, pairingID string) (Association, error) {
	var association Association
	err := tx.QueryRowContext(ctx, activeAssociationSQL, pairingID, senderID).
		Scan(&association.PairingID, &association.ReceiverID, &association.Alias)
	if errors.Is(err, sql.ErrNoRows) {
		return Association{}, ErrAssociationNotFound
	}
	if err != nil {
		return Association{}, err
	}
	return association, nil
}

// lockedReservation is one of the Sender's unfinished reservations for a
// Receiver: the session that holds it and the Pairing ID it reserved.
type lockedReservation struct {
	sessionID string
	pairingID string
}

// lockLiveReservations locks the Sender's reservations that may still activate
// an association with one Receiver.
func lockLiveReservations(ctx context.Context, tx *sql.Tx, senderID, receiverID string, now time.Time) ([]lockedReservation, error) {
	rows, err := tx.QueryContext(ctx, liveReservationsSQL, senderID, receiverID, now)
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

// renameAssociation writes one Sender's own alias and reports the association
// as it now is. A Pairing that is not this Sender's active association reports
// the not-found outcome instead of creating one.
func renameAssociation(ctx context.Context, tx *sql.Tx, senderID, pairingID, alias string) (Association, error) {
	result, err := tx.ExecContext(ctx, renameAssociationSQL, pairingID, senderID, alias)
	if err != nil {
		return Association{}, err
	}
	renamed, err := result.RowsAffected()
	if err != nil {
		return Association{}, err
	}
	if renamed != 1 {
		return Association{}, ErrAssociationNotFound
	}
	return activeAssociation(ctx, tx, senderID, pairingID)
}

// removeAssociation marks one Sender's own active association removed and
// reports the association as it was, which is the identity the Receiver's
// notification states. An association that changed while this removal waited
// for its turn reports the not-found outcome and leaves whatever is current
// untouched.
func removeAssociation(ctx context.Context, tx *sql.Tx, senderID, pairingID string, now time.Time) (Association, error) {
	association := Association{PairingID: pairingID}
	err := tx.QueryRowContext(ctx, removeAssociationSQL, pairingID, senderID, now).
		Scan(&association.ReceiverID, &association.Alias)
	if errors.Is(err, sql.ErrNoRows) {
		return Association{}, ErrAssociationNotFound
	}
	if err != nil {
		return Association{}, err
	}
	return association, nil
}
