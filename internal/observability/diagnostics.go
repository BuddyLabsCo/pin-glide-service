package observability

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// diagnosticQueue holds the records one write burst can hand over before an
	// insert keeps up. A full queue drops its record rather than delaying the
	// operation it describes.
	diagnosticQueue = 256
	// diagnosticWriteTimeout bounds one insert, so a database that stopped
	// answering cannot hold the writer goroutine open.
	diagnosticWriteTimeout = 5 * time.Second
)

// diagnosticEvent is one safe outcome of the composed service. Every field is
// an identifier, a fixed meaning or a duration the service itself produced, so
// no request body, credential, ciphertext or provider payload can reach it.
type diagnosticEvent struct {
	occurredAt time.Time
	requestID  string
	kind       string
	subject    string
	method     string
	code       *int
	durationMS *int64
}

// DiagnosticEvent is one service outcome a caller can record: an HTTP request
// that finished, or a WSS connection that ended. An empty text or absent
// number is stored as NULL.
type DiagnosticEvent struct {
	RequestID string
	// Subject is the safe Receiver or Pairing Session identifier the outcome
	// belongs to, empty when it belongs to neither.
	Subject string
	// Method is the HTTP method of the recorded operation, empty for one that
	// has none.
	Method string
	// Code is the HTTP status or the WSS Close code, nil before it has one.
	Code *int
	// Duration is how long the recorded operation took, nil for a connection
	// that reports its own lifetime instead.
	Duration *time.Duration
}

// Recorder persists safe service outcomes. It never records credentials,
// payloads or provider data, and it cannot fail the operation it describes.
type Recorder interface {
	// RecordHTTP stores one finished HTTP request.
	RecordHTTP(event DiagnosticEvent)
	// RecordWSS stores one accepted or ended WSS connection.
	RecordWSS(event DiagnosticEvent)
}

// Diagnostics stores service outcomes in the diagnostic table: the log tier of
// this service, never its authoritative state. Writes happen on their own
// goroutine, so recording an outcome never delays the request that produced it.
type Diagnostics struct {
	pool   *sql.DB
	logger *slog.Logger

	events  chan diagnosticEvent
	done    chan struct{}
	once    sync.Once
	closed  atomic.Bool
	dropped atomic.Int64
}

// NewDiagnostics starts a recorder that stores outcomes through pool.
func NewDiagnostics(pool *sql.DB, logger *slog.Logger) *Diagnostics {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	diagnostics := &Diagnostics{
		pool:   pool,
		logger: logger,
		events: make(chan diagnosticEvent, diagnosticQueue),
		done:   make(chan struct{}),
	}
	go diagnostics.write()
	return diagnostics
}

// RecordHTTP stores one finished HTTP request.
func (d *Diagnostics) RecordHTTP(event DiagnosticEvent) {
	d.record("http", event)
}

// RecordWSS stores one accepted or ended WSS connection.
func (d *Diagnostics) RecordWSS(event DiagnosticEvent) {
	d.record("wss", event)
}

// Close stores the records still queued and joins the writer, so a caller can
// read what it recorded before the process ends. It is safe to call twice.
func (d *Diagnostics) Close() {
	d.once.Do(func() {
		d.closed.Store(true)
		close(d.events)
		<-d.done
		if dropped := d.dropped.Load(); dropped > 0 {
			d.logger.Warn("service diagnostics dropped records",
				slog.Int64("dropped", dropped),
			)
		}
	})
}

// record queues one event, or drops it when the writer is behind or the
// recorder has closed. An outcome record is diagnostic only, so losing it under
// pressure is safe while delaying its caller is not.
func (d *Diagnostics) record(kind string, event DiagnosticEvent) {
	if d.closed.Load() {
		d.dropped.Add(1)
		return
	}
	select {
	case d.events <- diagnosticEvent{
		occurredAt: time.Now().UTC(),
		requestID:  event.RequestID,
		kind:       kind,
		subject:    event.Subject,
		method:     event.Method,
		code:       event.Code,
		durationMS: milliseconds(event.Duration),
	}:
	default:
		d.dropped.Add(1)
	}
}

// write stores queued events until the recorder closes, which drains whatever
// the caller queued before that.
func (d *Diagnostics) write() {
	defer close(d.done)
	for event := range d.events {
		d.insert(event)
	}
}

// insert stores one event. A failed insert is reported and dropped: a record
// describes an outcome the service already answered, so it never changes one.
func (d *Diagnostics) insert(event diagnosticEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), diagnosticWriteTimeout)
	defer cancel()
	_, err := d.pool.ExecContext(ctx, `
INSERT INTO pin_glide.service_diagnostics
    (occurred_at, request_id, kind, subject, method, code, duration_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		event.occurredAt, event.requestID, event.kind, textOrNull(event.subject),
		textOrNull(event.method), event.code, event.durationMS)
	if err != nil {
		d.logger.Warn("service diagnostic was not stored",
			slog.String("request_id", event.requestID),
			slog.String("kind", event.kind),
			slog.String("cause", err.Error()),
		)
	}
}

func textOrNull(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// milliseconds measures one operation in whole milliseconds, or reports no
// duration for a record that has none.
func milliseconds(duration *time.Duration) *int64 {
	if duration == nil {
		return nil
	}
	milliseconds := duration.Milliseconds()
	return &milliseconds
}
