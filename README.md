# Hookshot

Real-time webhook inspector and API debugger. Think RequestBin meets Postman, but live.

Hit your deployed URL, get a unique endpoint, POST anything to it, and watch requests appear instantly in your browser via WebSockets — headers, body, query params, timing, the lot. Replay captured requests with one click.

## Quick Start

```bash
# Local with Docker
docker compose up

# Or manually (requires Postgres)
export DATABASE_URL="postgres://user:pass@localhost:5432/hookshot?sslmode=disable"
go run .
```

Open `http://localhost:8080`, click **Create Endpoint**, and start sending requests.

## Interfaces

Hookshot is structured around a few key interfaces and types that keep the code testable and the concurrency model explicit.

### `Store` — Persistence Interface

```go
type Store interface {
    CreateEndpoint(ctx context.Context, e *Endpoint) error
    GetEndpoint(ctx context.Context, id string) (*Endpoint, error)
    SaveRequest(ctx context.Context, r *CapturedRequest) error
    GetRequests(ctx context.Context, endpointID string, limit int) ([]CapturedRequest, error)
    GetRequest(ctx context.Context, id string) (*CapturedRequest, error)
}
```

This is the boundary between HTTP handlers and the database. The production implementation is `*DB`, which wraps a `pgxpool.Pool` for Postgres. In tests, `MockStore` implements the same interface with in-memory maps, so every handler can be tested without a database.

**Why an interface?** The handlers don't care whether data lives in Postgres, SQLite, or a map. Extracting `Store` means:
- Unit tests run in milliseconds with no external dependencies
- You can swap storage backends without touching handler logic
- The mock is trivial — just maps protected by a mutex

### `Hub` — WebSocket Fan-Out

```go
type Hub struct {
    mu    sync.RWMutex
    rooms map[string]map[*Client]struct{}
}
```

The Hub is the core concurrency primitive. It groups WebSocket clients by endpoint ID into "rooms". When a webhook arrives:

1. The handler captures the request and calls `hub.Broadcast(endpointID, req)` in a new goroutine
2. Broadcast serializes the request once, then fans the bytes to every `Client.send` channel in that room
3. Each client has a dedicated `WritePump` goroutine that drains the channel and writes to the WebSocket

The `rooms` map uses `sync.RWMutex` — broadcasts take a read lock (concurrent), while register/unregister take a write lock (exclusive). Slow clients with full send channels get their messages dropped rather than blocking the broadcaster.

### `Client` — Per-Connection Goroutine Pair

```go
type Client struct {
    conn       *websocket.Conn
    endpointID string
    send       chan []byte
}
```

Each browser connection spawns exactly two goroutines:

- **`WritePump`**: Reads from `send` channel, writes to WebSocket. When the channel closes (on unregister), the goroutine exits and closes the connection.
- **`ReadPump`**: Reads from WebSocket (discards messages). When the connection drops, it calls `hub.Unregister` which triggers cleanup.

This pattern avoids concurrent writes to the WebSocket (only WritePump writes) and gives a clean shutdown path: close the channel → WritePump exits → connection closes → ReadPump exits.

### `Server` — HTTP Routing

```go
type Server struct {
    db       Store
    hub      *Hub
    upgrader websocket.Upgrader
}
```

The Server wires everything together. It depends on `Store` (not `*DB`), making it fully testable. Routes:

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/endpoints` | Create a new unique endpoint |
| `GET` | `/api/endpoints/{id}/requests` | List captured requests (JSON) |
| `ANY` | `/hook/{id}` | Capture incoming webhook |
| `ANY` | `/hook/{id}/{path...}` | Capture with sub-path |
| `POST` | `/api/requests/{id}/replay` | Replay a captured request |
| `GET` | `/ws/{id}` | WebSocket stream for live updates |
| `GET` | `/inspect/{id}` | Inspector UI |

### Data Models

```go
type Endpoint struct {
    ID        string
    CreatedAt time.Time
}

type CapturedRequest struct {
    ID         string
    EndpointID string
    Method     string
    Path       string
    Headers    map[string][]string
    Query      string
    Body       string
    Size       int64
    RemoteAddr string
    ReceivedAt time.Time
    DurationMs float64
}
```

`CapturedRequest` stores everything about an incoming webhook — the full headers as a multi-value map (matching Go's `http.Header`), raw body, query string, source IP, and capture timing. Headers are stored as JSONB in Postgres.

## Testing

```bash
# Unit tests (no Postgres required)
go test -v ./...

# With race detector
go test -race ./...
```

Tests cover:
- **Hub**: register/unregister, broadcast fan-out to correct rooms, slow client message dropping, concurrent broadcast safety, empty room broadcasting
- **Handlers**: endpoint creation, webhook capture (all HTTP methods, subpaths, headers, query strings), request listing, replay, 404s for missing endpoints/requests, ID generation uniqueness

## Deployment

### Railway

Railway auto-detects the Dockerfile. Add a Postgres plugin — it provides `DATABASE_URL` automatically. The app runs migrations on startup.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://localhost:5432/hookshot?sslmode=disable` | Postgres connection string |
| `PORT` | `8080` | HTTP listen port |

## Architecture

```
Browser ──WebSocket──▶ Hub ◀── Broadcast ◀── Webhook Handler
                       │                          │
                       │ (per-client goroutines)   │ (saves to DB)
                       ▼                          ▼
                    WritePump                   Store (Postgres)
```

One goroutine per connected browser, one per incoming webhook, channels fanning payloads to the right viewers. No shared mutable state beyond the Hub's mutex-protected room map.
