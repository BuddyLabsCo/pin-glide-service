package pairing

import (
	"context"
	"database/sql"
	"time"
)

// receiverDeviceRevocation permanently closes one Receiver registration the
// Account owner approved, together with everything that authorized it: the
// credential generation that could still subscribe, the issuance and recovery
// grants that could still hand out a replacement, the delivery connection it
// holds and every Pairing that forwards to it, all in one commit.
//
// Nothing this Receiver ever held survives the closure, and no other Receiver is
// reached by it. The live transport learns the outcome only once it is durable.
func (s *Service) receiverDeviceRevocation(ctx context.Context, receiverID, ownerAccount string, now time.Time) error {
	canceled := []string{}
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		canceled, err = revokeReceiverRegistration(ctx, tx, receiverID, ownerAccount, now)
		return err
	})
	if err != nil {
		return err
	}

	// The committed closure is what the Receiver's connection follows: it is
	// told with the adopted control and ended, and every participant of the
	// grants that closed with it reads its own outcome. Notification comes last,
	// so a failure to report a change never reports one that did not commit.
	if s.deliveryNotifier != nil {
		s.deliveryNotifier.DeviceRevoked(receiverID)
	}
	s.sessionsChanged(ctx, canceled, now)
	return nil
}
