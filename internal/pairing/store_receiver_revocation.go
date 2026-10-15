package pairing

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/table"
	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"
)

// receiverGrantsSQL locks every grant one Receiver could still redeem: the
// issuance a live session still owes, whichever pairing work its Sender started,
// and the recovery that could still replace the credential. A session that
// completed, closed or expired holds nothing, so this is exactly the work a
// closure invalidates. The rows are locked in a stable order before the
// associations, which is the order a completion takes them in.
const receiverGrantsSQL = `
SELECT id, pending_pairing_id
FROM pin_glide.pairing_sessions
WHERE receiver_id = $1
  AND state IN ('awaiting_authorization', 'awaiting_confirmation', 'credential_ready', 'credential_issued')
  AND expires_at > $2
ORDER BY id
FOR UPDATE`

// closeReceiverRegistrationSQL permanently closes one Receiver registration and
// records when owner security revocation did it. The row stays, which is what a
// later request is refused against, and the state and its time are written
// together, which the table also enforces.
const closeReceiverRegistrationSQL = `
UPDATE pin_glide.identity_receiver_registrations
SET state = 'revoked', revoked_at = $2
WHERE id = $1`

// removeReceiverAssociationsSQL removes every association that still forwards to
// one Receiver, whichever Sender owns it, active or reserved. The rows stay as
// removed history: only forwarding authority ends here, and no retained record
// is deleted with it.
const removeReceiverAssociationsSQL = `
UPDATE pin_glide.pairing_pairings
SET state = 'removed', removed_at = $2
WHERE receiver_id = $1 AND state IN ('active', 'pending')`

// senderRegistrationExists reports whether deviceID names a Sender
// registration, so a device revocation acts on the role that registration has.
func senderRegistrationExists(ctx context.Context, db qrm.Queryable, deviceID string) (bool, error) {
	var row model.IdentitySenderRegistrations
	err := table.IdentitySenderRegistrations.
		SELECT(table.IdentitySenderRegistrations.ID).
		FROM(table.IdentitySenderRegistrations).
		WHERE(table.IdentitySenderRegistrations.ID.EQ(postgres.String(deviceID))).
		QueryContext(ctx, db, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// lockReceiverGrants locks the grants of one Receiver that may still be
// redeemed, so the closure that follows cannot leave a grant that would hand out
// a credential for a registration it just closed.
func lockReceiverGrants(ctx context.Context, tx *sql.Tx, receiverID string, now time.Time) ([]lockedReservation, error) {
	rows, err := tx.QueryContext(ctx, receiverGrantsSQL, receiverID, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var grants []lockedReservation
	for rows.Next() {
		var grant lockedReservation
		// A recovery session holds no reservation, so it reports no Pairing ID
		// and closing it releases nothing.
		var pairingID *string
		if err := rows.Scan(&grant.sessionID, &pairingID); err != nil {
			return nil, err
		}
		if pairingID != nil {
			grant.pairingID = *pairingID
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// revokeReceiverRegistration closes one owned Receiver registration inside the
// caller's transaction and reports the sessions whose grants closed with it, so
// their participants can be told once the commit is durable. Everything the
// Receiver still authorizes goes: its credential generation, the grants that
// could still issue one and every Pairing that forwards to it.
func revokeReceiverRegistration(ctx context.Context, tx *sql.Tx, receiverID, ownerAccount string, now time.Time) ([]string, error) {
	// The Receiver's generation boundary comes first and is the same one
	// redemption, recovery and the final 101 commit take: a revocation either
	// commits before an issuance or lands after it, so a credential issued by a
	// redemption that won is revoked as well, and one a revocation won against
	// finds its grant closed.
	if err := lockReceiver(ctx, tx, receiverID); err != nil {
		return nil, err
	}
	// The credential rows are locked before the registration row, the order
	// admission and the final commit take them in, so a possession arriving at
	// the same moment waits here instead of deadlocking with this closure.
	if _, err := tx.ExecContext(ctx, revokeReceiverCredentialsSQL, receiverID, now); err != nil {
		return nil, err
	}
	account, state, err := lockRegistration(ctx, tx, receiverID)
	if err != nil {
		return nil, err
	}
	if account != ownerAccount {
		// Another Account's Receiver is as invisible as one that does not exist.
		return nil, ErrReceiverNotOwned
	}
	if state == string(RegistrationRevoked) {
		return nil, ErrReceiverRevoked
	}
	if _, err := tx.ExecContext(ctx, closeReceiverRegistrationSQL, receiverID, now); err != nil {
		return nil, err
	}
	grants, err := lockReceiverGrants(ctx, tx, receiverID, now)
	if err != nil {
		return nil, err
	}
	canceled, err := cancelReservations(ctx, tx, grants, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, removeReceiverAssociationsSQL, receiverID, now); err != nil {
		return nil, err
	}
	return canceled, nil
}
