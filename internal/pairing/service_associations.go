package pairing

import (
	"context"
	"database/sql"
	"time"
)

// SetDeliveryNotifier installs the transport notifier that reports a removed
// association to the Receiver's live delivery connection. A process composed
// without one keeps working; only that notification stops.
func (s *Service) SetDeliveryNotifier(notifier DeliveryNotifier) {
	s.deliveryNotifier = notifier
}

// Associations returns one Sender's own active associations. An association
// is visible only to the Sender that owns it, and a pending reservation is not
// an association yet: it appears only as its Pairing Session.
func (s *Service) Associations(ctx context.Context, senderID string) ([]Association, error) {
	associations, err := activeAssociations(ctx, s.pool, senderID)
	if err != nil {
		return nil, storageError(err)
	}
	return associations, nil
}

// RenameAssociation replaces the owning Sender's own alias for one of its
// active associations and reports the association as it now is. A Pairing
// that is unknown, belongs to another Sender, is still pending or was already
// removed reports the same not-found outcome.
func (s *Service) RenameAssociation(ctx context.Context, senderID, pairingID, alias string) (Association, error) {
	association := Association{}
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		// The Sender's lock serializes this write with the claim and the
		// replacement that may be committing the same association, so the
		// rename either lands on the association this Sender still holds or
		// reports it as no longer there.
		if err := lockSender(ctx, tx, senderID); err != nil {
			return err
		}
		renamed, err := renameAssociation(ctx, tx, senderID, pairingID, alias)
		if err != nil {
			return err
		}
		association = renamed
		return nil
	})
	if err != nil {
		return Association{}, err
	}
	return association, nil
}

// RemoveAssociation removes one Sender's own active association. It withdraws
// only that Sender's forwarding authorization: the Receiver's credential, its
// delivery connection and every other Sender's association are untouched.
//
// The Sender's unfinished work for the same Receiver is canceled in the same
// transaction, so a destination that loses its association cannot be
// recreated by a stale key-confirmation report: the report finds its session
// canceled and its reservation released.
func (s *Service) RemoveAssociation(ctx context.Context, senderID, pairingID string, now time.Time) error {
	var removed Association
	var canceled []string
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		association, err := activeAssociation(ctx, tx, senderID, pairingID)
		if err != nil {
			return err
		}
		// The Sender's reservations are locked before the Sender's own lock,
		// which is the order a claim and a completion take them in.
		reservations, err := lockLiveReservations(ctx, tx, senderID, association.ReceiverID, now)
		if err != nil {
			return err
		}
		if err := lockSender(ctx, tx, senderID); err != nil {
			return err
		}
		removed, err = removeAssociation(ctx, tx, senderID, pairingID, now)
		if err != nil {
			return err
		}
		for _, reservation := range reservations {
			// Closing a session releases its reservation exactly as an explicit
			// cancellation does, so the two paths cannot drift apart.
			if err := closeSession(ctx, tx, Session{
				ID:               reservation.sessionID,
				PendingPairingID: reservation.pairingID,
			}, StateCanceled, now); err != nil {
				return err
			}
			canceled = append(canceled, reservation.sessionID)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// The Receiver learns that this Sender no longer forwards to it, and every
	// participant of the work this removal made obsolete learns that its
	// session closed.
	if s.deliveryNotifier != nil {
		s.deliveryNotifier.PairingRemoved(senderID, removed.ReceiverID, removed.PairingID)
	}
	for _, sessionID := range canceled {
		if session, err := sessionByID(ctx, s.pool, sessionID); err == nil {
			s.sessionChanged(session, now)
		}
	}
	return nil
}
