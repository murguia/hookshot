package main

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

// Hub manages WebSocket connections grouped by endpoint ID.
// One goroutine per browser connection, channels fan out payloads.
type Hub struct {
	mu    sync.RWMutex
	rooms map[string]map[*Client]struct{}
}

type Client struct {
	conn       *websocket.Conn
	endpointID string
	send       chan []byte
}

func NewHub() *Hub {
	return &Hub{
		rooms: make(map[string]map[*Client]struct{}),
	}
}

func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[c.endpointID] == nil {
		h.rooms[c.endpointID] = make(map[*Client]struct{})
	}
	h.rooms[c.endpointID][c] = struct{}{}
	log.Printf("client connected to endpoint %s (%d viewers)", c.endpointID, len(h.rooms[c.endpointID]))
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if clients, ok := h.rooms[c.endpointID]; ok {
		delete(clients, c)
		if len(clients) == 0 {
			delete(h.rooms, c.endpointID)
		}
	}
	close(c.send)
}

// Broadcast sends a captured request to all viewers of that endpoint.
func (h *Hub) Broadcast(endpointID string, req *CapturedRequest) {
	data, err := json.Marshal(req)
	if err != nil {
		log.Printf("marshal broadcast: %v", err)
		return
	}

	h.mu.RLock()
	clients := h.rooms[endpointID]
	h.mu.RUnlock()

	for c := range clients {
		select {
		case c.send <- data:
		default:
			// slow client, drop message
			log.Printf("dropping message for slow client on endpoint %s", endpointID)
		}
	}
}

// WritePump runs as a goroutine per client, draining the send channel.
func (c *Client) WritePump() {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

// ReadPump runs as a goroutine per client, reading (and discarding) messages.
// When the connection closes, it unregisters from the hub.
func (c *Client) ReadPump(hub *Hub) {
	defer hub.Unregister(c)
	defer c.conn.Close()
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}
