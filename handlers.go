package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
)

type Server struct {
	db       Store
	hub      *Hub
	upgrader websocket.Upgrader
}

func NewServer(db Store, hub *Hub) *Server {
	return &Server{
		db:  db,
		hub: hub,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Static assets
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// Pages
	mux.HandleFunc("GET /", s.handleHome)
	mux.HandleFunc("GET /inspect/{endpointID}", s.handleInspect)

	// API
	mux.HandleFunc("POST /api/endpoints", s.handleCreateEndpoint)
	mux.HandleFunc("GET /api/endpoints/{endpointID}/requests", s.handleGetRequests)
	mux.HandleFunc("POST /api/requests/{requestID}/replay", s.handleReplay)

	// WebSocket
	mux.HandleFunc("GET /ws/{endpointID}", s.handleWS)

	// SSE stream (demonstrates http.Flusher)
	mux.HandleFunc("GET /api/endpoints/{endpointID}/stream", s.handleSSE)

	// Webhook catch-all: register each method explicitly to avoid conflicts
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		mux.HandleFunc(m+" /hook/{endpointID}", s.handleWebhook)
		mux.HandleFunc(m+" /hook/{endpointID}/{path...}", s.handleWebhook)
	}

	return mux
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "static/index.html")
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	endpointID := r.PathValue("endpointID")
	_, err := s.db.GetEndpoint(r.Context(), endpointID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "static/inspect.html")
}

func (s *Server) handleCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	id := generateID(8)
	ep := &Endpoint{
		ID:        id,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.db.CreateEndpoint(r.Context(), ep); err != nil {
		http.Error(w, "failed to create endpoint", http.StatusInternalServerError)
		log.Printf("create endpoint: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(ep)
}

func (s *Server) handleGetRequests(w http.ResponseWriter, r *http.Request) {
	endpointID := r.PathValue("endpointID")
	reqs, err := s.db.GetRequests(r.Context(), endpointID, 100)
	if err != nil {
		http.Error(w, "failed to get requests", http.StatusInternalServerError)
		log.Printf("get requests: %v", err)
		return
	}
	if reqs == nil {
		reqs = []CapturedRequest{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reqs)
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	endpointID := r.PathValue("endpointID")

	_, err := s.db.GetEndpoint(r.Context(), endpointID)
	if err != nil {
		http.Error(w, "endpoint not found", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	subPath := r.PathValue("path")
	captured := &CapturedRequest{
		ID:         generateID(12),
		EndpointID: endpointID,
		Method:     r.Method,
		Path:       "/" + subPath,
		Headers:    r.Header,
		Query:      r.URL.RawQuery,
		Body:       string(body),
		Size:       int64(len(body)),
		RemoteAddr: r.RemoteAddr,
		ReceivedAt: start.UTC(),
		DurationMs: float64(time.Since(start).Microseconds()) / 1000.0,
	}

	// Fan out with a deadline: if DB save or broadcast exceeds 5s, the
	// context is cancelled and the other goroutine sees it too.
	processCtx, processCancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer processCancel()

	g, ctx := errgroup.WithContext(processCtx)
	g.Go(func() error {
		if err := s.db.SaveRequest(ctx, captured); err != nil {
			log.Printf("save request: %v", err)
		}
		return nil
	})
	g.Go(func() error {
		s.hub.Broadcast(endpointID, captured)
		return nil
	})
	g.Wait()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"captured","id":"%s"}`, captured.ID)
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("requestID")
	captured, err := s.db.GetRequest(r.Context(), requestID)
	if err != nil {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	// Build the replay URL (send it back to ourselves)
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	target := fmt.Sprintf("%s://%s/hook/%s%s", scheme, r.Host, captured.EndpointID, captured.Path)
	if captured.Query != "" {
		target += "?" + captured.Query
	}

	// Context-based timeout: composes with the client's connection context.
	// If the caller disconnects OR the timeout fires, the outbound request
	// is cancelled automatically — no separate http.Client.Timeout needed.
	timeout := 10 * time.Second
	if t, err := time.ParseDuration(r.URL.Query().Get("timeout")); err == nil && t > 0 {
		timeout = t
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, captured.Method, target, strings.NewReader(captured.Body))
	if err != nil {
		http.Error(w, "failed to build replay request", http.StatusInternalServerError)
		return
	}

	// Restore original headers (skip hop-by-hop)
	skip := map[string]bool{"Host": true, "Connection": true, "Transfer-Encoding": true}
	for k, vals := range captured.Headers {
		if skip[k] {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("X-Hookshot-Replay", "true")

	resp, err := http.DefaultClient.Do(req)
	elapsed := float64(time.Since(start).Microseconds()) / 1000.0

	if err != nil {
		result := map[string]any{"status": "failed", "duration_ms": elapsed}
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			result["status"] = "timed_out"
			result["timeout"] = timeout.String()
			log.Printf("replay %s: timed out after %s", requestID, timeout)
		case ctx.Err() == context.Canceled:
			result["status"] = "cancelled"
			log.Printf("replay %s: cancelled (client disconnected)", requestID)
		default:
			result["error"] = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		json.NewEncoder(w).Encode(result)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":      "replayed",
		"status_code": resp.StatusCode,
		"duration_ms": elapsed,
	})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	endpointID := r.PathValue("endpointID")
	if _, err := s.db.GetEndpoint(r.Context(), endpointID); err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	// Reuse the Hub's room infrastructure — SSE client is just a Client
	// whose send channel we drain here instead of in WritePump.
	client := &Client{
		conn:       nil, // no websocket conn
		endpointID: endpointID,
		send:       make(chan []byte, 64),
	}
	s.hub.Register(client)
	defer s.hub.Unregister(client)

	for {
		select {
		case msg, ok := <-client.send:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	endpointID := r.PathValue("endpointID")

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}

	client := &Client{
		conn:       conn,
		endpointID: endpointID,
		send:       make(chan []byte, 64),
	}

	s.hub.Register(client)
	go client.WritePump()
	go client.ReadPump(s.hub)
}

func generateID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
