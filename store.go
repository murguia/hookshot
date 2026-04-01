package main

import "context"

// Store is the persistence interface that handlers depend on.
// The real implementation is *DB (backed by Postgres).
// Tests use MockStore to avoid needing a database.
type Store interface {
	CreateEndpoint(ctx context.Context, e *Endpoint) error
	GetEndpoint(ctx context.Context, id string) (*Endpoint, error)
	SaveRequest(ctx context.Context, r *CapturedRequest) error
	GetRequests(ctx context.Context, endpointID string, limit int) ([]CapturedRequest, error)
	GetRequest(ctx context.Context, id string) (*CapturedRequest, error)
}
