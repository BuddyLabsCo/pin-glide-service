package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db/jetgen/pin_glide/model"
	"github.com/go-jet/jet/v2/qrm"
)

// The credential half of a Sender revocation and an Account switch. Both run
// inside a transaction the caller owns, because each must commit together with
// the Pairing cleanup of the same Sender: a Sender that kept its associations
// without keeping its credentials, or the other way round, is exactly what
// these operations exist to prevent. The pairing domain's Sender security
// operation owns that transaction, which is what makes both domains' effects
// one commit.
//
// These functions report only their domain outcomes: a database failure stays
// the caller's to classify, and an explicit outcome such as
// ErrCredentialInvalid is exactly what the HTTP surface reports.

// SwitchAccountTx revokes one Sender's whole credential family inside one
// transaction the caller owns, the way an Account switch requires: the Sender
// stops authorizing anything, and the registration itself stays active so the
// Sender can log in again under another Account only through a new
// registration.
//
// The caller resolves the presented credential and names the Sender it
// authorizes before this transaction, and the credential is read again under
// this transaction's locks, exactly as ordinary logout reads its own source
// again. A switch whose authority an earlier login, renewal or logout already
// replaced therefore revokes nothing instead of revoking the family that
// replaced its own.
func (s *Service) SwitchAccountTx(ctx context.Context, tx *sql.Tx, senderID, presentedCredential string, now time.Time) error {
	sender, err := lockSender(ctx, tx, senderID)
	if errors.Is(err, qrm.ErrNoRows) {
		return ErrCredentialInvalid
	}
	if err != nil {
		return err
	}
	if sender.State != model.IdentitySenderRegistrationState_Active {
		// A registration an owner permanently closed authorizes no switch.
		return ErrCredentialRevoked
	}
	presented, err := lockCredential(ctx, tx, digest(presentedCredential))
	if err != nil {
		return err
	}
	// These are the outcomes the first read reports, so a switch refused under
	// the lock reports the same fact as one refused before it.
	if presented.SenderID != senderID || presented.RevokedAt != nil ||
		Scope(presented.Scope) != ScopeUserSession {
		return ErrCredentialInvalid
	}
	if !presented.ExpiresAt.After(now) {
		return ErrCredentialExpired
	}
	return revokeSenderCredentials(ctx, tx, senderID, now)
}

// RevokeSenderTx permanently closes one Sender registration inside one
// transaction the caller owns. The registration is closed before its
// credentials are revoked, so a later request can never observe credentials
// that still authorize an active registration, and an already closed
// registration is refused rather than closed twice.
func (s *Service) RevokeSenderTx(ctx context.Context, tx *sql.Tx, senderID, ownerAccount string, now time.Time) error {
	registration, err := lockSender(ctx, tx, senderID)
	if errors.Is(err, qrm.ErrNoRows) {
		return ErrSenderNotFound
	}
	if err != nil {
		return err
	}
	if registration.AccountID != ownerAccount {
		// Another Account's registration is as invisible as one that does not
		// exist, and its own owner learns it is already closed.
		return ErrSenderNotFound
	}
	if registration.State == model.IdentitySenderRegistrationState_Revoked {
		return ErrSenderRevoked
	}
	if err := closeSender(ctx, tx, senderID, now); err != nil {
		return err
	}
	return revokeSenderCredentials(ctx, tx, senderID, now)
}
