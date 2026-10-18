package pairing

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// SenderMutations is the credential half of a Sender credential change. Which
// Sender a credential authorizes and which credential rules a revocation
// replays belong to the identity domain, so the pairing domain asks for those
// two facts through this seam instead of restating them: the transaction that
// also removes the Sender's Pairings owns one commit across both.
type SenderMutations interface {
	// SwitchAccount revokes one Sender's credential family inside this
	// transaction.
	SwitchAccount(ctx context.Context, tx *sql.Tx, senderID, managementCredential string, now time.Time) error
	// RevokeSender closes one Sender registration and revokes every credential
	// it holds inside this transaction.
	RevokeSender(ctx context.Context, tx *sql.Tx, senderID, ownerAccount string, now time.Time) error
}

// senderCleanUp is what one Sender credential change changed about that Sender's
// Pairings: the associations it removed and the live sessions it closed. The
// committing transaction publishes both once they are durable.
type senderCleanUp struct {
	Removed  []Association
	Sessions []string
}

// DeviceRevocation permanently closes one device registration the caller's own
// Account owns, whichever role that registration has: the caller proves a fresh
// identity authorization for the Account its management credential belongs to,
// and the device ID names one Sender or Receiver registration of that Account,
// so revocation stays an owner action. A Sender revocation invalidates every
// credential of the named Sender, cancels its unfinished authorized Pairing work
// and removes its own associations; a Receiver revocation invalidates its
// credential generation, cancels the grants it could still redeem, ends its
// delivery connection and removes every Pairing that forwards to it. Each is one
// commit, and each leaves every other registration, credential, association and
// connection as it was.
func (s *Service) DeviceRevocation(ctx context.Context, managementCredential, deviceID string, proof identity.Proof, now time.Time) error {
	if s.revocations == nil {
		return ErrStorageUnavailable
	}
	// A registration an owner permanently closed authorizes nothing, so its own
	// credential reports revocation before the closed registration is even
	// considered; an absent or unusable one never reaches the owner proof.
	callerID, err := s.authenticator.Authenticate(ctx, managementCredential, string(identity.ScopeUserSession))
	if err != nil {
		return managementOutcome(err)
	}
	caller, err := senderAccount(ctx, s.pool, callerID)
	if err != nil {
		return storageError(err)
	}
	// The provider call is an external boundary, so it stays outside the
	// transaction that closes the registration.
	owner, err := s.verifyOwner(ctx, proof)
	if err != nil {
		return err
	}
	if owner.AccountID != caller {
		// A proof that verifies another Account proves nothing about this
		// caller's own registrations.
		return ErrFreshIdentityRequired
	}
	// A device ID names at most one registration, and the branch that acts on
	// that registration decides ownership and state again under its own locks,
	// so this read only routes the request to the right one.
	sender, err := senderRegistrationExists(ctx, s.pool, deviceID)
	if err != nil {
		return storageError(err)
	}
	if sender {
		return s.senderDeviceRevocation(ctx, deviceID, owner.AccountID, now)
	}
	return s.receiverDeviceRevocation(ctx, deviceID, owner.AccountID, now)
}

// senderDeviceRevocation permanently closes one Sender registration: every
// credential of that Sender is invalidated, its unfinished authorized Pairing
// work is canceled and its own associations are removed, all in one commit. The
// Receiver's credential, its delivery connection and every other Sender's
// association are untouched, and a later login presenting the closed Sender ID
// is refused forever.
func (s *Service) senderDeviceRevocation(ctx context.Context, senderID, ownerAccount string, now time.Time) error {
	return s.senderTransaction(ctx, senderID, now, func(ctx context.Context, tx *sql.Tx) error {
		return s.revocations.RevokeSender(ctx, tx, senderID, ownerAccount, now)
	})
}

