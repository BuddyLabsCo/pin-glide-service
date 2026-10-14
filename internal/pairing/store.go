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
// active association or a live pending reservation, which includes one both
// clients have confirmed but not yet activated. A re-pairing of an existing
// Receiver counts once, so it can never consume a second slot, and a
// reservation whose session lapsed releases its slot without any cleanup work.
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

// insertPendingPairing reserves one Pairing ID for a session that will
// establish or replace an association. The row holds binding capacity from
// here until both clients confirm or the reservation is released.
func insertPendingPairing(ctx context.Context, tx *sql.Tx, pairingID, senderID, receiverID string, createdAt time.Time) error {
	_, err := table.PairingPairings.
		INSERT(
			table.PairingPairings.ID,
			table.PairingPairings.SenderID,
			table.PairingPairings.ReceiverID,
			table.PairingPairings.State,
			table.PairingPairings.CreatedAt,
		).
		VALUES(pairingID, senderID, receiverID, string(AssociationPending), createdAt).
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

// currentPairing returns the Sender's live association with one Receiver,
// whether it is already active or still a pending reservation. Its owner
// reuses that Pairing ID instead of reserving a second row, so a replacement
// of an existing Receiver can never consume another capacity slot.
func currentPairing(ctx context.Context, tx *sql.Tx, senderID, receiverID string) (string, error) {
	var row model.PairingPairings
	err := table.PairingPairings.
		SELECT(table.PairingPairings.ID).
		FROM(table.PairingPairings).
		WHERE(table.PairingPairings.SenderID.EQ(postgres.String(senderID)).
			AND(table.PairingPairings.ReceiverID.EQ(postgres.String(receiverID))).
			AND(table.PairingPairings.State.IN(enum.PairingState.Pending, enum.PairingState.Active))).
		QueryContext(ctx, tx, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return "", nil
	}
	return row.ID, err
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

// setAlias stores the owning Sender's own label for a pairing. The Sender must
// already own the row, so one Sender cannot rename another's association.
func setAlias(ctx context.Context, tx *sql.Tx, pairingID, alias string) error {
	_, err := table.PairingPairings.
		UPDATE(table.PairingPairings.Alias_).
		SET(postgres.String(alias)).
		WHERE(table.PairingPairings.ID.EQ(postgres.String(pairingID))).
		ExecContext(ctx, tx)
	return err
}

// cancelSession closes a session that may still continue and releases the
// reservation it held. Deleting the reservation row is what frees the slot,
// which is why it happens before the session records its closure.
func cancelSession(ctx context.Context, tx *sql.Tx, session Session, now time.Time) error {
	if session.PendingPairingID != "" {
		if _, err := table.PairingPairings.
			DELETE().
			WHERE(table.PairingPairings.ID.EQ(postgres.String(session.PendingPairingID)).
				AND(table.PairingPairings.State.EQ(enum.PairingState.Pending))).
			ExecContext(ctx, tx); err != nil {
			return err
		}
	}
	_, err := table.PairingSessions.
		UPDATE(table.PairingSessions.State, table.PairingSessions.ClosedAt).
		SET(enum.PairingSessionState.Canceled, postgres.TimestampzT(now)).
		WHERE(table.PairingSessions.ID.EQ(postgres.String(session.ID))).
		ExecContext(ctx, tx)
	return err
}

// sessionByID reads one session as it is stored.
func sessionByID(ctx context.Context, db qrm.Queryable, sessionID string) (Session, error) {
	return readSession(ctx, db, table.PairingSessions.ID.EQ(postgres.String(sessionID)))
}

// sessionByTokenDigest reads the session one restricted credential belongs to.
func sessionByTokenDigest(ctx context.Context, db qrm.Queryable, tokenDigest string) (Session, error) {
	return readSession(ctx, db, table.PairingSessions.TokenDigest.EQ(postgres.String(tokenDigest)))
}

// readSession reads one session row behind where and maps it to the domain
// type. A caller that cannot tell apart "unknown" and "not visible to me"
// must map the missing row to its own outcome.
func readSession(ctx context.Context, db qrm.Queryable, where postgres.BoolExpression) (Session, error) {
	var row model.PairingSessions
	err := table.PairingSessions.
		SELECT(table.PairingSessions.AllColumns).
		FROM(table.PairingSessions).
		WHERE(where).
		QueryContext(ctx, db, &row)
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
