package pairing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
// replacement both clients have confirmed but not yet activated. A re-pairing
// of an existing Receiver counts once however many reservations it holds, so
// it can never consume a second slot, and a reservation whose session lapsed
// releases its slot without any cleanup work.
const capacitySQL = `
SELECT count(DISTINCT pairings.receiver_id)
FROM pin_glide.pairing_pairings AS pairings
WHERE pairings.sender_id = $1
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

// insertSession persists one created session. A recovery session has no
// pending Pairing ID, which the table's check constraint also enforces.
func insertSession(ctx context.Context, tx *sql.Tx, session sessionInfo) error {
	_, err := table.PairingSessions.
		INSERT(
			table.PairingSessions.ID,
			table.PairingSessions.Mode,
			table.PairingSessions.ReceiverID,
			table.PairingSessions.State,
			table.PairingSessions.TokenDigest,
			table.PairingSessions.CreatedAt,
			table.PairingSessions.ExpiresAt,
		).
		VALUES(
			session.id, string(session.mode), session.receiverID, string(session.state),
			session.tokenDigest, session.createdAt, session.expiresAt,
		).
		ExecContext(ctx, tx)
	return err
}

// confirmSender records one Sender-side key-confirmation report without
// overwriting the timestamp an earlier report of the same side already set.
func confirmSender(ctx context.Context, tx *sql.Tx, sessionID string, confirmedAt time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE pin_glide.pairing_sessions
SET sender_confirmed_at = COALESCE(sender_confirmed_at, $2)
WHERE id = $1`, sessionID, confirmedAt)
	return err
}

// confirmReceiver records one Receiver-side key-confirmation report without
// overwriting the timestamp an earlier report of the same side already set.
func confirmReceiver(ctx context.Context, tx *sql.Tx, sessionID string, confirmedAt time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE pin_glide.pairing_sessions
SET receiver_confirmed_at = COALESCE(receiver_confirmed_at, $2)
WHERE id = $1`, sessionID, confirmedAt)
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

// setSessionState moves one session to its next lifecycle state.
func setSessionState(ctx context.Context, tx *sql.Tx, sessionID string, state SessionState) error {
	_, err := tx.ExecContext(ctx, `
UPDATE pin_glide.pairing_sessions
SET state = $2::pin_glide.pairing_session_state
WHERE id = $1`, sessionID, string(state))
	return err
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

// lockSender serializes every capacity decision of one Sender.
func lockSender(ctx context.Context, tx *sql.Tx, senderID string) error {
	_, err := tx.ExecContext(ctx, lockSenderSQL, senderID)
	return err
}

// countReceivers returns how many distinct Receivers the Sender holds.
func countReceivers(ctx context.Context, tx *sql.Tx, senderID string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, capacitySQL, senderID).Scan(&count)
	return count, err
}

// claimedSender returns the Sender that claimed a session, or an empty string
// while the session is unclaimed.
func claimedSender(ctx context.Context, tx *sql.Tx, sessionID string) (string, error) {
	var row model.PairingSessions
	err := table.PairingSessions.
		SELECT(table.PairingSessions.AuthorizedSenderID).
		FROM(table.PairingSessions).
		WHERE(table.PairingSessions.ID.EQ(postgres.String(sessionID))).
		QueryContext(ctx, tx, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return "", ErrSessionNotFound
	}
	if err != nil || row.AuthorizedSenderID == nil {
		return "", err
	}
	return *row.AuthorizedSenderID, nil
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

// claimSession records the first successful claim of a session. The update is
// guarded on the unclaimed state, so a concurrent claim cannot overwrite it.
func claimSession(ctx context.Context, tx *sql.Tx, sessionID, senderID string) (bool, error) {
	result, err := table.PairingSessions.
		UPDATE(table.PairingSessions.AuthorizedSenderID, table.PairingSessions.State).
		SET(postgres.String(senderID), enum.PairingSessionState.AwaitingConfirmation).
		WHERE(table.PairingSessions.ID.EQ(postgres.String(sessionID)).
			AND(table.PairingSessions.AuthorizedSenderID.IS_NULL())).
		ExecContext(ctx, tx)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	return updated == 1, err
}

// reservePairing records the Pairing ID a session allocated.
func reservePairing(ctx context.Context, tx *sql.Tx, sessionID, pairingID string) error {
	_, err := table.PairingSessions.
		UPDATE(table.PairingSessions.PendingPairingID).
		SET(postgres.String(pairingID)).
		WHERE(table.PairingSessions.ID.EQ(postgres.String(sessionID))).
		ExecContext(ctx, tx)
	return err
}

// cancelSession closes a session that may still continue and releases the
// reservation it held, which leaves the association it replaced untouched.
func cancelSession(ctx context.Context, tx *sql.Tx, session Session, now time.Time) error {
	if err := releaseReservation(ctx, tx, session.PendingPairingID); err != nil {
		return err
	}
	return closeSession(ctx, tx, session.ID, StateCanceled, now)
}

// supersedeSession closes a session whose reservation lost its race to a
// concurrent replacement and releases that reservation only.
func supersedeSession(ctx context.Context, tx *sql.Tx, session Session, now time.Time) error {
	if err := releaseReservation(ctx, tx, session.PendingPairingID); err != nil {
		return err
	}
	return closeSession(ctx, tx, session.ID, StateSuperseded, now)
}

// releaseReservation deletes the pending association one session holds, which
// is what frees its binding capacity.
func releaseReservation(ctx context.Context, tx *sql.Tx, pairingID string) error {
	if pairingID == "" {
		return nil
	}
	_, err := table.PairingPairings.
		DELETE().
		WHERE(table.PairingPairings.ID.EQ(postgres.String(pairingID)).
			AND(table.PairingPairings.State.EQ(enum.PairingState.Pending))).
		ExecContext(ctx, tx)
	return err
}

// closeSession records the terminal state of a session that may still
// continue.
func closeSession(ctx context.Context, tx *sql.Tx, sessionID string, state SessionState, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE pin_glide.pairing_sessions
SET state = $2::pin_glide.pairing_session_state, closed_at = $3
WHERE id = $1`, sessionID, string(state), now)
	return err
}

