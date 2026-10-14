package pairing

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Complete records one participant's key-confirmation report. Both reports
// activate the reservation the session holds: a new_receiver session then lets
// its Receiver redeem the first credential, while an add_sender session
// completes without issuing one. Repeating a successful side's report while
// its credential remains valid is idempotent. A reservation that replaces the
// Sender's current association commits that replacement here, and a
// replacement whose expected association is no longer current is reported as
// superseded, which preserves the association that won.
func (s *Service) Complete(ctx context.Context, request CompleteRequest, now time.Time) (Session, error) {
	if request.KeyConfirmation != KeyConfirmed {
		// Only a recovery session may report a missing key, and that report
		// arrives with Receiver credential recovery.
		return Session{}, ErrInvalidMode
	}

	superseded := false
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		session, err := lockedSessionByID(ctx, tx, request.SessionID)
		if err != nil {
			return err
		}
		if err := authorizeParticipant(session, request.SenderID, request.Token); err != nil {
			return err
		}
		if session.Expired(now) {
			// Credential validity comes before protected session state, so the
			// restricted credential reports its own expiry.
			if request.SenderID == "" {
				return ErrCredentialExpired
			}
			return ErrSessionExpired
		}
		switch session.State {
		case StateAwaitingAuthorization:
			// A report is a participant action, so it requires the claim that
			// fixes the Sender side first.
			return ErrPairingNotReady
		case StateAwaitingConfirmation:
		case StateCredentialReady, StateCredentialIssued, StateCompleted:
			// The side's report already committed; repeating it changes
			// nothing and reports the session as it now is.
			return nil
		default:
			return ErrSessionClosed
		}
		if session.Mode == ModeRecoverReceiver {
			// Recovery completes with the retained-key reconciliation of
			// Receiver credential recovery rather than with a Pairing.
			return ErrPairingNotReady
		}
		if request.SenderID != "" {
			if err := confirmSender(ctx, tx, session.ID, now); err != nil {
				return err
			}
		} else if err := confirmReceiver(ctx, tx, session.ID, now); err != nil {
			return err
		}

		senderConfirmed := session.SenderConfirmedAt != nil || request.SenderID != ""
		receiverConfirmed := session.ReceiverConfirmedAt != nil || request.SenderID == ""
		if !senderConfirmed || !receiverConfirmed {
			return nil
		}
		if err := activatePairing(ctx, tx, session, now); err != nil {
			if !errors.Is(err, ErrOperationSuperseded) {
				return err
			}
			// This replacement is stale, so it releases its own reservation
			// and leaves the association that is current untouched.
			if err := supersedeSession(ctx, tx, session, now); err != nil {
				return err
			}
			superseded = true
			return nil
		}
		state := StateCredentialReady
		if session.Mode == ModeAddSender {
			// An add_sender session proves the key but issues no credential.
			state = StateCompleted
		}
		return setSessionState(ctx, tx, session.ID, state)
	})
	if err != nil {
		return Session{}, err
	}
	completed, err := sessionByID(ctx, s.pool, request.SessionID)
	if err != nil {
		return Session{}, storageError(err)
	}
	s.sessionChanged(completed, now)
	if superseded {
		return Session{}, ErrOperationSuperseded
	}
	return completed, nil
}

// Redeem uses the restricted credential to issue the Receiver's token. The
// session's one issuance operation orders the attempts: an equal or lower
// attempt and a competing operation are superseded, while the one permitted
// higher attempt may replace a credential no one has proved possession of yet.
func (s *Service) Redeem(ctx context.Context, request RedeemRequest, now time.Time) (IssuedCredential, error) {
	if request.Attempt != 0 && request.Attempt != 1 {
		return IssuedCredential{}, ErrAttemptUnsupported
	}
	if request.OperationID == "" {
		return IssuedCredential{}, ErrIssuanceInvalid
	}

	issued := IssuedCredential{}
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		// The restricted credential decides its own validity before the
		// session's protected state: an expired B is an expired credential.
		session, err := sessionByTokenDigest(ctx, tx, digest(request.Token))
		if errors.Is(err, ErrSessionNotFound) {
			return ErrCredentialInvalid
		}
		if err != nil {
			return err
		}
		if session.Expired(now) {
			return ErrCredentialExpired
		}
		if session.ID != request.SessionID {
			// A restricted credential authorizes nothing outside its session.
			return ErrCredentialInvalid
		}
		// Admission takes the Receiver's lock before the session row, so
		// redemption takes the same two locks in the same order and waits
		// instead of deadlocking with a concurrent first handshake.
		if err := lockReceiver(ctx, tx, session.ReceiverID); err != nil {
			return err
		}
		session, err = lockedSessionByID(ctx, tx, request.SessionID)
		if err != nil {
			return err
		}

		switch session.Mode {
		case ModeAddSender:
			return ErrModeForbidsCredentialIssue
		case ModeNewReceiver, ModeRecoverReceiver:
		default:
			return ErrInvalidMode
		}
		switch session.State {
		case StateCredentialReady:
			// First redemption: the original attempt, or its retransmission
			// when that attempt never reached the service.
		case StateCredentialIssued:
			if session.IssuanceOperationID != request.OperationID || request.Attempt <= session.IssuanceAttempt {
				return ErrOperationSuperseded
			}
		case StateCompleted, StateCanceled, StateSuperseded:
			return ErrSessionClosed
		default:
			return ErrPairingNotReady
		}

		token, generation, err := issueReceiverCredential(ctx, tx, session.ReceiverID, now)
		if err != nil {
			return err
		}
		if err := recordIssuance(ctx, tx, session.ID, request.OperationID, request.Attempt, generation); err != nil {
			return err
		}
		issued = IssuedCredential{ReceiverID: session.ReceiverID, Token: token}
		return nil
	})
	if err != nil {
		return IssuedCredential{}, err
	}
	if session, err := sessionByID(ctx, s.pool, request.SessionID); err == nil {
		s.sessionChanged(session, now)
	}
	return issued, nil
}

