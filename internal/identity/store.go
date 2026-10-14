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
	var registration model.IdentitySenderRegistrations
	err := table.IdentitySenderRegistrations.
		SELECT(table.IdentitySenderRegistrations.ID,
			table.IdentitySenderRegistrations.AccountID,
			table.IdentitySenderRegistrations.State).
		FROM(table.IdentitySenderRegistrations).
		WHERE(table.IdentitySenderRegistrations.ID.EQ(postgres.String(senderID))).
		FOR(postgres.UPDATE()).
		QueryContext(ctx, tx, &registration)
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

// revokeSenderCredentials revokes every still-usable credential of the Sender,
// which is its current family. A delayed operation carrying an older family
// therefore cannot authorize, restore or revoke the freshly issued one.
func revokeSenderCredentials(ctx context.Context, tx *sql.Tx, senderID string, revokedAt time.Time) error {
	_, err := table.IdentityCredentials.
		UPDATE(table.IdentityCredentials.RevokedAt).
		SET(table.IdentityCredentials.RevokedAt.SET(postgres.TimestampzT(revokedAt))).
		WHERE(table.IdentityCredentials.SenderID.EQ(postgres.String(senderID)).
			AND(table.IdentityCredentials.RevokedAt.IS_NULL())).
		ExecContext(ctx, tx)
	return err
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

// findAccount reads the Account one verified WeChat identity belongs to.
func findAccount(ctx context.Context, db qrm.Queryable, verified WeChatIdentity) (string, error) {
	var account model.IdentityAccounts
	err := table.IdentityAccounts.
		SELECT(table.IdentityAccounts.ID).
		FROM(table.IdentityAccounts).
		WHERE(table.IdentityAccounts.WechatAppID.EQ(postgres.String(verified.AppID)).
			AND(table.IdentityAccounts.WechatOpenID.EQ(postgres.String(verified.OpenID)))).
		QueryContext(ctx, db, &account)
	if errors.Is(err, qrm.ErrNoRows) {
		return "", ErrSenderNotFound
	}
	return account.ID, err
}

// findCredential reads the credential record for a digest. The digest is the
// only credential value the database holds.
func findCredential(ctx context.Context, db qrm.Queryable, tokenDigest string) (model.IdentityCredentials, error) {
	var credential model.IdentityCredentials
	err := table.IdentityCredentials.
		SELECT(table.IdentityCredentials.AllColumns).
		FROM(table.IdentityCredentials).
		WHERE(table.IdentityCredentials.TokenDigest.EQ(postgres.String(tokenDigest))).
		QueryContext(ctx, db, &credential)
	if errors.Is(err, qrm.ErrNoRows) {
		return model.IdentityCredentials{}, ErrCredentialInvalid
	}
	return credential, err
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
