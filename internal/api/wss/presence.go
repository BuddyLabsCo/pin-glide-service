package wss

import (
	"sync"
	"time"
)

// PresenceState is one Receiver's published liveness: whether it currently
// holds a committed delivery connection, and when that state began. The owning
// Sender reads it with its own Receiver associations.
type PresenceState struct {
	Online         bool
	StateChangedAt time.Time
}

// Presence is the process-local liveness of Receivers. One API process owns the
// delivery connections, so a Receiver is online exactly while this process
// holds its committed connection: a restart begins with every Receiver offline
// and reports when this process started observing them. No connection
// coordination is persisted, because none outlives the process.
type Presence struct {
	startedAt time.Time

	mutex     sync.Mutex
	receivers map[string]receiverPresence
}

// receiverPresence records the committed connection that makes one Receiver
// online. The connection identifier is what lets an older connection's close
// leave the Receiver online for the newer connection that replaced it.
type receiverPresence struct {
	online       bool
	connectionID uint64
	changedAt    time.Time
}

// NewPresence returns the presence registry of one process run, which started
// observing Receivers at startedAt.
func NewPresence(startedAt time.Time) *Presence {
	return &Presence{startedAt: startedAt, receivers: map[string]receiverPresence{}}
}

// State reports one Receiver's published liveness. A Receiver this process has
// never observed is offline: nothing in this process commits its connection, and
// the service has been reporting it that way since the process started.
func (p *Presence) State(receiverID string) PresenceState {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	presence, observed := p.receivers[receiverID]
	if !observed {
		return PresenceState{StateChangedAt: p.startedAt}
	}
	return PresenceState{Online: presence.online, StateChangedAt: presence.changedAt}
}

// online records that one committed connection now holds the Receiver. The
// state-change time only moves when the published state changes, so a
// reconnect that replaces a live connection keeps the same online time, while a
// close that follows it stays offline.
func (p *Presence) online(receiverID string, connectionID uint64, now time.Time) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	presence := p.receivers[receiverID]
	if !presence.online {
		presence.changedAt = now
	}
	presence.online = true
	presence.connectionID = connectionID
	p.receivers[receiverID] = presence
}

// offline ends the Receiver's online state once the connection that made it
// online is gone. A close of a connection a newer one already replaced changes
// nothing, so a stale cleanup cannot report a healthy newer connection offline.
func (p *Presence) offline(receiverID string, connectionID uint64, now time.Time) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	presence, observed := p.receivers[receiverID]
	if !observed || presence.connectionID != connectionID {
		return
	}
	presence.online = false
	presence.connectionID = 0
	presence.changedAt = now
	p.receivers[receiverID] = presence
}
