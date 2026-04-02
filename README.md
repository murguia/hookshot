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
| `GET` | `/api/endpoints/{id}/stream` | SSE stream for live updates |
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

## Why Go

Hookshot is a small project, but it exercises several things Go does better than most languages.

### Goroutines and channels for real-time fan-out

Each browser viewer gets two goroutines (WritePump + ReadPump). Incoming webhooks fan out to viewers through buffered channels. The `select/default` pattern in `Broadcast` drops messages for slow clients instead of blocking — a non-blocking fan-out in three lines:

```go
select {
case c.send <- data:
default:
    log.Printf("dropping message for slow client on endpoint %s", endpointID)
}
```

Doing this in Node or Python requires an async framework, a message queue, or both. In Go it's built into the language.

### `sync.RWMutex` for fine-grained locking

The Hub's room map uses a read-write lock. Broadcasts (frequent) take a read lock and run concurrently. Register/unregister (rare) take a write lock. No single-threaded event loop bottleneck.

### `errgroup` for structured concurrency

When a webhook arrives, the handler saves to the DB and broadcasts to viewers concurrently, with a shared context deadline:

```go
processCtx, processCancel := context.WithTimeout(r.Context(), 5*time.Second)
defer processCancel()

g, ctx := errgroup.WithContext(processCtx)
g.Go(func() error { return s.db.SaveRequest(ctx, captured) })
g.Go(func() error { s.hub.Broadcast(endpointID, captured); return nil })
g.Wait()
```

If the DB save is slow, the context fires and the broadcast sees it too. Two real OS-level parallel goroutines, automatic cancellation propagation, five lines.

### `context.Context` for cancellation

The replay handler uses `context.WithTimeout` composed with the HTTP request context. This means two things cancel the outbound request: the timeout firing, or the client disconnecting. The response tells you which:

```bash
# Timeout demo
curl -X POST ".../api/requests/{id}/replay?timeout=1ms"
# → {"status":"timed_out","duration_ms":1.2,"timeout":"1ms"}

# Cancellation demo (Ctrl-C mid-flight)
curl -X POST ".../api/requests/{id}/replay?timeout=30s"
# ^C → server logs: "replay xxx: cancelled (client disconnected)"
```

No separate cancellation tokens, no abort controllers. The context flows through every layer automatically.

### `http.Flusher` for SSE streaming

The `/api/endpoints/{id}/stream` endpoint type-asserts `ResponseWriter` to `http.Flusher` and streams events in real time:

```go
flusher, ok := w.(http.Flusher)
// ...
fmt.Fprintf(w, "data: %s\n\n", msg)
flusher.Flush()
```

This is Go's interface composition at work — `ResponseWriter` doesn't promise flushing, but the concrete type supports it, and a one-line type assertion unlocks it. No streaming framework needed.

### Interfaces without `implements`

The `Store` interface has five methods. `*DB` satisfies it by having those methods — no `implements` keyword, no registration. Tests swap in `MockStore` with zero ceremony. Structural typing at the package boundary.

### Stdlib HTTP server, zero framework

Go 1.22+ pattern routing (`GET /hook/{endpointID}/{path...}`) handles the entire app. No Express, no Flask, no Spring. WebSockets, SSE, REST, and static file serving — all with `net/http` and one WebSocket library.

### Built-in benchmarks

Go's `testing.B` measures throughput without external tools:

```bash
go test -bench=. -run=^$ ./...
```

```
BenchmarkWebhookCapture-8            46960   50958 ns/op   9765 B/op   112 allocs/op
BenchmarkWebhookCaptureParallel-8   168270   13251 ns/op   9610 B/op   111 allocs/op
BenchmarkBroadcast-8                130162   15985 ns/op    256 B/op     2 allocs/op
```

The parallel benchmark hits **3.8x** the sequential throughput on 8 cores — `b.RunParallel` spins up GOMAXPROCS goroutines and it just scales. No JMeter, no k6, no separate harness.

### Graceful shutdown in 10 lines

```go
quit := make(chan os.Signal, 1)
signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
<-quit

shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
defer shutdownCancel()
httpSrv.Shutdown(shutdownCtx)
```

Signal → channel → context timeout → drain. In-flight requests complete cleanly. This would be a library in most ecosystems.

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

![Architecture diagram](docs/architecture.svg?v=2)

<details>
<summary>Regenerate diagram</summary>

```bash
dot -Tsvg docs/architecture.dot -o docs/architecture.svg
```
</details>

When a webhook arrives, the handler fans out to two goroutines via `errgroup`: save to Postgres and broadcast to viewers — concurrently, under a shared context deadline. The Hub delivers to WebSocket clients via per-connection WritePump goroutines, and to SSE clients via `http.Flusher`. No shared mutable state beyond the Hub's mutex-protected room map.
