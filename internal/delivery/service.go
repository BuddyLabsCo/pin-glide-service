package delivery

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/go-jet/jet/v2/qrm"
)

// Service implements Delivery admission, deduplication, Receipt handling and
// terminal results over PostgreSQL. Live work belongs to this process, so a
// duplicate submission in this process joins it instead of sending again, and
// a restart leaves only the durable identity and the terminal outcomes.
type Service struct {
	pool          *sql.DB
	channel       Channel
	authenticator Authenticator

	// writeTimeout, receiptTimeout and batchBudget are the adopted bounds of
	// one upload. They bound work; they are not fixed delays.
	writeTimeout   time.Duration
	receiptTimeout time.Duration
	batchBudget    time.Duration

	mutex sync.Mutex
	live  map[key]*work

	// stopping is closed when this process stops taking Delivery work, which
	// also ends the wait of the work it still owns.
	stopping chan struct{}
	// closing is the same fact as stopping, held by the mutex that serializes
	// it with the registration of each entry.
	closing bool
	// running counts the registered work still executing, so Shutdown joins it
	// before the process stops touching its storage.
	running sync.WaitGroup
}

// NewService composes the delivery service on its storage, live Receiver
// channel and upload authority.
func NewService(pool *sql.DB, channel Channel, authenticator Authenticator) *Service {
	return &Service{
		pool:           pool,
		channel:        channel,
		authenticator:  authenticator,
		writeTimeout:   WriteTimeout,
		receiptTimeout: ReceiptTimeout,
		batchBudget:    BatchBudget,
		live:           map[key]*work{},
		stopping:       make(chan struct{}),
	}
}

// Shutdown stops this process from registering further Delivery work and joins
// the registered work it still owns inside ctx's window. A Delivery this
// process can no longer confirm finalizes interrupted where storage permits,
// and nothing recovers, retries or resends its ciphertext.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mutex.Lock()
	if !s.closing {
		s.closing = true
		close(s.stopping)
	}
	s.mutex.Unlock()

	// Wait has no context of its own, so the join is raced against the window
	// rather than blocking the caller past it.
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		s.running.Wait()
	}()

	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// work is the live Delivery one Sender, Message and Receiver identity owns in
// this process: the Receipt it is waiting for and the terminal outcome every
// duplicate that joined it reads. The closed done channel publishes that
// outcome, and the closed receipt channel makes a Receipt idempotent.
type work struct {
	done    chan struct{}
	receipt chan struct{}
	once    sync.Once

	// opened says the owner started its own send, so only a Receipt that can
	// belong to this send is accepted. It is guarded by Service.mutex.
	opened bool

	// result and err are written before done is closed and read after it.
	result Result
	err    error
}

// Upload admits one Sender's Message and answers the terminal outcome of every
// entry, in input order. The upload credential authorizes the whole request;
// every entry is then validated and authorized before its dedup lookup, and
// the entries are processed in parallel inside the adopted batch budget.
//
// A failure that leaves an entry without a reliable terminal result reports
// ErrStatusUnavailable instead of a pending success, because delivery may
// already have occurred. Work this call registered keeps running to its own
// terminal outcome after the caller disconnects, and nothing recovers or
// resends ciphertext afterwards.
func (s *Service) Upload(ctx context.Context, uploadCredential string, request Request) ([]Result, error) {
	senderID, err := s.authenticator.Authenticate(ctx, uploadCredential)
	if err != nil {
		return nil, err
	}

	// The batch budget is measured from admission, and the caller's own
	// cancellation ends its wait rather than the work it registered.
	deadline := time.Now().UTC().Add(s.batchBudget)
	batch, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()

	duplicated := duplicatedReceivers(request.Items)
	results := make([]Result, len(request.Items))

	var (
		waiting sync.WaitGroup
		mutex   sync.Mutex
		failed  error
	)
	for index, item := range request.Items {
		// Registering under the lock Shutdown takes is what makes its join
		// complete: no entry starts after the process stopped accepting work.
		if !s.startWork() {
			return nil, ErrStatusUnavailable
		}
		waiting.Go(func() {
			defer s.running.Done()

			result, err := s.admit(batch, senderID, request.MessageID, index, item, duplicated[item.ReceiverID])
			if err != nil {
				mutex.Lock()
				if failed == nil {
					failed = err
				}
				mutex.Unlock()
				return
			}
			results[index] = result
		})
	}
	waiting.Wait()
	if failed != nil {
		return nil, failed
	}
	return results, nil
}

// startWork counts one entry of registered work, or reports that the process
// stopped taking Delivery work.
func (s *Service) startWork() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closing {
		return false
	}
	s.running.Add(1)
	return true
}

