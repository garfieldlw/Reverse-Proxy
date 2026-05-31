# AGENTS.md — Reverse-Proxy

## Project Overview

Go reverse proxy supporting 7 protocols: HTTP, WebSocket, TCP, gRPC, Unix socket, UDP, JSON-RPC.
Features: YAML config, load balancing (4 strategies), health checks, rate limiting, TLS, structured logging.

## Commands

```bash
# Build and run (config flag defaults to config.yaml)
go build -o bin/reverse-proxy ./cmd/reverse-proxy
./bin/reverse-proxy -config config.yaml

# Run directly
go run ./cmd/reverse-proxy -config config.yaml

# Test all
go test ./...

# Test a single package
go test ./internal/proxy/
go test ./internal/balancer/
go test ./internal/config/

# Integration tests (require network — they spin up real listeners)
go test ./test/integration/

# Benchmarks (E2E in test/integration/, unit-level in internal/proxy/)
go test -bench=. -benchmem ./test/integration/
go test -bench=. -benchmem ./internal/proxy/

# Vet
go vet ./...
```

No Makefile, no Dockerfile, no CI config present. No lint or formatter config beyond Go defaults.

## Architecture

```
cmd/reverse-proxy/main.go → entrypoint: flag parse → config.Load → logger.Init → server.NewServer → Start → signal wait → Shutdown
internal/
  config/    → YAML config loading, validation, defaults (config.go)
  server/    → Server orchestrator: creates pools → balancers → limiter → health checkers → listeners (server.go)
  backend/   → Pool (backend collection) + HealthChecker (periodic HTTP/TCP/Unix dial checks)
  balancer/  → Balancer interface + 4 strategies: round_robin, weighted_round_robin, least_connections, random
  proxy/     → Protocol-specific proxy implementations (http.go, websocket.go, tcp.go, grpc.go, udp.go, rpc.go)
  middleware/
    ratelimit/ → Token-bucket rate limiter (per-IP or global) using golang.org/x/time/rate
  logger/    → slog wrapper: JSON/text output, level config, request-ID helpers
  tls/       → TLS config builder (min TLS 1.2, restricted cipher suites)
test/integration/ → E2E tests + benchmarks: YAML → Server → real request → response
config.example.yaml → Full reference config with all protocols and options
```

## Key Patterns

- **Module path**: `github.com/garfieldlw/reverse-proxy` — use this for all imports
- **Go version**: 1.25.0
- **Logging**: `log/slog` structured logging everywhere. Pass `*slog.Logger` into constructors. Use `slog.Default()` after `logger.Init()`. Never use `log` or `fmt` for logging. `AddSource: true` only at debug level.
- **Error wrapping**: Always use `fmt.Errorf("context: %w", err)` for error chains
- **Config flow**: YAML → `config.Load()` (parse + defaults + validate) → pass `*config.Config` down
- **Concurrency**: `sync.RWMutex` for pool/backend access, `atomic.Int64` for connection counters and consecutive pass/fail counts
- **Server construction order** (in `NewServer()`): pools → balancers → rate limiter → health checkers → listeners. Pools must exist before balancers reference them.
- **Socket proxy**: Reuses `TCPProxy` with `listenNetwork`/`dialNetwork` set to `"unix"`. `NewSocketProxy()` returns `*TCPProxy`. Backend address uses `URL.Path` (not `URL.Host`).
- **gRPC proxy**: Transparent byte-level forwarding via custom `proxyCodec` registered in `init()`. Uses `grpc.UnknownServiceHandler` — no proto definitions needed. `ForceServerCodec` + `ForceCodec` on backend streams.
- **HTTP/WS handlers**: Wrap with `recoveryMiddleware` (outer) → `limiter.Middleware` (inner) → actual handler. Rate limiting only applies to HTTP/WS; TCP, gRPC, UDP, RPC have no rate limiting.
- **Error responses**: JSON `{"error": "..."}` for HTTP (503 for no backends, 502 for bad gateway, 500 for panics); JSON-RPC 2.0 error objects for RPC; gRPC status codes for gRPC
- **UDP proxy**: Session-based routing — maps client addresses to backend connections so response packets route back correctly. 30s idle session timeout, cleanup every `sessionTimeout/2`.
- **WeightedRoundRobin**: Stateful — uses `currentWeight` map keyed by backend URL

## Config (YAML)

