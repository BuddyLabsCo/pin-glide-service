package pairing

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// recoveryOutcome is what one committed recovery report changes beyond its
// session row: the approving Sender's own association it no longer holds, and
// the sessions whose reservation closed with it.
type recoveryOutcome struct {
	lostPairingID    string
	canceledSessions []string
}

// completeRecovery applies one authenticated report of a recovery session and
// reconciles the Pairings whose client keys survived. The approving Sender
// states whether it still holds the key of its own association; the Receiver
// side states the Pairing IDs it still holds, which is what keeps those
// associations and removes every other one. The session reaches
// credential_ready once both sides have reported, and a pure recovery never
// creates a Pairing or reserves binding capacity.
func (s *Service) completeRecovery(ctx context.Context, tx *sql.Tx, session Session, request CompleteRequest, now time.Time) (recoveryOutcome, error) {
	receiverSide := request.SenderID == ""
	if receiverSide {
		// The Receiver's report and the keys it kept state the same fact, so a
		// report that contradicts its own inventory is rejected rather than
		// keeping an association its key store does not have.
		if held := len(request.RetainedPairingIDs) > 0; held != (request.KeyConfirmation == KeyConfirmed) {
			return recoveryOutcome{}, ErrRetainedPairingsInvalid
		}
	} else if len(request.RetainedPairingIDs) > 0 {
		// The local key store the set reports is the Receiver's, not a Sender's.
		return recoveryOutcome{}, ErrRetainedPairingsInvalid
	}

	outcome := recoveryOutcome{}
	if receiverSide {
		if err := confirmReceiver(ctx, tx, session.ID, now); err != nil {
			return recoveryOutcome{}, err
		}
		canceled, err := reconcileRetainedPairings(ctx, tx, session.ReceiverID, request.RetainedPairingIDs, now)
		if err != nil {
			return recoveryOutcome{}, err
		}
		outcome.canceledSessions = canceled
	} else {
		if err := confirmSender(ctx, tx, session.ID, now); err != nil {
			return recoveryOutcome{}, err
		}
		if request.KeyConfirmation == KeyNotAvailable {
			lost, canceled, err := dropSenderReceiverAccess(ctx, tx, session.AuthorizedSenderID, session.ReceiverID, now)
			if err != nil {
				return recoveryOutcome{}, err
			}
			outcome.lostPairingID = lost
			outcome.canceledSessions = canceled
		}
	}

	senderReported := session.SenderConfirmedAt != nil || !receiverSide
	receiverReported := session.ReceiverConfirmedAt != nil || receiverSide
	if !senderReported || !receiverReported {
		return outcome, nil
	}
	return outcome, setSessionState(ctx, tx, session.ID, StateCredentialReady)
}

// dropSenderReceiverAccess withdraws one Sender's access to the Receiver whose
// association key it no longer holds: its own current association goes, and
// its unfinished work for the same Receiver goes with it, so a later
// key-confirmation report cannot recreate an association the Sender cannot
// encrypt for. The Sender's own lock is held from here, so the association this
// reads is the one the removal acts on, and the Receiver side of the same
// recovery having removed it first leaves this report nothing to withdraw.
func dropSenderReceiverAccess(ctx context.Context, tx *sql.Tx, senderID, receiverID string, now time.Time) (string, []string, error) {
	reservations, err := lockSenderRemoval(ctx, tx, senderID, receiverID, now)
	if err != nil {
		return "", nil, err
	}
	current, err := currentPairing(ctx, tx, senderID, receiverID)
	if err != nil {
		return "", nil, err
	}
	lost := ""
	if current.ID != "" {
		removed, err := removeAssociation(ctx, tx, senderID, current.ID, now)
		if err != nil && !errors.Is(err, ErrAssociationNotFound) {
			return "", nil, err
		}
		lost = removed.PairingID
	}
	canceled, err := cancelReservations(ctx, tx, reservations, now)
	if err != nil {
		return "", nil, err
	}
	return lost, canceled, nil
}

// reconcileRetainedPairings preserves the Receiver's associations whose
// Pairing ID it still holds a key for and removes the ones whose keys it lost,
// together with the unfinished work that reserved a lost Pairing ID. A
// preserved association keeps its Pairing ID, its alias and its activation, and
// the Receiver's credential is replaced separately.
func reconcileRetainedPairings(ctx context.Context, tx *sql.Tx, receiverID string, retained []string, now time.Time) ([]string, error) {
	kept := make(map[string]bool, len(retained))
	for _, pairingID := range retained {
		kept[pairingID] = true
	}
	if err := requireReceiverPairings(ctx, tx, receiverID, kept); err != nil {
		return nil, err
	}

	// Session rows are locked before association rows, which is the order a
	// removal and a claim take them in. A reservation that already activated
	// stays outside this lock, so an activation committing in the window
	// between the two reads keeps its association.
	// shortcut: that association is unusable until the Sender pairs again,
	// serialize activation on the Receiver when this window has to close.
	canceled, err := cancelLostReservations(ctx, tx, receiverID, kept, now)
	if err != nil {
		return nil, err
	}
	associations, err := lockReceiverAssociations(ctx, tx, receiverID)
	if err != nil {
		return nil, err
	}
	for _, association := range associations {
		if kept[association.pairingID] {
			continue
		}
		if _, err := removeAssociation(ctx, tx, association.senderID, association.pairingID, now); err != nil {
			return nil, err
		}
	}
	return canceled, nil
}

// cancelLostReservations closes the Receiver's unfinished pairing work whose
// reserved Pairing ID it did not report as one it still holds.
func cancelLostReservations(ctx context.Context, tx *sql.Tx, receiverID string, kept map[string]bool, now time.Time) ([]string, error) {
	reservations, err := lockReceiverReservations(ctx, tx, receiverID, now)
	if err != nil {
		return nil, err
	}
	lost := make([]lockedReservation, 0, len(reservations))
	for _, reservation := range reservations {
		if !kept[reservation.pairingID] {
			lost = append(lost, reservation)
		}
	}
	return cancelReservations(ctx, tx, lost, now)
}
