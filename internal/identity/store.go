package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/table"
	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"
)

// The account upsert is parameterized SQL because no Jet builder expresses a
// conflict update that keeps a recorded UnionID when this verification has
// none. It returns the existing Account ID when two first logins race.
const upsertAccountSQL = `
INSERT INTO pin_glide.identity_accounts AS account
    (id, wechat_app_id, wechat_open_id, wechat_union_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (wechat_app_id, wechat_open_id) DO UPDATE
SET wechat_union_id = COALESCE(EXCLUDED.wechat_union_id, account.wechat_union_id)
RETURNING id`

// upsertAccount maps the verified identity to its Account, creating the Account
// on first login. The AppID is part of the key, so the same OpenID under
// another configured application is a different Account.
func upsertAccount(ctx context.Context, tx *sql.Tx, verified WeChatIdentity) (string, error) {
	var accountID string
	err := tx.QueryRowContext(ctx, upsertAccountSQL,
		newValue(), verified.AppID, verified.OpenID, nullable(verified.UnionID)).Scan(&accountID)
	return accountID, err
}

// insertSender registers one new installation. The service assigns the ID; the
// caller's device is identified by it afterwards.
func insertSender(ctx context.Context, tx *sql.Tx, senderID, accountID string) error {
	_, err := table.IdentitySenderRegistrations.
		INSERT(table.IdentitySenderRegistrations.ID, table.IdentitySenderRegistrations.AccountID).
		VALUES(senderID, accountID).
		ExecContext(ctx, tx)
	return err
}

// restoreSender locks the supplied registration for the rest of the
// transaction and requires it to belong to the verified Account and to be
// active. A foreign or unknown ID reports the same outcome as a missing one, so
// the response cannot confirm another Account's registrations.
func restoreSender(ctx context.Context, tx *sql.Tx, senderID, accountID string) error {
	registration, err := lockSender(ctx, tx, senderID)
	if errors.Is(err, qrm.ErrNoRows) {
		return ErrSenderNotFound
	}
	if err != nil {
		return err
	}
	if registration.AccountID != accountID {
		return ErrSenderNotFound
	}
	if registration.State == model.IdentitySenderRegistrationState_Revoked {
		return ErrSenderRevoked
	}
	return nil
}

// lockSender locks one Sender registration for the rest of the transaction and
// returns it. Every path that reads or mutates that Sender's credential family
// — login, renewal and revocation — takes this lock first, so their outcomes
// cannot interleave.
func lockSender(ctx context.Context, tx *sql.Tx, senderID string) (model.IdentitySenderRegistrations, error) {
	var registration model.IdentitySenderRegistrations
	err := table.IdentitySenderRegistrations.
		SELECT(table.IdentitySenderRegistrations.AllColumns).
		FROM(table.IdentitySenderRegistrations).
		WHERE(table.IdentitySenderRegistrations.ID.EQ(postgres.String(senderID))).
		FOR(postgres.UPDATE()).
		QueryContext(ctx, tx, &registration)
	return registration, err
}

// revokeCredentials revokes every still-usable credential that matches
// predicate. Revoked rows stay for their retention window, so replay detection
// can still name the family a spent token belonged to.
func revokeCredentials(ctx context.Context, tx *sql.Tx, predicate postgres.BoolExpression, revokedAt time.Time) error {
	_, err := table.IdentityCredentials.
		UPDATE(table.IdentityCredentials.RevokedAt).
		SET(table.IdentityCredentials.RevokedAt.SET(postgres.TimestampzT(revokedAt))).
		WHERE(predicate.AND(table.IdentityCredentials.RevokedAt.IS_NULL())).
		ExecContext(ctx, tx)
	return err
}

// revokeFamilyCredentials revokes one family and nothing outside it.
func revokeFamilyCredentials(ctx context.Context, tx *sql.Tx, familyID string, revokedAt time.Time) error {
	return revokeCredentials(ctx, tx, table.IdentityCredentials.FamilyID.EQ(postgres.String(familyID)), revokedAt)
}

// revokeSenderCredentials revokes every still-usable credential of the Sender,
// which is its current family. A delayed operation carrying an older family
// therefore cannot authorize, restore or revoke the freshly issued one.
func revokeSenderCredentials(ctx context.Context, tx *sql.Tx, senderID string, revokedAt time.Time) error {
	return revokeCredentials(ctx, tx, table.IdentityCredentials.SenderID.EQ(postgres.String(senderID)), revokedAt)
}

