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

// Every credential issuance, possession and later revocation for one Receiver
// serializes on this transaction-scoped advisory lock, so a delayed claim can
// never replace a newer generation's working access.
const lockReceiverSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`

// nextReceiverGenerationSQL reports the generation a new credential becomes.
// Rows of superseded generations are kept but never counted, so revocation
// cannot hand a stale generation the current number again.
const nextReceiverGenerationSQL = `
SELECT COALESCE(max(generation), 0) + 1
FROM pin_glide.identity_receiver_credentials
WHERE receiver_id = $1`

// revokeReceiverCredentialsSQL supersedes every credential of a Receiver that
// still authorizes. The replacement row is written in the same transaction,
// so a Receiver never loses access without gaining its next generation.
const revokeReceiverCredentialsSQL = `
UPDATE pin_glide.identity_receiver_credentials
SET revoked_at = $2
WHERE receiver_id = $1 AND revoked_at IS NULL`

// notOlderIssuanceSQL reports whether a session of this Receiver that is not
// older than this one already issued a credential, which is what makes this
// session's own grant stale: the generation that already works is the one to
// keep, and a delayed first issuance never revokes it. Creation time orders the
// sessions of one Receiver, and two sessions created in the same instant
// resolve to the one that issued first, so no later redemption replaces a
// credential an issuance already handed out.
const notOlderIssuanceSQL = `
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
// a grant of their own. It is the other half of the ordering
// notOlderIssuanceSQL decides, so a grant that can no longer be redeemed
// reports its own outcome instead of leaving its holder to discover it by
// redemption. Only sessions that hold a grant are closed, and the Pairing they
// belong to is already active, so no unfinished pairing work and no association
// ends here.
const replacedGrantSQL = `
UPDATE pin_glide.pairing_sessions
SET state = 'superseded', closed_at = $4
WHERE receiver_id = $1
  AND id <> $2
  AND created_at <= $3
  AND state IN ('credential_ready', 'credential_issued')
RETURNING id`

// lockReceiver serializes every generation decision of one Receiver.
func lockReceiver(ctx context.Context, tx *sql.Tx, receiverID string) error {
	_, err := tx.ExecContext(ctx, lockReceiverSQL, receiverID)
	return err
}

// issueReceiverCredential supersedes the Receiver's current credential and
// issues the next generation inside the caller's transaction. Only the digest
// of the new credential is persisted; the raw value leaves through this result.
func issueReceiverCredential(ctx context.Context, tx *sql.Tx, receiverID string, issuedAt time.Time) (string, int64, error) {
	if err := lockReceiver(ctx, tx, receiverID); err != nil {
		return "", 0, err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, nextReceiverGenerationSQL, receiverID).Scan(&generation); err != nil {
		return "", 0, err
	}
	token := newValue()
	if _, err := tx.ExecContext(ctx, revokeReceiverCredentialsSQL, receiverID, issuedAt); err != nil {
		return "", 0, err
	}
	_, err := table.IdentityReceiverCredentials.
		INSERT(
			table.IdentityReceiverCredentials.ReceiverID,
			table.IdentityReceiverCredentials.Generation,
			table.IdentityReceiverCredentials.TokenDigest,
			table.IdentityReceiverCredentials.IssuedAt,
		).
		VALUES(receiverID, generation, digest(token), issuedAt).
		ExecContext(ctx, tx)
	if err != nil {
		return "", 0, err
	}
	return token, generation, nil
}

// recordIssuance stores the one operation a session's credential belongs to,
// so a later attempt can be ordered against it.
func recordIssuance(ctx context.Context, tx *sql.Tx, sessionID, operationID string, attempt int, generation int64) error {
	_, err := table.PairingSessions.
		UPDATE(
			table.PairingSessions.State,
			table.PairingSessions.IssuanceOperationID,
			table.PairingSessions.IssuanceAttempt,
			table.PairingSessions.IssuedGeneration,
		).
		SET(
			enum.PairingSessionState.CredentialIssued,
			postgres.String(operationID),
			postgres.Int16(int16(attempt)),
			postgres.Int64(generation),
		).
		WHERE(table.PairingSessions.ID.EQ(postgres.String(sessionID))).
		ExecContext(ctx, tx)
	return err
}

