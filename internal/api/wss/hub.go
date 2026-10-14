package wss

import (
	"sync"

	"github.com/BuddyLabsCo/pin-glide-service/internal/pairing"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// Hub is the in-memory registry of live pairing connections. One API process
// owns it, so routing and state fan-out stay local to that process and never
// become durable work.
type Hub struct {
	mutex       sync.Mutex
	connections map[string]map[pairing.Side]*Connection
}

// NewHub returns an empty registry.
func NewHub() *Hub {
	return &Hub{connections: map[string]map[pairing.Side]*Connection{}}
}

// Subscribe registers one authenticated connection. A reconnect of the same
// session and side replaces the old connection, whose close ends its serve
// loop rather than leaving two readers on one side.
func (h *Hub) Subscribe(connection *Connection) {
	h.mutex.Lock()
	peers, known := h.connections[connection.sessionID]
	if !known {
		peers = map[pairing.Side]*Connection{}
		h.connections[connection.sessionID] = peers
	}
	previous := peers[connection.side]
	peers[connection.side] = connection
	h.mutex.Unlock()

	if previous != nil && previous != connection {
		previous.Close(closeNothing)
	}
}

// Unsubscribe removes one connection. A replaced connection's close runs
// after the replacement registered, so ownership is checked before removal.
func (h *Hub) Unsubscribe(connection *Connection) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	peers, known := h.connections[connection.sessionID]
	if !known || peers[connection.side] != connection {
		return
	}
	delete(peers, connection.side)
	if len(peers) == 0 {
		delete(h.connections, connection.sessionID)
	}
}

// Relay delivers one participant's handshake frame to the other participant.
// It reports whether a peer was present to receive it.
func (h *Hub) Relay(sessionID string, from pairing.Side, message protocol.PairingRelayOut) bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	peers := h.connections[sessionID]
	peer := peers[from.Other()]
	if peer == nil {
		return false
	}
	peer.Send(message)
	return true
}

// SendState publishes one session state snapshot to every participant
// currently subscribed to it. It is the transport half of the domain's change
// callback, so a reconnect and a state change both reach the same code.
func (h *Hub) SendState(sessionID string, message protocol.PairingState) {
	h.send(sessionID, message)
}

// send delivers message to every connection of one session.
func (h *Hub) send(sessionID string, message any) {
	h.mutex.Lock()
	peers := make([]*Connection, 0, len(h.connections[sessionID]))
	for _, connection := range h.connections[sessionID] {
		peers = append(peers, connection)
	}
	h.mutex.Unlock()

	for _, connection := range peers {
		connection.Send(message)
	}
}

// Close ends every live pairing connection, so bounded shutdown does not wait
// for a reader that would otherwise block until its transport fails.
func (h *Hub) Close() {
	h.mutex.Lock()
	var connections []*Connection
	for _, peers := range h.connections {
		for _, connection := range peers {
			connections = append(connections, connection)
		}
	}
	h.mutex.Unlock()

	for _, connection := range connections {
		connection.Close(closeGoingAway)
	}
}