// SwitchAccount ends one Sender's service access and removes that Sender's own
// associations in one commit, which is what an Account switch on an
// installation requires. Switching removes associations, so only management
// authority may do it. The registration stays active and every other Sender,
// Receiver credential and delivery connection survives; whatever local
// identity, keys and storage the switch also clears belongs to the client.
func (s *Service) SwitchAccount(ctx context.Context, managementCredential string, now time.Time) error {
	if managementCredential == "" {
		return ErrCredentialMissing
	}
	// The credential is read once to name the Sender whose transaction this is;
	// the transaction then re-reads it under its locks and decides with that.
	senderID, err := s.authenticator.Authenticate(ctx, managementCredential, string(identity.ScopeUserSession))
	if err != nil {
		return managementOutcome(err)
	}
	err = s.senderTransaction(ctx, senderID, now, func(ctx context.Context, tx *sql.Tx) error {
		return s.revocations.SwitchAccount(ctx, tx, senderID, managementCredential, now)
	})
	return err
}

// managementOutcome restates one management credential result. Switching an
// Account without presenting any credential reports the missing source rather
// than an invalid one; every other unusable value stays the credential outcome
// the identity domain reported.
func managementOutcome(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrCredentialMissing):
		return ErrCredentialMissing
	default:
		return credentialOutcome(err)
	}
}

// senderOutcome restates one identity outcome a Sender revocation reports. The
// registration this caller owns is already permanently closed, which the
// published contract reports as its own outcome rather than as an internal
// failure.
func senderOutcome(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, identity.ErrSenderRevoked):
		return ErrSenderRevoked
	case errors.Is(err, identity.ErrSenderNotFound):
		return ErrSenderNotOwned
	case errors.Is(err, identity.ErrStorageUnavailable):
		return ErrStorageUnavailable
	default:
		return err
	}
}

// senderTransaction runs one Sender credential change together with the Pairing
// cleanup that belongs to it, and publishes what that one commit changed to
// each Receiver and session participant. A failure after the credential change
// rolls the whole change back; a process composed without a delivery notifier
// still commits the same change and only stops reporting it to Receivers.
func (s *Service) senderTransaction(ctx context.Context, senderID string, now time.Time, change func(ctx context.Context, tx *sql.Tx) error) error {
	cleanUp := senderCleanUp{}
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		// The Sender's unfinished work is locked before the Sender's own
		// advisory lock, which is the order a claim, a completion and an
		// explicit removal take those two locks in, so this change waits for
		// them instead of deadlocking with them. Neither change below writes
		// one of these reservations, so locking them here reads the same set.
		reservations, err := lockSenderReservations(ctx, tx, senderID, now)
		if err != nil {
			return err
		}
		if err := lockSender(ctx, tx, senderID); err != nil {
			return err
		}
		if err := change(ctx, tx); err != nil {
			return err
		}
		cleanUp, err = cleanSenderPairings(ctx, tx, senderID, now, reservations)
		return err
	})
	if err != nil {
		return err
	}

	// Only the committed change is published: a Receiver learns that a Pairing
	// was removed only once that removal is durable.
	if s.deliveryNotifier != nil {
		for _, removed := range cleanUp.Removed {
			s.deliveryNotifier.PairingRemoved(senderID, removed.ReceiverID, removed.PairingID)
		}
	}
	for _, sessionID := range cleanUp.Sessions {
		if session, err := sessionByID(ctx, s.pool, sessionID); err == nil {
			s.sessionChanged(session, now)
		}
	}
	return nil
}

// cleanSenderPairings cancels one Sender's unfinished authorized Pairing work
// and removes that Sender's own associations inside the caller's transaction.
// Another Sender's association with the same Receiver, the Receiver's
// credential and its delivery connection are not touched.
func cleanSenderPairings(ctx context.Context, tx *sql.Tx, senderID string, now time.Time, reservations []lockedReservation) (senderCleanUp, error) {
	cleanUp := senderCleanUp{}
	for _, reservation := range reservations {
		// Closing a session releases its reservation exactly as an explicit
		// cancellation does, so the two paths cannot drift apart and a stale
		// confirmation report finds nothing left to activate.
		if err := closeSession(ctx, tx, Session{
			ID:               reservation.sessionID,
			PendingPairingID: reservation.pairingID,
		}, StateCanceled, now); err != nil {
			return senderCleanUp{}, err
		}
		cleanUp.Sessions = append(cleanUp.Sessions, reservation.sessionID)
	}
	// Whatever this Sender still holds afterwards is removed with the same
	// commit.
	removed, err := removeSenderAssociations(ctx, tx, senderID, now)
	if err != nil {
		return senderCleanUp{}, err
	}
	cleanUp.Removed = removed
	return cleanUp, nil
}
