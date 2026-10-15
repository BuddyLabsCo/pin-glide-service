package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/table"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"
)

// executor is the database surface a Delivery write needs: one row-returning
// statement, one update and the queryable surface the reads use, which *sql.DB
// and *sql.Tx both provide.
type executor interface {
	qrm.Queryable
	qrm.Executable
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// key identifies one Delivery: the Sender that uploaded it, the Message ID the
// Sender generated once for one SMS operation, and the Receiver this entry
// selected. This identity is what the service deduplicates on while its row
// exists.
type key struct {
	senderID   string
	messageID  string
	receiverID string
}

// claimDelivery records the processing identity of one Delivery and reports
// whether this caller claimed it. The insert is the atomic claim the adopted
// dedup contract requires: a concurrent duplicate of the same identity loses
// the unique index and learns that the identity already exists, and the stored
// row is then read to decide whether it is retained work or live work.
func claimDelivery(ctx context.Context, db executor, identity key, pairingID string, now time.Time) (bool, Delivery, error) {
	var id string
	err := db.QueryRowContext(ctx, `
INSERT INTO pin_glide.delivery_deliveries
    (id, sender_id, message_id, receiver_id, pairing_id, status, created_at)
VALUES ($1, $2, $3, $4, $5, 'processing', $6)
ON CONFLICT (sender_id, message_id, receiver_id) DO NOTHING
RETURNING id`,
		newIdentity(), identity.senderID, identity.messageID, identity.receiverID, pairingID, now).Scan(&id)
	switch {
	case err == nil:
		return true, Delivery{
			ReceiverID: identity.receiverID,
			PairingID:  pairingID,
			Status:     protocol.DeliveryProcessing,
			CreatedAt:  now,
		}, nil
	case errors.Is(err, sql.ErrNoRows):
		retained, err := readDelivery(ctx, db, identity)
		return false, retained, err
	default:
		return false, Delivery{}, unavailable(err)
	}
}

// completeDelivery stores the terminal outcome this process produced. The
// update is guarded on the processing status, so no later report can rewrite
// the Delivery history an earlier outcome already established.
func completeDelivery(ctx context.Context, db executor, identity key, status protocol.DeliveryStatus, now time.Time) error {
	result, err := db.ExecContext(ctx, `
UPDATE pin_glide.delivery_deliveries
SET status = $4::pin_glide.delivery_status, completed_at = $5
WHERE sender_id = $1 AND message_id = $2 AND receiver_id = $3
  AND status = 'processing'`,
		identity.senderID, identity.messageID, identity.receiverID, string(status), now)
	if err != nil {
		return unavailable(err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return unavailable(err)
	}
	if updated != 1 {
		// The identity already carries a terminal outcome, so this process
		// never owned the row it thinks it is completing. The caller reports an
		// uncertain result instead of an outcome it did not store.
		return ErrStatusUnavailable
	}
	return nil
}

// interruptDelivery records the adopted outcome of registered work this
// process can no longer finish, which is what a restart or a lost owner
// leaves behind, and never resumes the delivery itself. Work another writer
// already finished reports the terminal outcome it stored.
func interruptDelivery(ctx context.Context, db qrm.Queryable, identity key, now time.Time) (Delivery, error) {
	var row model.DeliveryDeliveries
	err := table.DeliveryDeliveries.
		UPDATE(table.DeliveryDeliveries.Status, table.DeliveryDeliveries.CompletedAt).
		SET(postgres.NewEnumValue(string(protocol.DeliveryInterrupted)), postgres.TimestampzT(now)).
		WHERE(table.DeliveryDeliveries.SenderID.EQ(postgres.String(identity.senderID)).
			AND(table.DeliveryDeliveries.MessageID.EQ(postgres.String(identity.messageID))).
			AND(table.DeliveryDeliveries.ReceiverID.EQ(postgres.String(identity.receiverID))).
			AND(table.DeliveryDeliveries.Status.EQ(postgres.NewEnumValue(string(protocol.DeliveryProcessing))))).
		RETURNING(table.DeliveryDeliveries.AllColumns).
		QueryContext(ctx, db, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		// Another writer gave this identity its terminal outcome while this
		// interruption ran, so that outcome is what the identity now has.
		return readDelivery(ctx, db, identity)
	}
	if err != nil {
		return Delivery{}, unavailable(err)
	}
	return deliveryOf(row), nil
}

// readDelivery reads one stored Delivery by its identity.
func readDelivery(ctx context.Context, db qrm.Queryable, identity key) (Delivery, error) {
	var row model.DeliveryDeliveries
	err := table.DeliveryDeliveries.
		SELECT(table.DeliveryDeliveries.AllColumns).
		FROM(table.DeliveryDeliveries).
		WHERE(table.DeliveryDeliveries.SenderID.EQ(postgres.String(identity.senderID)).
			AND(table.DeliveryDeliveries.MessageID.EQ(postgres.String(identity.messageID))).
			AND(table.DeliveryDeliveries.ReceiverID.EQ(postgres.String(identity.receiverID)))).
		QueryContext(ctx, db, &row)
	if errors.Is(err, qrm.ErrNoRows) {
		return Delivery{}, ErrStatusUnavailable
	}
	if err != nil {
		return Delivery{}, unavailable(err)
	}
	return deliveryOf(row), nil
}

// deliveriesOfMessage reads one Sender's retained Deliveries of one Message. A
// Message with none is reported as absent, which is also what a Message whose
// rows scheduled cleanup already deleted reports.
func deliveriesOfMessage(ctx context.Context, db qrm.Queryable, senderID, messageID string) ([]Delivery, error) {
	var rows []model.DeliveryDeliveries
	err := table.DeliveryDeliveries.
		SELECT(table.DeliveryDeliveries.AllColumns).
		FROM(table.DeliveryDeliveries).
		WHERE(table.DeliveryDeliveries.SenderID.EQ(postgres.String(senderID)).
			AND(table.DeliveryDeliveries.MessageID.EQ(postgres.String(messageID)))).
		ORDER_BY(table.DeliveryDeliveries.CreatedAt.ASC(), table.DeliveryDeliveries.ReceiverID.ASC()).
		QueryContext(ctx, db, &rows)
	if err != nil {
		return nil, unavailable(err)
	}

	deliveries := make([]Delivery, 0, len(rows))
	for _, row := range rows {
		deliveries = append(deliveries, deliveryOf(row))
	}
	return deliveries, nil
}

// deliveryOf maps one stored row to the domain type.
func deliveryOf(row model.DeliveryDeliveries) Delivery {
	return Delivery{
		ReceiverID:  row.ReceiverID,
		PairingID:   row.PairingID,
		Status:      protocol.DeliveryStatus(row.Status),
		CreatedAt:   row.CreatedAt,
		CompletedAt: row.CompletedAt,
	}
}

// eligible decides whether one entry may forward to the Receiver it selected.
// The four ways a Receiver can be ineligible stay distinct here and share the
// one external outcome: an identifier no registration holds, a closed
// registration, a registration that belongs to another Account, and a Receiver
// that is not this Sender's own active association under the Pairing the entry
// named. The read takes the current authority of the Receiver, which is the
// decision this request freezes.
func eligible(ctx context.Context, db executor, senderID string, item Item) error {
	var receiverState string
	var receiverAccount, senderAccount *string
	var pairingState *string
	err := db.QueryRowContext(ctx, `
SELECT registrar.state, registrar.account_id, sender.account_id, pairing.state
FROM pin_glide.identity_receiver_registrations AS registrar
JOIN pin_glide.identity_sender_registrations AS sender
    ON sender.id = $2
LEFT JOIN pin_glide.pairing_pairings AS pairing
    ON pairing.receiver_id = registrar.id
    AND pairing.sender_id = $2
    AND pairing.id = $1
WHERE registrar.id = $3`,
		item.PairingID, senderID, item.ReceiverID).Scan(
		&receiverState, &receiverAccount, &senderAccount, &pairingState)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %w", ErrReceiverNotAuthorized, errReceiverUnknown)
	case err != nil:
		return unavailable(err)
	case receiverState == string(model.IdentityReceiverRegistrationState_Revoked):
		return fmt.Errorf("%w: %w", ErrReceiverNotAuthorized, errReceiverRevoked)
	// Pin Glide pairs Senders and Receivers of one Account, so a registration
	// owned by a different Account is another Account's Receiver rather than
	// one of this Sender's own unpaired ones.
	case receiverAccount != nil && senderAccount != nil && *receiverAccount != *senderAccount:
		return fmt.Errorf("%w: %w", ErrReceiverNotAuthorized, errReceiverForeign)
	case pairingState == nil || *pairingState != string(model.PairingState_Active):
		return fmt.Errorf("%w: %w", ErrReceiverNotAuthorized, errReceiverUnpaired)
	default:
		return nil
	}
}

// unavailable marks a failure that is not an explicit business outcome: the
// caller cannot obtain a reliable Delivery status, so it reports uncertainty
// rather than an outcome the service did not establish.
func unavailable(err error) error {
	if err == nil || errors.Is(err, ErrStatusUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStatusUnavailable, err)
}
