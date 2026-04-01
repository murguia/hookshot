package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- MockStore ---

type MockStore struct {
	mu        sync.RWMutex
	endpoints map[string]*Endpoint
	requests  map[string]*CapturedRequest // keyed by request ID
}

func NewMockStore() *MockStore {
	return &MockStore{
		endpoints: make(map[string]*Endpoint),
		requests:  make(map[string]*CapturedRequest),
	}
}

func (m *MockStore) CreateEndpoint(_ context.Context, e *Endpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endpoints[e.ID] = e
	return nil
}

func (m *MockStore) GetEndpoint(_ context.Context, id string) (*Endpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ep, ok := m.endpoints[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return ep, nil
}

func (m *MockStore) SaveRequest(_ context.Context, r *CapturedRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[r.ID] = r
	return nil
}

func (m *MockStore) GetRequests(_ context.Context, endpointID string, limit int) ([]CapturedRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []CapturedRequest
	for _, r := range m.requests {
		if r.EndpointID == endpointID {
			out = append(out, *r)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MockStore) GetRequest(_ context.Context, id string) (*CapturedRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.requests[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return r, nil
}

// --- Tests ---

func newTestServer() (*Server, *MockStore) {
	store := NewMockStore()
	hub := NewHub()
	srv := NewServer(store, hub)
	return srv, store
}

func TestCreateEndpoint(t *testing.T) {
	srv, store := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/endpoints", "", nil)
	if err != nil {
		t.Fatalf("POST /api/endpoints: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var ep Endpoint
	if err := json.NewDecoder(resp.Body).Decode(&ep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ep.ID == "" {
		t.Fatal("expected non-empty endpoint ID")
	}
	if ep.CreatedAt.IsZero() {
		t.Fatal("expected non-zero created_at")
	}

	// Verify it's in the store
	store.mu.RLock()
	_, ok := store.endpoints[ep.ID]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("endpoint not saved to store")
	}
}

func TestWebhookCapture(t *testing.T) {
	srv, store := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// Create an endpoint first
	epID := "test-ep-1"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.mu.Unlock()

	// POST a webhook
	body := `{"event":"push","repo":"hookshot"}`
	resp, err := http.Post(ts.URL+"/hook/"+epID+"?ref=main", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /hook: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "captured" {
		t.Fatalf("expected status=captured, got %s", result["status"])
	}
	if result["id"] == "" {
		t.Fatal("expected non-empty captured request ID")
	}

	// Verify stored
	store.mu.RLock()
	captured := store.requests[result["id"]]
	store.mu.RUnlock()

	if captured == nil {
		t.Fatal("request not saved to store")
	}
	if captured.Method != "POST" {
		t.Fatalf("expected POST, got %s", captured.Method)
	}
	if captured.Body != body {
		t.Fatalf("expected body %q, got %q", body, captured.Body)
	}
	if captured.Query != "ref=main" {
		t.Fatalf("expected query ref=main, got %q", captured.Query)
	}
	if captured.EndpointID != epID {
		t.Fatalf("expected endpoint_id %s, got %s", epID, captured.EndpointID)
	}
}

func TestWebhookNotFound(t *testing.T) {
	srv, _ := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/hook/nonexistent", "text/plain", strings.NewReader("hi"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestWebhookAllMethods(t *testing.T) {
	srv, store := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	epID := "method-test"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.mu.Unlock()

	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	for _, method := range methods {
		req, _ := http.NewRequest(method, ts.URL+"/hook/"+epID, strings.NewReader(""))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", method, resp.StatusCode)
		}
	}

	// All 5 methods should be captured
	store.mu.RLock()
	count := 0
	for _, r := range store.requests {
		if r.EndpointID == epID {
			count++
		}
	}
	store.mu.RUnlock()
	if count != len(methods) {
		t.Fatalf("expected %d captured requests, got %d", len(methods), count)
	}
}

func TestWebhookWithSubpath(t *testing.T) {
	srv, store := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	epID := "subpath-ep"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.mu.Unlock()

	resp, err := http.Post(ts.URL+"/hook/"+epID+"/events/push", "text/plain", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Find the captured request and check path
	store.mu.RLock()
	var found *CapturedRequest
	for _, r := range store.requests {
		if r.EndpointID == epID {
			found = r
			break
		}
	}
	store.mu.RUnlock()

	if found == nil {
		t.Fatal("request not captured")
	}
	if found.Path != "/events/push" {
		t.Fatalf("expected path /events/push, got %s", found.Path)
	}
}

func TestGetRequests(t *testing.T) {
	srv, store := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	epID := "list-ep"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.requests["r1"] = &CapturedRequest{ID: "r1", EndpointID: epID, Method: "POST", ReceivedAt: time.Now()}
	store.requests["r2"] = &CapturedRequest{ID: "r2", EndpointID: epID, Method: "GET", ReceivedAt: time.Now()}
	store.requests["r3"] = &CapturedRequest{ID: "r3", EndpointID: "other", Method: "POST", ReceivedAt: time.Now()}
	store.mu.Unlock()

	resp, err := http.Get(ts.URL + "/api/endpoints/" + epID + "/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var reqs []CapturedRequest
	json.NewDecoder(resp.Body).Decode(&reqs)
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests for endpoint, got %d", len(reqs))
	}
}

func TestGetRequestsEmpty(t *testing.T) {
	srv, store := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	epID := "empty-ep"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.mu.Unlock()

	resp, err := http.Get(ts.URL + "/api/endpoints/" + epID + "/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var reqs []CapturedRequest
	json.NewDecoder(resp.Body).Decode(&reqs)
	if reqs == nil || len(reqs) != 0 {
		t.Fatalf("expected empty array, got %v", reqs)
	}
}

func TestReplay(t *testing.T) {
	srv, store := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	epID := "replay-ep"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.requests["orig"] = &CapturedRequest{
		ID:         "orig",
		EndpointID: epID,
		Method:     "POST",
		Path:       "/",
		Headers:    map[string][]string{"Content-Type": {"application/json"}, "X-Custom": {"hello"}},
		Body:       `{"replayed":true}`,
		ReceivedAt: time.Now(),
	}
	store.mu.Unlock()

	resp, err := http.Post(ts.URL+"/api/requests/orig/replay", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "replayed" {
		t.Fatalf("expected replayed status, got %v", result["status"])
	}

	// The replay should have created a new captured request
	time.Sleep(50 * time.Millisecond) // brief wait for async save
	store.mu.RLock()
	count := 0
	for _, r := range store.requests {
		if r.EndpointID == epID {
			count++
		}
	}
	store.mu.RUnlock()
	if count < 2 {
		t.Fatalf("expected at least 2 requests (original + replayed), got %d", count)
	}
}

func TestReplayNotFound(t *testing.T) {
	srv, _ := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/requests/nonexistent/replay", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestGenerateID(t *testing.T) {
	id1 := generateID(8)
	id2 := generateID(8)

	if len(id1) != 16 { // 8 bytes = 16 hex chars
		t.Fatalf("expected 16 char ID, got %d: %s", len(id1), id1)
	}
	if id1 == id2 {
		t.Fatal("expected unique IDs")
	}
}

func TestWebhookHeaders(t *testing.T) {
	srv, store := newTestServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	epID := "header-ep"
	store.mu.Lock()
	store.endpoints[epID] = &Endpoint{ID: epID, CreatedAt: time.Now()}
	store.mu.Unlock()

	req, _ := http.NewRequest("POST", ts.URL+"/hook/"+epID, strings.NewReader("test"))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature", "sha256=abc123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var result map[string]string
	// Find captured request
	store.mu.RLock()
	var captured *CapturedRequest
	for _, r := range store.requests {
		if r.EndpointID == epID {
			captured = r
			break
		}
	}
	store.mu.RUnlock()
	_ = result

	if captured == nil {
		t.Fatal("no captured request found")
	}
	if captured.Headers["X-Github-Event"] == nil && captured.Headers["X-GitHub-Event"] == nil {
		// Go's http canonicalizes headers
		t.Fatal("expected X-Github-Event header to be captured")
	}
}
