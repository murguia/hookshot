# CLAUDE.md

Behavioral guidelines to reduce common LLM coding mistakes. Merge with project-specific instructions as needed.

**Tradeoff:** These guidelines bias toward caution over speed. For trivial tasks, use judgment.

## 1. Think Before Coding

**Don't assume. Don't hide confusion. Surface tradeoffs.**

Before implementing:
- State your assumptions explicitly. If uncertain, ask.
- If multiple interpretations exist, present them - don't pick silently.
- If a simpler approach exists, say so. Push back when warranted.
- If something is unclear, stop. Name what's confusing. Ask.

## 2. Simplicity First

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

Ask yourself: "Would a senior engineer say this is overcomplicated?" If yes, simplify.

## 3. Surgical Changes

**Touch only what you must. Clean up only your own mess.**

When editing existing code:
- Don't "improve" adjacent code, comments, or formatting.
- Don't refactor things that aren't broken.
- Match existing style, even if you'd do it differently.
- If you notice unrelated dead code, mention it - don't delete it.

When your changes create orphans:
- Remove imports/variables/functions that YOUR changes made unused.
- Don't remove pre-existing dead code unless asked.

The test: Every changed line should trace directly to the user's request.

## 4. Git Discipline

**Never commit or push without explicit permission.**

- Always ask before running `git commit` or `git push`
- Don't assume the user wants changes committed just because a task is complete
- Wait for the user to say "commit", "push", or similar before doing so

## 5. Goal-Driven Execution

**Define success criteria. Loop until verified.**

Transform tasks into verifiable goals:
- "Add validation" → "Write tests for invalid inputs, then make them pass"
- "Fix the bug" → "Write a test that reproduces it, then make it pass"
- "Refactor X" → "Ensure tests pass before and after"

For multi-step tasks, state a brief plan:
```
1. [Step] → verify: [check]
2. [Step] → verify: [check]
3. [Step] → verify: [check]
```

Strong success criteria let you loop independently. Weak criteria ("make it work") require constant clarification.

---

## Project: Hookshot

Real-time webhook inspector and API debugger in Go.

### Stack

- **Go** (1.26) — `net/http` stdlib router, gorilla/websocket, pgx v5
- **Postgres** — stores endpoints and captured requests (JSONB for headers)
- **WebSockets** — live fan-out of captured requests to browser viewers
- **Docker** — Dockerfile for Railway deploy, docker-compose for local dev

### Project Structure

```
main.go          — entry point, graceful shutdown
store.go         — Store interface (persistence boundary)
db.go            — *DB implements Store via pgxpool
handlers.go      — Server struct, HTTP routes, webhook capture, replay
hub.go           — WebSocket hub: rooms, fan-out, Client read/write pumps
models.go        — Endpoint, CapturedRequest structs
static/          — SPA frontend (index.html, inspect.html, style.css)
hub_test.go      — Hub concurrency tests
handlers_test.go — HTTP handler tests with MockStore
```

### Key Patterns

- **`Store` interface** is the testing seam. Handlers depend on `Store`, not `*DB`. Tests use `MockStore` (in-memory maps). Never bypass this — new DB methods must go through the interface.
- **Hub concurrency**: one goroutine per browser (WritePump + ReadPump), channels for fan-out, `sync.RWMutex` on the room map. Don't write to `*websocket.Conn` outside WritePump.
- **Webhook routes** are registered per-method (`GET /hook/...`, `POST /hook/...`, etc.) to avoid Go 1.22+ mux conflicts with method-less patterns.

### Running Tests

```bash
go test -v -race ./...
```

Tests require no external services. All 17 tests use MockStore and run in ~1.5s with the race detector.

### Running Locally

```bash
docker compose up                    # Postgres + app on :8080
# or
export DATABASE_URL="postgres://..."
go run .
```

---

**These guidelines are working if:** fewer unnecessary changes in diffs, fewer rewrites due to overcomplication, and clarifying questions come before implementation rather than after mistakes.
