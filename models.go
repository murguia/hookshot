package main

import "time"

type Endpoint struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

type CapturedRequest struct {
	ID         string              `json:"id"`
	EndpointID string              `json:"endpoint_id"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Headers    map[string][]string `json:"headers"`
	Query      string              `json:"query"`
	Body       string              `json:"body"`
	Size       int64               `json:"size"`
	RemoteAddr string              `json:"remote_addr"`
	ReceivedAt time.Time           `json:"received_at"`
	DurationMs float64             `json:"duration_ms"`
}
