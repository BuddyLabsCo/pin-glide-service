package identity

import (
	"context"
	"errors"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/enum"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/table"
	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"
)

func accountForSender(ctx context.Context, db qrm.Queryable, senderID string) (AccountProfile, error) {
	a, s := table.IdentityAccounts, table.IdentitySenderRegistrations
	return scanAccountProfile(ctx, db, a.SELECT(a.ID, a.Nickname, a.AvatarURL).
		FROM(a.INNER_JOIN(s, s.AccountID.EQ(a.ID))).
		WHERE(s.ID.EQ(postgres.String(senderID))))
}

func accountForReceiverCredential(ctx context.Context, db qrm.Queryable, tokenDigest string) (AccountProfile, error) {
	a, r, c := table.IdentityAccounts, table.IdentityReceiverRegistrations, table.IdentityReceiverCredentials
	newer := c.AS("newer")
	return scanAccountProfile(ctx, db, a.SELECT(a.ID, a.Nickname, a.AvatarURL).
		FROM(a.INNER_JOIN(r, r.AccountID.EQ(a.ID)).INNER_JOIN(c, c.ReceiverID.EQ(r.ID))).
		WHERE(c.TokenDigest.EQ(postgres.String(tokenDigest)).
			AND(c.RevokedAt.IS_NULL()).
			AND(r.State.EQ(enum.IdentityReceiverRegistrationState.Active)).
			AND(postgres.NOT(postgres.EXISTS(postgres.SELECT(newer.ID).FROM(newer).
				WHERE(newer.ReceiverID.EQ(c.ReceiverID).AND(newer.Generation.GT(c.Generation))))))))
}

func scanAccountProfile(ctx context.Context, db qrm.Queryable, statement postgres.SelectStatement) (AccountProfile, error) {
	var row model.IdentityAccounts
	err := statement.QueryContext(ctx, db, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return AccountProfile{}, ErrCredentialInvalid
	}
	return AccountProfile{AccountID: row.ID, Nickname: row.Nickname, AvatarURL: row.AvatarURL}, err
}
