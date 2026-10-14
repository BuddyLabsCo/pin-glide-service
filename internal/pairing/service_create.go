package pairing

import (
	"context"
	"database/sql"
	"time"
)

// creation is one created Pairing Session together with the restricted
// credential that opens it and the grant its creation consumed, if any.
type creation struct {
	session  Session
	token    string
	consumed Session
}

// Create opens one restricted session in the requested mode and returns it
// together with the raw B credential, which leaves the service only through
// this result and only to the Receiver side.
func (s *Service) Create(ctx context.Context, request CreateRequest, now time.Time) (Session, string, error) {
	created := creation{}
	err := s.inTransaction(ctx, func(tx *sql.Tx) error {
		var err error
		created, err = s.create(ctx, tx, request, now)
		return err
	})
	if err != nil {
		return Session{}, "", err
	}
	if created.consumed.ID != "" {
		// Presenting the Receiver's credential proved possession of it, so the
		// grant whose credential this was is now consumed.
		s.sessionChanged(created.consumed, now)
	}
	s.sessionChanged(created.session, now)
	return created.session, created.token, nil
}

// create builds one session and its Receiver context inside the caller's
// transaction. The mode is fixed here, and no later request can switch it.
func (s *Service) create(ctx context.Context, tx *sql.Tx, request CreateRequest, now time.Time) (creation, error) {
	receiverID := request.ReceiverID
	// consumed is the issuance grant the created session's own credential
	// presentation proved possession of, if one was still outstanding.
	consumed := Session{}
	switch request.Mode {
	case ModeNewReceiver:
		if request.ReceiverID != "" {
			return creation{}, ErrInvalidMode
		}
		receiverID = newValue()
		if err := insertReceiverRegistration(ctx, tx, receiverID); err != nil {
			return creation{}, err
		}
	case ModeAddSender:
		if request.ReceiverID == "" {
			return creation{}, ErrInvalidMode
		}
		// An already credentialed Receiver authorizes its own pairing channel
		// with the credential it holds, which is the one it presents here.
		admitted, grant, err := admitReceiver(ctx, tx, request.Token, now)
		if err != nil {
			return creation{}, err
		}
		if admitted.ReceiverID != request.ReceiverID {
			// The credential authenticates one Receiver, so every other target
			// stays invisible to the caller that holds it.
			return creation{}, ErrReceiverNotOwned
		}
		consumed = grant
	case ModeRecoverReceiver:
		if request.ReceiverID == "" {
			return creation{}, ErrInvalidMode
		}
		// Recovery acts on a Receiver that already has an owning Account; an
		// unknown, unclaimed or revoked one is not visible to anyone.
		if _, err := claimedReceiverAccount(ctx, tx, receiverID); err != nil {
			return creation{}, err
		}
	default:
		return creation{}, ErrInvalidMode
	}

	info := sessionInfo{
		id:         newValue(),
		mode:       request.Mode,
		receiverID: receiverID,
		state:      StateAwaitingAuthorization,
		createdAt:  now,
		expiresAt:  now.Add(SessionLifetime),
	}
	token := newValue()
	info.tokenDigest = digest(token)
	if err := insertSession(ctx, tx, info); err != nil {
		return creation{}, err
	}
	return creation{session: info.session(), token: token, consumed: consumed}, nil
}
