package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	pool *pgxpool.Pool
}

func NewDB(ctx context.Context) (*DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_PRIVATE_URL")
	}
	if dsn == "" {
		dsn = "postgres://localhost:5432/hookshot?sslmode=disable"
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to db: %w", err)
	}

	// Retry ping — Postgres may still be starting on Railway
	for attempt := 1; attempt <= 10; attempt++ {
		if err = pool.Ping(ctx); err == nil {
			return &DB{pool: pool}, nil
		}
		log.Printf("database: ping db: %v (attempt %d/10)", err, attempt)
		time.Sleep(time.Duration(attempt) * time.Second)
	}

	return nil, fmt.Errorf("ping db: %w", err)
}

func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS endpoints (
			id TEXT PRIMARY KEY,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS requests (
			id TEXT PRIMARY KEY,
			endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
			method TEXT NOT NULL,
			path TEXT NOT NULL DEFAULT '',
			headers JSONB NOT NULL DEFAULT '{}',
			query TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL DEFAULT '',
			size BIGINT NOT NULL DEFAULT 0,
			remote_addr TEXT NOT NULL DEFAULT '',
			received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			duration_ms DOUBLE PRECISION NOT NULL DEFAULT 0
		);

		CREATE INDEX IF NOT EXISTS idx_requests_endpoint ON requests(endpoint_id, received_at DESC);
	`)
	return err
}

func (db *DB) CreateEndpoint(ctx context.Context, e *Endpoint) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO endpoints (id, created_at) VALUES ($1, $2)`,
		e.ID, e.CreatedAt,
	)
	return err
}

func (db *DB) GetEndpoint(ctx context.Context, id string) (*Endpoint, error) {
	var e Endpoint
	err := db.pool.QueryRow(ctx,
		`SELECT id, created_at FROM endpoints WHERE id = $1`, id,
	).Scan(&e.ID, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (db *DB) SaveRequest(ctx context.Context, r *CapturedRequest) error {
	headersJSON, err := json.Marshal(r.Headers)
	if err != nil {
		return err
	}
	_, err = db.pool.Exec(ctx,
		`INSERT INTO requests (id, endpoint_id, method, path, headers, query, body, size, remote_addr, received_at, duration_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		r.ID, r.EndpointID, r.Method, r.Path, headersJSON, r.Query, r.Body, r.Size, r.RemoteAddr, r.ReceivedAt, r.DurationMs,
	)
	return err
}

func (db *DB) GetRequests(ctx context.Context, endpointID string, limit int) ([]CapturedRequest, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.pool.Query(ctx,
		`SELECT id, endpoint_id, method, path, headers, query, body, size, remote_addr, received_at, duration_ms
		 FROM requests WHERE endpoint_id = $1 ORDER BY received_at DESC LIMIT $2`,
		endpointID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reqs []CapturedRequest
	for rows.Next() {
		var r CapturedRequest
		var headersJSON []byte
		err := rows.Scan(&r.ID, &r.EndpointID, &r.Method, &r.Path, &headersJSON, &r.Query, &r.Body, &r.Size, &r.RemoteAddr, &r.ReceivedAt, &r.DurationMs)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(headersJSON, &r.Headers); err != nil {
			return nil, err
		}
		reqs = append(reqs, r)
	}
	return reqs, nil
}

func (db *DB) GetRequest(ctx context.Context, id string) (*CapturedRequest, error) {
	var r CapturedRequest
	var headersJSON []byte
	err := db.pool.QueryRow(ctx,
		`SELECT id, endpoint_id, method, path, headers, query, body, size, remote_addr, received_at, duration_ms
		 FROM requests WHERE id = $1`, id,
	).Scan(&r.ID, &r.EndpointID, &r.Method, &r.Path, &headersJSON, &r.Query, &r.Body, &r.Size, &r.RemoteAddr, &r.ReceivedAt, &r.DurationMs)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(headersJSON, &r.Headers); err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *DB) Close() {
	db.pool.Close()
}
