package wss

import (
	"sync/atomic"

	"github.com/coder/websocket"
)

// receiverSlot is the reserved connection of one Receiver generation. It is
// reserved before the upgrade commits and released only by the connection that
// owns it, so a failed upgrade, a rejected handshake or a stale close callback
// cannot release a newer connection's slot.
type receiverSlot struct {
	connectionID uint64
	key          receiverKey
	socket       *websocket.Conn
	// closeCode is the Close status this connection ended with, or 0 while it
	// is open or its peer ended it. Shutdown and the liveness goroutine end a
	// connection from different goroutines, so whichever ends it first claims
	// the meaning the retained record carries.
	closeCode atomic.Int32
	// ending carries the Close meaning when the service ends this committed
	// connection, so the goroutine that owns the socket also owns that close.
	ending chan closeIntent
}

// claim records the Close status of this connection once, so the first closer
// owns the meaning and a later one cannot restate it.
func (s *receiverSlot) claim(code int) bool {
	return s.closeCode.CompareAndSwap(0, int32(code))
}

// end claims a service-started close's status and performs it; a later closer
// yields, so the peer and the retained record carry the status claimed first.
func (s *receiverSlot) end(intent closeIntent) {
	code, reason := closeMeaning(intent)
	if !s.claim(int(code)) {
		return
	}
	_ = s.socket.Close(code, reason)
}
