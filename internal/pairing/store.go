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

// setSessionState moves one session to its next lifecycle state.
func setSessionState(ctx context.Context, tx *sql.Tx, sessionID string, state SessionState) error {
	_, err := tx.ExecContext(ctx, `
UPDATE pin_glide.pairing_sessions
SET state = $2::pin_glide.pairing_session_state
WHERE id = $1`, sessionID, string(state))
	return err
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

// closeSession releases the reservation a session still holds and records its
// terminal state. Releasing deletes only the pending association the session
// reserved, so the working association a canceled or superseded replacement
// expected to replace keeps forwarding.
func closeSession(ctx context.Context, tx *sql.Tx, session Session, state SessionState, now time.Time) error {
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
		SET(postgres.NewEnumValue(string(state)), postgres.TimestampzT(now)).
		WHERE(table.PairingSessions.ID.EQ(postgres.String(session.ID))).
		ExecContext(ctx, tx)
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
