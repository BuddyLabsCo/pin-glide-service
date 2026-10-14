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
// its credential remains valid is idempotent.
func (s *Service) Complete(ctx context.Context, request CompleteRequest, now time.Time) (Session, error) {
	if request.KeyConfirmation != KeyConfirmed {
		// Only a recovery session may report a missing key, and that report
		// arrives with Receiver credential recovery.
		return Session{}, ErrInvalidMode
	}

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
			return err
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
		issued = IssuedCredential{ReceiverID: session.ReceiverID, Token: token, Generation: generation}
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
		presented, err := receiverCredentialByDigest(ctx, tx, digest(token))
		if err != nil {
			return err
		}
		if err := lockReceiver(ctx, tx, presented.ReceiverID); err != nil {
			return err
		}
		current, err := lockCurrentReceiverCredential(ctx, tx, presented.ReceiverID)
		if err != nil {
			return err
		}
		if current.TokenDigest != presented.TokenDigest {
			// A superseded generation is indistinguishable from an unknown
			// credential, so stale possession proves nothing.
			return ErrCredentialInvalid
		}
		_, state, err := lockRegistration(ctx, tx, presented.ReceiverID)
		if err != nil {
			return ErrCredentialInvalid
		}
		if state != string(RegistrationActive) {
			return ErrCredentialInvalid
		}

		grant, err := outstandingIssuance(ctx, tx, presented.ReceiverID, current.Generation)
		if err != nil && !errors.Is(err, ErrSessionNotFound) {
			return err
		}
		if err == nil {
			if err := consumeGrant(ctx, tx, grant.ID, now); err != nil {
				return err
			}
			completed = grant
		}
		admitted = ReceiverCredential{ReceiverID: presented.ReceiverID, Generation: current.Generation}
		return nil
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