// Deliveries returns the retained metadata of one Sender's own Message.
// Processing work this process no longer owns is reported as interrupted
// rather than left pending, and a Message without admitted Deliveries is
// reported as absent.
func (s *Service) Deliveries(ctx context.Context, uploadCredential, messageID string) ([]Delivery, error) {
	senderID, err := s.authenticator.Authenticate(ctx, uploadCredential)
	if err != nil {
		return nil, err
	}

	deliveries, err := deliveriesOfMessage(ctx, s.pool, senderID, messageID)
	if err != nil {
		return nil, err
	}
	if len(deliveries) == 0 {
		return nil, ErrMessageNotFound
	}

	for index, retained := range deliveries {
		if retained.Status != protocol.DeliveryProcessing {
			continue
		}
		identity := key{senderID: senderID, messageID: messageID, receiverID: retained.ReceiverID}
		if s.working(identity) {
			continue
		}
		resolved, err := interruptDelivery(ctx, s.pool, identity, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		deliveries[index] = resolved
	}
	return deliveries, nil
}

// Receipt records one Receipt frame an authenticated Receiver sent. Only live
// work of that Receiver accepts it: an unknown, duplicate or late Receipt
// changes nothing, so a terminal Delivery result is never rewritten.
func (s *Service) Receipt(senderID, messageID, receiverID string) {
	s.mutex.Lock()
	live, exists := s.live[key{senderID: senderID, messageID: messageID, receiverID: receiverID}]
	opened := exists && live.opened
	s.mutex.Unlock()

	if !opened {
		return
	}
	live.once.Do(func() { close(live.receipt) })
}

// admit returns the terminal outcome of one entry. The order is the accepted
// one: a duplicated Receiver first, then the entry's own shape and encoding,
// then the current eligibility of its Receiver, and only then the retained
// dedup identity, so an entry that lost its authority can never reuse an
// earlier Delivery's result.
func (s *Service) admit(ctx context.Context, senderID, messageID string, index int, item Item, duplicated bool) (Result, error) {
	if duplicated {
		return reject(index, item.ReceiverID, ErrDuplicateReceiver), nil
	}
	if err := item.validate(); err != nil {
		return reject(index, item.ReceiverID, err), nil
	}
	if err := eligible(ctx, s.pool, senderID, item); err != nil {
		if errors.Is(err, ErrReceiverNotAuthorized) {
			return reject(index, item.ReceiverID, err), nil
		}
		return Result{}, err
	}
	return s.forward(ctx, senderID, messageID, index, item)
}

// forward owns or joins the one live send of one Delivery identity. This
// process registers the identity before PostgreSQL does, so a duplicate that
// arrives while the first send runs joins that work instead of claiming a
// second Delivery for the same identity.
func (s *Service) forward(ctx context.Context, senderID, messageID string, index int, item Item) (Result, error) {
	identity := key{senderID: senderID, messageID: messageID, receiverID: item.ReceiverID}
	live, owner := s.enter(identity)
	if !owner {
		return joined(ctx, live, index, item.ReceiverID)
	}

	result, err := s.run(ctx, identity, index, item, live)
	s.leave(identity, result, err)
	return result, err
}

// run owns the live send of one Delivery identity. The processing row is
// claimed before any transport side effect, one owner sends, and the terminal
// outcome is stored before it is reported as confirmed.
func (s *Service) run(ctx context.Context, identity key, index int, item Item, live *work) (Result, error) {
	claimed, retained, err := claimDelivery(ctx, s.pool, identity, item.PairingID, time.Now().UTC())
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		status, err := retainedStatus(ctx, s.pool, identity, retained)
		if err != nil {
			return Result{}, err
		}
		return outcome(index, item.ReceiverID, status), nil
	}

	status, err := s.send(ctx, identity, item, live)
	if err != nil {
		return Result{}, err
	}
	if err := completeDelivery(ctx, s.pool, identity, status, time.Now().UTC()); err != nil {
		return Result{}, err
	}
	return outcome(index, item.ReceiverID, status), nil
}

