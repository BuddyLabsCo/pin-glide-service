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

// The capacity decision and the reservation must not interleave with another
// authorization for the same Sender, so every claim serializes on this
// transaction-scoped advisory lock before it counts and inserts. The lock is
// keyed by the Sender ID and released when the transaction ends.
const lockSenderSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`

// capacitySQL counts the distinct Receivers one Sender already holds with an
// active association or a live pending reservation, which includes a
// replacement both clients have confirmed but not yet activated. $2 is the
// Receiver the caller is deciding for: it is counted here exactly like every
// other one, so a Receiver the Sender already holds never consumes a second
// slot however many reservations it holds, and a reservation whose session
// lapsed releases its slot without any cleanup work.
const capacitySQL = `
SELECT count(DISTINCT pairings.receiver_id)
FROM pin_glide.pairing_pairings AS pairings
WHERE pairings.sender_id = $1
  AND pairings.receiver_id <> $2
  AND (
    pairings.state = 'active'
    OR (
      pairings.state = 'pending'
      AND EXISTS (
        SELECT 1
        FROM pin_glide.pairing_sessions AS sessions
        WHERE sessions.pending_pairing_id = pairings.id
          AND sessions.state IN (
            'awaiting_authorization', 'awaiting_confirmation', 'credential_ready'
          )
          AND sessions.expires_at > now()
      )
    )
  )`

// lockSender serializes every capacity decision of one Sender.
func lockSender(ctx context.Context, tx *sql.Tx, senderID string) error {
	_, err := tx.ExecContext(ctx, lockSenderSQL, senderID)
	return err
}

// countOtherReceivers returns how many distinct Receivers the Sender holds
// besides the one the caller is deciding for.
func countOtherReceivers(ctx context.Context, tx *sql.Tx, senderID, receiverID string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, capacitySQL, senderID, receiverID).Scan(&count)
	return count, err
}

// currentPairing returns the Sender's current association with one Receiver
// and the Sender's own alias for it. A reserved replacement is not the current
// association: the working one stays current until both clients confirm the
// replacement.
func currentPairing(ctx context.Context, tx *sql.Tx, senderID, receiverID string) (model.PairingPairings, error) {
	var row model.PairingPairings
	err := table.PairingPairings.
		SELECT(table.PairingPairings.ID, table.PairingPairings.Alias_).
		FROM(table.PairingPairings).
		WHERE(table.PairingPairings.SenderID.EQ(postgres.String(senderID)).
			AND(table.PairingPairings.ReceiverID.EQ(postgres.String(receiverID))).
			AND(table.PairingPairings.State.EQ(enum.PairingState.Active))).
		QueryContext(ctx, tx, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return model.PairingPairings{}, nil
	}
	return row, err
}

// reservation is the association one Pairing Session reserves for its claiming
// Sender: the Pairing ID both clients store their provisional key under, the
// association it replaces when it activates, and the Sender's own alias.
type reservation struct {
	pairingID         string
	senderID          string
	receiverID        string
	replacesPairingID string
	alias             string
	createdAt         time.Time
}

// insertPendingPairing reserves one Pairing ID for a session that will
// establish or replace an association. The row holds binding capacity from
// here until it activates, or until a cancellation, expiry or superseded
// replacement releases it. A reservation that replaces a working association
// stands next to it, so the current association keeps forwarding until the
// replacement is committed.
func insertPendingPairing(ctx context.Context, tx *sql.Tx, reserved reservation) error {
	replaces := any(postgres.NULL)
	if reserved.replacesPairingID != "" {
		replaces = postgres.String(reserved.replacesPairingID)
	}
	_, err := table.PairingPairings.
		INSERT(
			table.PairingPairings.ID,
			table.PairingPairings.SenderID,
			table.PairingPairings.ReceiverID,
			table.PairingPairings.ReplacesPairingID,
			table.PairingPairings.Alias_,
			table.PairingPairings.State,
			table.PairingPairings.CreatedAt,
		).
		VALUES(reserved.pairingID, reserved.senderID, reserved.receiverID, replaces,
			reserved.alias, string(AssociationPending), reserved.createdAt).
		ExecContext(ctx, tx)
	return err
}

// retirePairingSQL retires one association a replacement expected to find
// active. Matching the whole identity and the active state is what makes a
// concurrent replacement visible: an association that already changed leaves
// this replacement with no effect instead of two associations claiming to be
// current.
const retirePairingSQL = `
UPDATE pin_glide.pairing_pairings
SET state = 'removed', removed_at = $2
WHERE id = $1
  AND sender_id = $3
  AND receiver_id = $4
  AND state = 'active'`

// activatePairingSQL activates one reservation. A row that is already active
// stays active with its original activation time.
const activatePairingSQL = `
UPDATE pin_glide.pairing_pairings
SET state = 'active', activated_at = COALESCE(activated_at, $2)
WHERE id = $1 AND state = 'pending'`

// activatePairing commits the reservation both clients confirmed, so it
// becomes the Sender's current association with the Receiver. A reservation
// that replaces an association retires exactly that association in the same
// transaction and only while it is still the current one; a first association
// only activates while nothing else became current first. Everything else is a
// stale replacement, which preserves the association that won.
func activatePairing(ctx context.Context, tx *sql.Tx, session Session, activatedAt time.Time) error {
	// The Sender's advisory lock serializes this commit with every claim and
	// every other activation of the same Sender.
	if err := lockSender(ctx, tx, session.AuthorizedSenderID); err != nil {
		return err
	}
	reserved, err := lockedPairing(ctx, tx, session.PendingPairingID)
	if err != nil {
		return err
	}
	if reserved.ReplacesPairingID != nil {
		result, err := tx.ExecContext(ctx, retirePairingSQL, *reserved.ReplacesPairingID, activatedAt,
			reserved.SenderID, reserved.ReceiverID)
		if err != nil {
			return err
		}
		retired, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if retired != 1 {
			return ErrOperationSuperseded
		}
	} else {
		current, err := currentPairing(ctx, tx, reserved.SenderID, reserved.ReceiverID)
		if err != nil {
			return err
		}
		if current.ID != "" {
			return ErrOperationSuperseded
		}
	}

	result, err := tx.ExecContext(ctx, activatePairingSQL, reserved.ID, activatedAt)
	if err != nil {
		return err
	}
	activated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if activated != 1 {
		return errors.New("the session's reserved Pairing disappeared before activation")
	}
	return nil
}

// lockedPairing reads one association for the rest of the transaction, so the
// replacement it reserves cannot interleave with another activation.
func lockedPairing(ctx context.Context, tx *sql.Tx, pairingID string) (model.PairingPairings, error) {
	var row model.PairingPairings
	err := table.PairingPairings.
		SELECT(table.PairingPairings.AllColumns).
		FROM(table.PairingPairings).
		WHERE(table.PairingPairings.ID.EQ(postgres.String(pairingID))).
		FOR(postgres.UPDATE()).
		QueryContext(ctx, tx, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return model.PairingPairings{}, errors.New("the session's reserved Pairing disappeared")
	}
	return row, err
}