// sessionByID reads one session as it is stored.
func sessionByID(ctx context.Context, db qrm.Queryable, sessionID string) (Session, error) {
	return readSession(ctx, db, table.PairingSessions.ID.EQ(postgres.String(sessionID)), false)
}

// lockedSessionByID reads one session for the rest of the transaction, so a
// completion that activates an association cannot interleave with a
// cancellation of the same session.
func lockedSessionByID(ctx context.Context, tx *sql.Tx, sessionID string) (Session, error) {
	return readSession(ctx, tx, table.PairingSessions.ID.EQ(postgres.String(sessionID)), true)
}

// sessionByTokenDigest reads the session one restricted credential belongs to.
func sessionByTokenDigest(ctx context.Context, db qrm.Queryable, tokenDigest string) (Session, error) {
	return readSession(ctx, db, table.PairingSessions.TokenDigest.EQ(postgres.String(tokenDigest)), false)
}

// readSession reads one session row behind where and maps it to the domain
// type. A caller that cannot tell apart "unknown" and "not visible to me"
// must map the missing row to its own outcome.
func readSession(ctx context.Context, db qrm.Queryable, where postgres.BoolExpression, lock bool) (Session, error) {
	statement := table.PairingSessions.
		SELECT(table.PairingSessions.AllColumns).
		FROM(table.PairingSessions).
		WHERE(where)
	if lock {
		statement = statement.FOR(postgres.UPDATE())
	}
	var row model.PairingSessions
	err := statement.QueryContext(ctx, db, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return sessionOf(row), nil
}

// sessionOf maps one stored session row to the domain type.
func sessionOf(row model.PairingSessions) Session {
	session := Session{
		ID:                  row.ID,
		Mode:                Mode(row.Mode),
		ReceiverID:          row.ReceiverID,
		State:               SessionState(row.State),
		TokenDigest:         row.TokenDigest,
		SenderConfirmedAt:   row.SenderConfirmedAt,
		ReceiverConfirmedAt: row.ReceiverConfirmedAt,
		CreatedAt:           row.CreatedAt,
		ExpiresAt:           row.ExpiresAt,
	}
	if row.AuthorizedSenderID != nil {
		session.AuthorizedSenderID = *row.AuthorizedSenderID
	}
	if row.PendingPairingID != nil {
		session.PendingPairingID = *row.PendingPairingID
	}
	if row.IssuanceOperationID != nil {
		session.IssuanceOperationID = *row.IssuanceOperationID
	}
	if row.IssuanceAttempt != nil {
		session.IssuanceAttempt = int(*row.IssuanceAttempt)
	}
	if row.IssuedGeneration != nil {
		session.IssuedGeneration = *row.IssuedGeneration
	}
	return session
}

// lockRegistration locks a Receiver registration for the rest of the
// transaction and returns its owning Account, which is NULL while no Sender
// has claimed it.
func lockRegistration(ctx context.Context, tx *sql.Tx, receiverID string) (account string, state string, err error) {
	var row model.IdentityReceiverRegistrations
	err = table.IdentityReceiverRegistrations.
		SELECT(table.IdentityReceiverRegistrations.AccountID,
			table.IdentityReceiverRegistrations.State).
		FROM(table.IdentityReceiverRegistrations).
		WHERE(table.IdentityReceiverRegistrations.ID.EQ(postgres.String(receiverID))).
		FOR(postgres.UPDATE()).
		QueryContext(ctx, tx, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return "", "", ErrReceiverNotOwned
	}
	if err != nil {
		return "", "", err
	}
	if row.AccountID != nil {
		account = *row.AccountID
	}
	return account, string(row.State), nil
}

// insertReceiverRegistration allocates the Receiver context a new-receiver
// session hands to a Windows installation. Ownership stays unset until a
// Sender claim fixes it.
func insertReceiverRegistration(ctx context.Context, tx *sql.Tx, receiverID string) error {
	_, err := table.IdentityReceiverRegistrations.
		INSERT(table.IdentityReceiverRegistrations.ID).
		VALUES(receiverID).
		ExecContext(ctx, tx)
	return err
}

// claimReceiverRegistration attaches an allocated Receiver to the Account of
// the Sender that claimed its session. It only fills an unset owner, so a
// concurrent claim cannot move a Receiver to another Account.
func claimReceiverRegistration(ctx context.Context, tx *sql.Tx, receiverID, accountID string) error {
	_, err := table.IdentityReceiverRegistrations.
		UPDATE(
			table.IdentityReceiverRegistrations.AccountID,
			table.IdentityReceiverRegistrations.State,
		).
		SET(postgres.String(accountID), enum.IdentityReceiverRegistrationState.Active).
		WHERE(table.IdentityReceiverRegistrations.ID.EQ(postgres.String(receiverID)).
			AND(table.IdentityReceiverRegistrations.AccountID.IS_NULL())).
		ExecContext(ctx, tx)
	return err
}

// senderAccount returns the Account a Sender registration belongs to.
func senderAccount(ctx context.Context, db qrm.Queryable, senderID string) (string, error) {
	var row model.IdentitySenderRegistrations
	err := table.IdentitySenderRegistrations.
		SELECT(table.IdentitySenderRegistrations.AccountID).
		FROM(table.IdentitySenderRegistrations).
		WHERE(table.IdentitySenderRegistrations.ID.EQ(postgres.String(senderID))).
		QueryContext(ctx, db, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return "", ErrReceiverNotOwned
	}
	return row.AccountID, err
}

// digest returns the persisted form of a restricted credential value.
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