// send writes one ciphertext to the Receiver's current committed connection
// and waits for its Receipt within the adopted bounds. The frame is a local
// value, so this entry's ciphertext copy ends with the attempt and is neither
// stored nor written to any log.
func (s *Service) send(ctx context.Context, identity key, item Item, live *work) (protocol.DeliveryStatus, error) {
	frame := protocol.Delivery{
		Version:       protocol.WSSVersion,
		Type:          "delivery",
		SenderID:      identity.senderID,
		ReceiverID:    identity.receiverID,
		PairingID:     item.PairingID,
		MessageID:     identity.messageID,
		CryptoVersion: protocol.CryptoVersion,
		Nonce:         item.Nonce,
		Ciphertext:    item.Ciphertext,
	}

	// A Receipt counts only once this work started its own send, so one left
	// over from an earlier Delivery of the same identity cannot be mistaken for
	// this one.
	s.mutex.Lock()
	live.opened = true
	s.mutex.Unlock()

	// Work that outlived the process taking it, or its own batch budget, never
	// starts a send: it has no outcome to confirm and is interrupted instead.
	if s.isStopping() || ctx.Err() != nil {
		return protocol.DeliveryInterrupted, nil
	}

	delivered, err := s.channel.Deliver(identity.receiverID, frame, s.writeTimeout)
	if !delivered {
		switch {
		case s.isStopping():
			// Shutdown ended this attempt, so its failure is this process
			// stopping rather than an outcome of the Receiver's transport.
			return protocol.DeliveryInterrupted, nil
		case err != nil:
			return protocol.DeliverySendFailed, nil
		default:
			return protocol.DeliveryOffline, nil
		}
	}

	select {
	case <-live.receipt:
		return protocol.DeliveryReceived, nil
	case <-time.After(s.receiptTimeout):
		return protocol.DeliveryAckTimeout, nil
	case <-ctx.Done():
		// The batch budget ended this wait before any Receipt confirmed
		// arrival, so the registered work has no confirmable outcome.
		return protocol.DeliveryInterrupted, nil
	case <-s.stopping:
		// Bounded shutdown ended this wait without an outcome of its own.
	}

	// A Receipt this process recorded as shutdown ended the wait still confirms
	// arrival, so it decides the outcome shutdown could not, and that outcome is
	// still stored while storage is reachable.
	select {
	case <-live.receipt:
		return protocol.DeliveryReceived, nil
	default:
		return protocol.DeliveryInterrupted, nil
	}
}

// isStopping reports whether this process stopped taking Delivery work.
func (s *Service) isStopping() bool {
	select {
	case <-s.stopping:
		return true
	default:
		return false
	}
}

// enter registers this caller as the one owner of one Delivery identity, or
// reports the live work an earlier duplicate already started.
func (s *Service) enter(identity key) (*work, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if live, exists := s.live[identity]; exists {
		return live, false
	}
	started := &work{done: make(chan struct{}), receipt: make(chan struct{})}
	s.live[identity] = started
	return started, true
}

// leave ends the live work of one Delivery identity and wakes every duplicate
// that joined it with the outcome this owner obtained.
func (s *Service) leave(identity key, result Result, err error) {
	s.mutex.Lock()
	live := s.live[identity]
	delete(s.live, identity)
	s.mutex.Unlock()

	live.result, live.err = result, err
	close(live.done)
}

// joined waits for the live work of a duplicated entry and answers its
// terminal result. The duplicate's own ciphertext copy is never sent, and live
// work that does not finish inside the batch budget is uncertain rather than
// pending.
func joined(ctx context.Context, live *work, index int, receiverID string) (Result, error) {
	select {
	case <-live.done:
		if live.err != nil {
			return Result{}, live.err
		}
		return outcome(index, receiverID, live.result.Status), nil
	case <-ctx.Done():
		return Result{}, ErrStatusUnavailable
	}
}

// working reports whether this process currently owns live work for one
// identity, which is what tells ongoing work from work whose owner is gone.
func (s *Service) working(identity key) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	_, live := s.live[identity]
	return live
}

// duplicatedReceivers reports the Receivers more than one readable entry of
// the same request named. An entry whose Receiver could not be read is never a
// duplicate, and every entry of a duplicated Receiver is rejected, so the
// service never chooses one of two different ciphertexts for one Delivery.
func duplicatedReceivers(items []Item) map[string]bool {
	entries := map[string]int{}
	for _, item := range items {
		if item.ReceiverID != "" {
			entries[item.ReceiverID]++
		}
	}

	duplicated := map[string]bool{}
	for receiverID, count := range entries {
		if count > 1 {
			duplicated[receiverID] = true
		}
	}
	return duplicated
}

// reject builds the terminal result of one entry this invocation did not
// forward.
func reject(index int, receiverID string, err error) Result {
	return Result{Index: index, ReceiverID: receiverID, Status: protocol.DeliveryRejected, Err: err}
}

// outcome builds the terminal result of one entry whose Delivery reached the
// given status.
func outcome(index int, receiverID string, status protocol.DeliveryStatus) Result {
	return Result{Index: index, ReceiverID: receiverID, Status: status, Err: TerminalError(status)}
}

// retainedStatus reports the outcome of an identity this process did not
// claim. Retained terminal work keeps the outcome it stored, and processing
// work without a live owner in this process is interrupted, never resumed.
func retainedStatus(ctx context.Context, db qrm.Queryable, identity key, retained Delivery) (protocol.DeliveryStatus, error) {
	if retained.Status != protocol.DeliveryProcessing {
		return retained.Status, nil
	}
	interrupted, err := interruptDelivery(ctx, db, identity, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return interrupted.Status, nil
}

// newIdentity returns an unpredictable Delivery identity. It carries no
// authority and is never derived from a Message ID or a Receiver.
func newIdentity() string {
	return rand.Text()
}
