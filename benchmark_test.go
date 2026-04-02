package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func benchClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 256,
			DisableKeepAlives:   false,
		},
	}
}

func BenchmarkWebhookCapture(b *testing.B) {
	store := NewMockStore()
	hub := NewHub()
	srv := NewServer(store, hub)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	epID := "bench-ep"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.mu.Unlock()

	body := `{"event":"order.created","id":1234,"ts":"2024-01-01T00:00:00Z"}`
	url := ts.URL + "/hook/" + epID
	client := benchClient()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("POST", url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkWebhookCaptureParallel(b *testing.B) {
	store := NewMockStore()
	hub := NewHub()
	srv := NewServer(store, hub)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	epID := "bench-ep-par"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.mu.Unlock()

	body := `{"event":"order.created","id":1234,"ts":"2024-01-01T00:00:00Z"}`
	url := ts.URL + "/hook/" + epID
	client := benchClient()

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", url, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkBroadcast(b *testing.B) {
	hub := NewHub()
	// Simulate 50 connected viewers
	clients := make([]*Client, 50)
	for i := range clients {
		clients[i] = &Client{endpointID: "ep1", send: make(chan []byte, 256)}
		hub.Register(clients[i])
	}
	// Drain channels in background
	done := make(chan struct{})
	defer close(done)
	for _, c := range clients {
		go func(c *Client) {
			for {
				select {
				case <-c.send:
				case <-done:
					return
				}
			}
		}(c)
	}

	req := &CapturedRequest{
		ID: "r1", EndpointID: "ep1", Method: "POST",
		Path: "/", Body: `{"test":true}`, ReceivedAt: time.Now(),
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		hub.Broadcast("ep1", req)
	}
}