// insertCredentials persists one family as digests only. The raw tokens exist
// outside this function and leave the service only to their holder.
func insertCredentials(ctx context.Context, tx *sql.Tx, familyID string, bundle Bundle, issuedAt time.Time) error {
	statement := table.IdentityCredentials.
		INSERT(table.IdentityCredentials.SenderID,
			table.IdentityCredentials.FamilyID,
			table.IdentityCredentials.Scope,
			table.IdentityCredentials.TokenDigest,
			table.IdentityCredentials.IssuedAt,
			table.IdentityCredentials.ExpiresAt)
	for _, issued := range []struct {
		scope Scope
		token Token
	}{
		{scope: ScopeUserSession, token: bundle.UserSession},
		{scope: ScopeUploader, token: bundle.Uploader},
		{scope: ScopeRefresh, token: bundle.Refresh},
	} {
		statement = statement.VALUES(
			bundle.SenderID, familyID, string(issued.scope), digest(issued.token.Value),
			issuedAt, issued.token.ExpiresAt)
	}
	_, err := statement.ExecContext(ctx, tx)
	return err
}

// findCredential reads the credential record for a digest. The digest is the
// only credential value the database holds.
func findCredential(ctx context.Context, db qrm.Queryable, tokenDigest string) (model.IdentityCredentials, error) {
	return scanCredential(ctx, db, credentialByDigest(tokenDigest))
}

// lockCredential reads one credential record for the rest of the transaction.
// A caller holding that Sender's lock uses it to decide with state no other
// Sender path can change.
func lockCredential(ctx context.Context, tx *sql.Tx, tokenDigest string) (model.IdentityCredentials, error) {
	return scanCredential(ctx, tx, credentialByDigest(tokenDigest).FOR(postgres.UPDATE()))
}

// credentialByDigest selects one credential record by the digest a caller
// presented.
func credentialByDigest(tokenDigest string) postgres.SelectStatement {
	return table.IdentityCredentials.
		SELECT(table.IdentityCredentials.AllColumns).
		FROM(table.IdentityCredentials).
		WHERE(table.IdentityCredentials.TokenDigest.EQ(postgres.String(tokenDigest)))
}

// scanCredential reads one credential record with statement. An unknown digest
// is the same public outcome as one that authorizes nothing.
func scanCredential(ctx context.Context, db qrm.Queryable, statement postgres.SelectStatement) (model.IdentityCredentials, error) {
	var credential model.IdentityCredentials
	err := statement.QueryContext(ctx, db, &credential)
	if errors.Is(err, qrm.ErrNoRows) {
		return model.IdentityCredentials{}, ErrCredentialInvalid
	}
	return credential, err
}

// findRefreshOperation reads the recorded operation of one Sender. found=false
// means no attempt of that operation ever committed.
func findRefreshOperation(ctx context.Context, tx *sql.Tx, senderID, operationID string) (model.IdentityRefreshOperations, bool, error) {
	var operation model.IdentityRefreshOperations
	err := table.IdentityRefreshOperations.
		SELECT(table.IdentityRefreshOperations.AllColumns).
		FROM(table.IdentityRefreshOperations).
		WHERE(table.IdentityRefreshOperations.SenderID.EQ(postgres.String(senderID)).
			AND(table.IdentityRefreshOperations.OperationID.EQ(postgres.String(operationID)))).
		QueryContext(ctx, tx, &operation)
	if errors.Is(err, qrm.ErrNoRows) {
		return model.IdentityRefreshOperations{}, false, nil
	}
	if err != nil {
		return model.IdentityRefreshOperations{}, false, err
	}
	return operation, true, nil
}

// tokenConsumed reports whether any operation already consumed this renewal
// token as its parent, which makes it a spent token rather than the family's
// current one.
func tokenConsumed(ctx context.Context, tx *sql.Tx, parentDigest string) (bool, error) {
	var operation model.IdentityRefreshOperations
	err := table.IdentityRefreshOperations.
		SELECT(table.IdentityRefreshOperations.Attempt).
		FROM(table.IdentityRefreshOperations).
		WHERE(table.IdentityRefreshOperations.ParentDigest.EQ(postgres.String(parentDigest))).
		QueryContext(ctx, tx, &operation)
	if errors.Is(err, qrm.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// The operation record is one row per operation holding its highest committed
// attempt. No Jet builder expresses the conflict update, and the row is written
// under the Sender's lock, so a recorded operation is updated rather than
// duplicated.
const recordRefreshAttemptSQL = `
INSERT INTO pin_glide.identity_refresh_operations AS operation
    (sender_id, operation_id, attempt, parent_digest, expires_at, committed_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (sender_id, operation_id) DO UPDATE
SET attempt = EXCLUDED.attempt, committed_at = EXCLUDED.committed_at`

// recordRefreshAttempt stores the highest committed attempt of one renewal
// operation with the token it consumed and that token's original expiry, so the
// ordering guard survives exactly as long as the token can be presented.
func recordRefreshAttempt(ctx context.Context, tx *sql.Tx, senderID, operationID string, attempt int, parent model.IdentityCredentials, committedAt time.Time) error {
	_, err := tx.ExecContext(ctx, recordRefreshAttemptSQL,
		senderID, operationID, attempt, parent.TokenDigest, parent.ExpiresAt, committedAt)
	return err
}

// digest returns the persisted form of a credential value.
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// nullable keeps an absent optional provider claim absent in PostgreSQL.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