// notOlderIssuance reports whether a session of this Receiver that is not older
// than this one already issued the credential that works.
func notOlderIssuance(ctx context.Context, tx *sql.Tx, session Session) (bool, error) {
	var notOlder bool
	err := tx.QueryRowContext(ctx, notOlderIssuanceSQL,
		session.ReceiverID, session.ID, session.CreatedAt).Scan(&notOlder)
	return notOlder, err
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

// receiverCredentialByDigest reads the credential row one presented value
// belongs to. An unknown digest reports the same outcome as one that
// authorizes nothing.
func receiverCredentialByDigest(ctx context.Context, db qrm.Queryable, tokenDigest string) (model.IdentityReceiverCredentials, error) {
	return scanReceiverCredential(ctx, db, table.IdentityReceiverCredentials.
		SELECT(table.IdentityReceiverCredentials.AllColumns).
		FROM(table.IdentityReceiverCredentials).
		WHERE(table.IdentityReceiverCredentials.TokenDigest.EQ(postgres.String(tokenDigest))))
}

// lockCurrentReceiverCredential reads the Receiver's current generation for
// the rest of the transaction. A superseded generation is not returned, so a
// stale presentation cannot replace the working credential.
func lockCurrentReceiverCredential(ctx context.Context, tx *sql.Tx, receiverID string) (model.IdentityReceiverCredentials, error) {
	return scanReceiverCredential(ctx, tx, table.IdentityReceiverCredentials.
		SELECT(table.IdentityReceiverCredentials.AllColumns).
		FROM(table.IdentityReceiverCredentials).
		WHERE(table.IdentityReceiverCredentials.ReceiverID.EQ(postgres.String(receiverID)).
			AND(table.IdentityReceiverCredentials.RevokedAt.IS_NULL())).
		ORDER_BY(table.IdentityReceiverCredentials.Generation.DESC()).
		LIMIT(1).
		FOR(postgres.UPDATE()))
}

// scanReceiverCredential reads one Receiver credential row. A missing row is
// an invalid credential rather than a missing resource.
func scanReceiverCredential(ctx context.Context, db qrm.Queryable, statement postgres.SelectStatement) (model.IdentityReceiverCredentials, error) {
	var credential model.IdentityReceiverCredentials
	err := statement.QueryContext(ctx, db, &credential)
	if errors.Is(err, qrm.ErrNoRows) {
		return model.IdentityReceiverCredentials{}, ErrCredentialInvalid
	}
	return credential, err
}

// outstandingIssuance reads the session whose grant the presented generation
// still owes possession for. A Receiver credential that already proved
// possession has no outstanding session, which is not an error.
func outstandingIssuance(ctx context.Context, tx *sql.Tx, receiverID string, generation int64) (Session, error) {
	var row model.PairingSessions
	err := table.PairingSessions.
		SELECT(table.PairingSessions.AllColumns).
		FROM(table.PairingSessions).
		WHERE(table.PairingSessions.ReceiverID.EQ(postgres.String(receiverID)).
			AND(table.PairingSessions.IssuedGeneration.EQ(postgres.Int64(generation))).
			AND(table.PairingSessions.State.EQ(enum.PairingSessionState.CredentialIssued))).
		FOR(postgres.UPDATE()).
		QueryContext(ctx, tx, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return sessionOf(row), nil
}

// consumeGrant closes the issuance grant a valid Receiver credential proved
// possession of. An already consumed grant is left untouched.
func consumeGrant(ctx context.Context, tx *sql.Tx, sessionID string, consumedAt time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE pin_glide.pairing_sessions
SET state = 'completed', closed_at = COALESCE(closed_at, $2)
WHERE id = $1 AND state = 'credential_issued'`, sessionID, consumedAt)
	return err
}