// AdmitReceiver authenticates one presented Receiver credential and commits
// possession of the grant it still owes. Presentation of the current
// generation is what consumes that grant; a superseded generation authorizes
// nothing. The two decisions share the Receiver's serialization boundary, so
// redemption, recovery and admission cannot interleave.
func (s *Service) AdmitReceiver(ctx context.Context, token string, now time.Time) (ReceiverCredential, error) {
	if token == "" {
		return ReceiverCredential{}, ErrCredentialMissing
	}

	admitted := ReceiverCredential{}
	var completed Session
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		admitted, completed, err = admitReceiver(ctx, tx, token, now)
		return err
	})
	if err != nil {
		return ReceiverCredential{}, err
	}
	if completed.ID != "" {
		if session, err := sessionByID(ctx, s.pool, completed.ID); err == nil {
			s.sessionChanged(session, now)
		}
	}
	return admitted, nil
}

// admitReceiver runs the possession decision of one presented Receiver
// credential inside the caller's transaction: the credential must still be the
// Receiver's current one, and proving that consumes the issuance grant it came
// from while one is outstanding. A credential that presented itself here
// authorizes that Receiver only, so the caller decides what else it may do.
func admitReceiver(ctx context.Context, tx *sql.Tx, token string, now time.Time) (ReceiverCredential, Session, error) {
	if token == "" {
		return ReceiverCredential{}, Session{}, ErrCredentialMissing
	}
	presented, err := receiverCredentialByDigest(ctx, tx, digest(token))
	if err != nil {
		return ReceiverCredential{}, Session{}, err
	}
	if err := lockReceiver(ctx, tx, presented.ReceiverID); err != nil {
		return ReceiverCredential{}, Session{}, err
	}
	current, err := lockCurrentReceiverCredential(ctx, tx, presented.ReceiverID)
	if err != nil {
		return ReceiverCredential{}, Session{}, err
	}
	if current.TokenDigest != presented.TokenDigest {
		// A superseded generation is indistinguishable from an unknown
		// credential, so stale possession proves nothing.
		return ReceiverCredential{}, Session{}, ErrCredentialInvalid
	}
	_, state, err := lockRegistration(ctx, tx, presented.ReceiverID)
	if err != nil {
		return ReceiverCredential{}, Session{}, ErrCredentialInvalid
	}
	if state != string(RegistrationActive) {
		return ReceiverCredential{}, Session{}, ErrCredentialInvalid
	}

	consumed := Session{}
	grant, err := outstandingIssuance(ctx, tx, presented.ReceiverID, current.Generation)
	if err != nil && !errors.Is(err, ErrSessionNotFound) {
		return ReceiverCredential{}, Session{}, err
	}
	if err == nil {
		if err := consumeGrant(ctx, tx, grant.ID, now); err != nil {
			return ReceiverCredential{}, Session{}, err
		}
		consumed = grant
	}
	return ReceiverCredential{ReceiverID: presented.ReceiverID, Generation: current.Generation}, consumed, nil
}

// ConfirmReceiverConnection revalidates one admitted connection's credential
// generation under the Receiver's serialization boundary. It is the domain half
// of the final upgrade commit: a revocation or recovery that committed while
// the handshake was in flight leaves the presented generation superseded, so the
// service cancels the pending connection instead of admitting it. The same
// boundary that serializes redemption and recovery therefore also decides
// whether an upgrade commits first or a generation change does.
func (s *Service) ConfirmReceiverConnection(ctx context.Context, receiverID string, generation int64) error {
	return s.inTransaction(ctx, func(tx *sql.Tx) error {
		if err := lockReceiver(ctx, tx, receiverID); err != nil {
			return err
		}
		current, err := lockCurrentReceiverCredential(ctx, tx, receiverID)
		if err != nil {
			return err
		}
		if current.Generation != generation {
			// The Receiver already has a newer generation, which is the
			// generation its live access belongs to.
			return ErrCredentialInvalid
		}
		_, state, err := lockRegistration(ctx, tx, receiverID)
		if err != nil {
			return err
		}
		if state != string(RegistrationActive) {
			// A closed registration denies the connection even while its last
			// credential row still exists.
			return ErrCredentialInvalid
		}
		return nil
	})
}
