package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestHub_RegisterUnregister(t *testing.T) {
	hub := NewHub()

	c := &Client{endpointID: "ep1", send: make(chan []byte, 8)}
	hub.Register(c)

	hub.mu.RLock()
	if len(hub.rooms["ep1"]) != 1 {
		t.Fatalf("expected 1 client in room ep1, got %d", len(hub.rooms["ep1"]))
	}
	hub.mu.RUnlock()

	hub.Unregister(c)

	hub.mu.RLock()
	if _, ok := hub.rooms["ep1"]; ok {
		t.Fatal("expected room ep1 to be cleaned up after last client unregisters")
	}
	hub.mu.RUnlock()
}

func TestHub_MultipleClientsPerEndpoint(t *testing.T) {
	hub := NewHub()

	c1 := &Client{endpointID: "ep1", send: make(chan []byte, 8)}
	c2 := &Client{endpointID: "ep1", send: make(chan []byte, 8)}
	c3 := &Client{endpointID: "ep2", send: make(chan []byte, 8)}

	hub.Register(c1)
	hub.Register(c2)
	hub.Register(c3)

	hub.mu.RLock()
	if len(hub.rooms["ep1"]) != 2 {
		t.Fatalf("expected 2 clients in ep1, got %d", len(hub.rooms["ep1"]))
	}
	if len(hub.rooms["ep2"]) != 1 {
		t.Fatalf("expected 1 client in ep2, got %d", len(hub.rooms["ep2"]))
	}
	hub.mu.RUnlock()

	// Unregister one from ep1 — room should remain
	hub.Unregister(c1)
	hub.mu.RLock()
	if len(hub.rooms["ep1"]) != 1 {
		t.Fatalf("expected 1 client remaining in ep1, got %d", len(hub.rooms["ep1"]))
	}
	hub.mu.RUnlock()
}

func TestHub_BroadcastDeliversToCorrectEndpoint(t *testing.T) {
	hub := NewHub()

	viewer1 := &Client{endpointID: "ep1", send: make(chan []byte, 8)}
	viewer2 := &Client{endpointID: "ep1", send: make(chan []byte, 8)}
	other := &Client{endpointID: "ep2", send: make(chan []byte, 8)}

	hub.Register(viewer1)
	hub.Register(viewer2)
	hub.Register(other)

	req := &CapturedRequest{
		ID:         "req1",
		EndpointID: "ep1",
		Method:     "POST",
		Path:       "/webhook",
		ReceivedAt: time.Now(),
	}

	hub.Broadcast("ep1", req)

	// Both ep1 viewers should receive
	for _, v := range []*Client{viewer1, viewer2} {
		select {
		case msg := <-v.send:
			var got CapturedRequest
			if err := json.Unmarshal(msg, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.ID != "req1" {
				t.Fatalf("expected req1, got %s", got.ID)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("viewer did not receive broadcast in time")
		}
	}

	// ep2 viewer should NOT receive
	select {
	case <-other.send:
		t.Fatal("ep2 viewer should not receive ep1 broadcast")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestHub_BroadcastDropsSlowClient(t *testing.T) {
	hub := NewHub()

	// Channel size 1 — fill it up, then broadcast should drop
	slow := &Client{endpointID: "ep1", send: make(chan []byte, 1)}
	hub.Register(slow)

	req := &CapturedRequest{ID: "r1", EndpointID: "ep1", Method: "POST", ReceivedAt: time.Now()}

	// First broadcast fills the buffer
	hub.Broadcast("ep1", req)
	// Second should be dropped (not block)
	hub.Broadcast("ep1", &CapturedRequest{ID: "r2", EndpointID: "ep1", Method: "POST", ReceivedAt: time.Now()})

	msg := <-slow.send
	var got CapturedRequest
	json.Unmarshal(msg, &got)
	if got.ID != "r1" {
		t.Fatalf("expected r1, got %s", got.ID)
	}

	// Channel should be empty — r2 was dropped
	select {
	case <-slow.send:
		t.Fatal("expected r2 to be dropped for slow client")
	default:
		// expected
	}
}

func TestHub_ConcurrentBroadcast(t *testing.T) {
	hub := NewHub()
	const numClients = 10
	const numBroadcasts = 50

	clients := make([]*Client, numClients)
	for i := range clients {
		clients[i] = &Client{endpointID: "ep1", send: make(chan []byte, numBroadcasts+10)}
		hub.Register(clients[i])
	}

	var wg sync.WaitGroup
	for i := 0; i < numBroadcasts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hub.Broadcast("ep1", &CapturedRequest{
				ID:         generateID(4),
				EndpointID: "ep1",
				Method:     "POST",
				ReceivedAt: time.Now(),
			})
		}(i)
	}
	wg.Wait()

	for i, c := range clients {
		if len(c.send) != numBroadcasts {
			t.Errorf("client %d: expected %d messages, got %d", i, numBroadcasts, len(c.send))
		}
	}
}

func TestHub_BroadcastToEmptyRoom(t *testing.T) {
	hub := NewHub()
	// Should not panic
	hub.Broadcast("nonexistent", &CapturedRequest{ID: "r1", Method: "GET", ReceivedAt: time.Now()})
}