- Example: `config.example.yaml` — always keep it in sync with `config.go` structs
- Default listener: if no `listeners` array but `server.listen` is set → auto-creates HTTP listener on that address
- If exactly one pool, default listener routes `/` to that pool
- Backend URL schemes: `http://`, `ws://`, `tcp://`, `grpc://`, `unix:`, `udp://`, `rpc://`
- Health check `path` empty → TCP/Unix dial check instead of HTTP
- Health checker performs initial check immediately on `Start()`, then on interval
- Duration strings: Go `time.ParseDuration` format (`"10s"`, `"5s"`, `"30s"`)
- Valid balancers: `round_robin`, `weighted_round_robin`, `least_connections`, `random`
- Valid protocols: `http`, `websocket`, `tcp`, `grpc`, `socket`, `udp`, `rpc`
- Valid log levels: `debug`, `info`, `warn`, `error`
- Valid log formats: `json`, `text`
- Valid log outputs: `stdout`, `stderr`

### Config Defaults (applied when zero-valued)

| Field | Default |
|-------|---------|
| `server.listen` | `:8080` (only if no listeners configured) |
| `health_check.interval` | `10s` |
| `health_check.timeout` | `5s` |
| `health_check.unhealthy_threshold` | `3` |
| `health_check.healthy_threshold` | `2` |
| `backends[].weight` | `1` |
| `rate_limit.burst` | `200` |
| `logging.level` | `info` |
| `logging.format` | `json` |
| `logging.output` | `stdout` |

## Testing

- Unit tests colocated: `*_test.go` next to source files; benchmark tests in `*_bench_test.go` files
- Integration tests in `test/integration/` — they start real `Server` instances with `httptest` backends and `net.Listen` on random ports
- Integration tests use hardcoded ports for UDP (`127.0.0.1:38297`) and RPC (`127.0.0.1:38298`) — may collide if run in parallel
- All tests use `t.TempDir()` for temporary config files
- No mocking framework — tests use real network connections and `httptest.NewServer`
- `TestConfigLoadFromExampleYAML` validates that `config.example.yaml` parses cleanly AND contains all 7 protocols + all 4 balancers
- Port allocation in tests/benchmarks: `net.Listen("tcp", "127.0.0.1:0")` → get addr → close → use addr (findFreePort pattern)
- Benchmarks: `b.Loop()` pattern (Go 1.24+), `b.ReportAllocs()` standard, `200ms` sleep after `srv.Start()` before requests
- Rate limit 429 response: JSON body `{"error":"rate limit exceeded","retry_after":N}` + `Retry-After` header

## Dependencies

- `github.com/gorilla/websocket` — WebSocket upgrade and framing
- `golang.org/x/time/rate` — token bucket rate limiter
- `google.golang.org/grpc` — gRPC transparent proxy
- `gopkg.in/yaml.v3` — YAML config parsing

## Gotchas

- Adding a new protocol requires changes in: `config.go` (validProtocols), `server.go` (createListener switch + new createXListener + Start/Shutdown), and a new `proxy/<protocol>.go`
- Adding a new balancer strategy requires: new file in `balancer/`, `balancer.New()` switch, `config.go` validBalancers
- Unix socket listeners: existing socket file is auto-removed on startup (`os.Remove` before `net.Listen("unix", ...)`)
- Rate limiter IP extraction: checks `X-Forwarded-For` → `X-Real-IP` → `RemoteAddr` (in that order)
- TLS min version is 1.2 with restricted cipher suites — see `tls/tls.go`
- TLS cipher list includes both TLS 1.3 (AES/CHACHA20) and TLS 1.2 (ECDHE variants) suites
- Shutdown timeout is hardcoded to 30s in `main.go`; individual test shutdowns use 5s
- Shutdown order: stop health checkers → shutdown HTTP servers → GracefulStop gRPC (with force-stop fallback on timeout) → close TCP/socket/UDP/RPC listeners → wait for goroutines
- Health checker `Stop()` waits up to 5s for the check goroutine to finish
- Per-IP rate limiter entries are cleaned up every 10 minutes (`cleanupInterval`)
- `httputil.ReverseProxy` creates a new transport per-request, so parallel Go benchmarks can exhaust macOS ephemeral ports — use `hey`/`wrk` for real parallel throughput
- `config.example.yaml` must contain all 7 protocols and all 4 balancers or `TestConfigLoadFromExampleYAML` fails
