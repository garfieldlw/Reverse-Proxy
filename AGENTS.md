# AGENTS.md — Reverse-Proxy

## Project Overview

Go reverse proxy supporting 7 protocols: HTTP, WebSocket, TCP, gRPC, Unix socket, UDP, JSON-RPC. Features: YAML config, load balancing (4 strategies), health checks, rate limiting, TLS, structured logging, connection pooling, pre-encoded error responses.

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
config/ → YAML config loading, validation, defaults (config.go)
server/ → Server orchestrator: creates pools → balancers → limiter → health checkers → listeners (server.go)
backend/ → Pool (backend collection) + HealthChecker (periodic HTTP/TCP/Unix dial checks)
balancer/ → Balancer interface + 4 strategies: round_robin, weighted_round_robin, least_connections, random
proxy/ → Protocol-specific proxy implementations (http.go, websocket.go, tcp.go, grpc.go, udp.go, rpc.go, errors.go)
middleware/
ratelimit/ → Token-bucket rate limiter (per-IP or global) using golang.org/x/time/rate
logger/ → slog wrapper: JSON/text output, level config, request-ID helpers
tls/ → TLS config builder (min TLS 1.2, restricted cipher suites, X25519/P256 curve preferences)
test/
integration/ → E2E tests + benchmarks: YAML → Server → real request → response
benchmark/ → External load testing: backend.go (standalone HTTP backend) + bench.sh (hey/wrk/ab driver)
docs/ → benchmark-report.md, ARCHITECTURE_RESEARCH.md, superpowers/plans/ (optimization plans)
config.example.yaml → Full reference config with all protocols and options
```

## Key Patterns

- **Module path**: `github.com/garfieldlw/reverse-proxy` — use this for all imports
- **Go version**: 1.25.0
- **Logging**: `log/slog` structured logging everywhere. Pass `*slog.Logger` into constructors. Use `slog.Default()` after `logger.Init()`. Never use `log` or `fmt` for logging. `AddSource: true` only at debug level.
- **Error wrapping**: Always use `fmt.Errorf("context: %w", err)` for error chains
- **Config flow**: YAML → `config.Load()` (parse + defaults + validate) → pass `*config.Config` down
- **Concurrency**: `atomic.Pointer[[]*Backend]` for healthy backend cache in Pool (lock-free reads), `sync.Map` for rate limiter per-IP entries and gRPC client connection cache, `atomic.Int64` for connection counters and consecutive pass/fail counts, `sync.Mutex` for TCP connection pool
- **Pre-encoded error responses**: `proxy/errors.go` defines `[]byte` constants (`errBytesNoBackends`, `errBytesBadGateway`, `errBytesInternalError`, `errBytesNoHealthy`) to avoid `json.NewEncoder` allocations on error paths. `errRateLimitFmt` duplicated in both `proxy/` and `ratelimit/` packages (avoids circular import).
- **Server construction order** (in `NewServer()`): pools → balancers → limiter → health checkers → listeners. Pools must exist before balancers reference them. If listener creation fails, already-started health checkers are stopped before returning error.
- **Listener categories**: `httpServers` (HTTP + WebSocket), `streamListeners` (TCP + Socket + RPC — share accept-loop pattern), `grpcServers` (gRPC), `packetListeners` (UDP)
- **Socket proxy**: Reuses `TCPProxy` with `listenNetwork`/`dialNetwork` set to `"unix"`. `NewSocketProxy()` returns `*TCPProxy`. Backend address uses `URL.Path` (not `URL.Host`). Unix connections are NOT pooled (always dial fresh).
- **gRPC proxy**: Transparent byte-level forwarding via custom `proxyCodec` registered in `init()`. Uses `grpc.UnknownServiceHandler` — no proto definitions needed. `ForceServerCodec` + `ForceCodec` on backend streams. Caches client connections per backend address via `sync.Map` in `getClient()` (with race-handling via `LoadOrStore`). `Close()` iterates and closes all cached connections.
- **TCP proxy**: Uses `backendConnPool` (global, per-address idle pool) with `Get`/`Put` for connection reuse. Max 2 idle per backend, 30s max idle age, 15s TCP keepalive on pooled connections. `io.CopyBuffer` with pooled 4KB buffers (`copyBufPool`) instead of default `io.Copy`. `DrainBackend(addr)` closes all pooled conns for an address (called on unhealthy transitions).
- **HTTP/WS handlers**: Wrap with `recoveryMiddleware` (outer) → `limiter.Middleware` (inner) → actual handler. Rate limiting only applies to HTTP/WS; TCP, gRPC, UDP, RPC have no rate limiting. HTTP and WS proxies accept `config.TransportConfig` for HTTP transport tuning.
- **WebSocket upgrader**: `CheckOrigin` always returns `true` (allows all origins). Buffer sizes: 1024 read/write. Returns `errBytesNoHealthy` ("no healthy backends available") on 503 — differs from HTTP which returns `errBytesNoBackends` ("no backends available").
- **RPC proxy**: JSON-RPC 2.0 over TCP. Returns proper JSON-RPC error objects (with `jsonrpc`, `error`, `id` fields) when no backends available or backend unreachable. Uses `streamListener` like TCP/Socket.
- **Error responses**: JSON `{"error": "..."}` for HTTP (503 for no backends, 502 for bad gateway, 500 for panics); JSON-RPC 2.0 error objects for RPC; gRPC status codes for gRPC. All HTTP/WS error bytes are pre-encoded in `proxy/errors.go`.
- **UDP proxy**: Session-based routing — maps client addresses to backend connections so response packets route back correctly. 30s idle session timeout, cleanup every `sessionTimeout/2`. Max packet size 65535. Configurable `maxSessions` with random eviction when limit reached.
- **WeightedRoundRobin**: Stateful — uses `currentWeight` map keyed by backend `RawURL` string
- **Accept loop deadline**: Socket and RPC listeners set a 1s accept deadline for graceful shutdown; TCP listeners block on accept (closed via `ln.Close()`)

## Config (YAML)

- Example: `config.example.yaml` — always keep it in sync with `config.go` structs
- Default listener: if no `listeners` array but `server.listen` is set → auto-creates HTTP listener on that address
- If exactly one pool, default listener routes `/` to that pool (sets both `Routes` and `BackendPool` on the listener)
- Backend URL schemes: `http://`, `ws://`, `tcp://`, `grpc://`, `unix:`, `udp://`, `rpc://`
- Health check `path` empty → TCP/Unix dial check instead of HTTP
- Health checker performs initial check immediately on `Start()`, then on interval
- Duration strings: Go `time.ParseDuration` format (`"10s"`, `"5s"`, `"30s"`)
- Valid balancers: `round_robin`, `weighted_round_robin`, `least_connections`, `random`
- Valid protocols: `http`, `websocket`, `tcp`, `grpc`, `socket`, `udp`, `rpc`
- Valid log levels: `debug`, `info`, `warn`, `error`
- Valid log formats: `json`, `text`
- Valid log outputs: `stdout`, `stderr` (validated at runtime in `logger.Init()`, NOT in `config.Validate()`)
- TLS can be configured per-listener or at server level (applies to default listener only)
- gRPC TLS: wraps the `net.Listener` with `tls.NewListener` (not `grpc.Credentials`)
- `ListenerConfig.BackendPool` required for all non-HTTP/non-WebSocket protocols (tcp, grpc, socket, udp, rpc)
- Pool names must be unique — duplicate names rejected by `Validate()`
- Rate limit: `requests_per_second` must be > 0 and `burst` must be >= 1 when enabled

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
| `server.transport.max_idle_conns` | `512` |
| `server.transport.max_idle_conns_per_host` | `64` |
| `server.transport.idle_conn_timeout` | `"90s"` |
| `server.transport.dial_timeout` | `"10s"` |
| `server.udp.max_sessions` | `10000` |
| `server.udp.dial_timeout` | `"10s"` |
| `server.udp.session_timeout` | `"30s"` |

## Testing

- Unit tests colocated: `*_test.go` next to source files; benchmark tests in `*_bench_test.go` files
- Unit benchmark files: `internal/proxy/{http,websocket,grpc,sse}_bench_test.go` — per-protocol micro-benchmarks
- Integration tests in `test/integration/` — they start real `Server` instances with `httptest` backends and `net.Listen` on random ports
- E2E benchmarks: `test/integration/benchmark_test.go` (HTTP) and `test/integration/sse_ws_grpc_benchmark_test.go` (SSE, WebSocket, gRPC)
- External benchmarking: `test/benchmark/backend.go` (standalone HTTP backend with `/`, `/large`, `/{size}` endpoints) + `test/benchmark/bench.sh` (shell script driving hey/wrk/ab)
- Integration tests use hardcoded ports for UDP (`127.0.0.1:38297`) and RPC (`127.0.0.1:38298`) — may collide if run in parallel
- All tests use `t.TempDir()` for temporary config files
- No mocking framework — tests use real network connections and `httptest.NewServer`
- `TestConfigLoadFromExampleYAML` validates that `config.example.yaml` parses cleanly AND contains all 7 protocols + all 4 balancers
- Port allocation in tests/benchmarks: `net.Listen("tcp", "127.0.0.1:0")` → get addr → close → use addr (findFreePort pattern)
- Benchmarks: `b.Loop()` pattern (Go 1.24+), `b.ReportAllocs()` standard, `200ms` sleep after `srv.Start()` before requests
- Rate limit 429 response: JSON body `{"error":"rate limit exceeded","retry_after":N}` + `Retry-After` header
- `server_test.go` covers Server-level lifecycle tests

## Dependencies

- `github.com/gorilla/websocket` — WebSocket upgrade and framing
- `golang.org/x/time/rate` — token bucket rate limiter
- `google.golang.org/grpc` — gRPC transparent proxy
- `google.golang.org/protobuf` — proto message interface (used by proxyCodec for type-asserting raw bytes)
- `gopkg.in/yaml.v3` — YAML config parsing

## Gotchas

- Adding a new protocol requires changes in: `config.go` (validProtocols), `server.go` (createListener switch + new createXListener + Start/Shutdown), and a new `proxy/<protocol>.go`
- Adding a new balancer strategy requires: new file in `balancer/`, `balancer.New()` switch, `config.go` validBalancers
- Unix socket listeners: existing socket file is auto-removed on startup (`os.Remove` before `net.Listen("unix", ...)`)
- Rate limiter IP extraction: checks `X-Forwarded-For` → `X-Real-IP` → `RemoteAddr` (in that order)
- TLS min version is 1.2 with restricted cipher suites — see `tls/tls.go`. Curve preferences: `X25519, P256`.
- TLS cipher list includes both TLS 1.3 (AES/CHACHA20) and TLS 1.2 (ECDHE variants) suites
- Shutdown timeout is hardcoded to 30s in `main.go`; individual test shutdowns use 5s
- Shutdown order: stop health checkers → shutdown HTTP servers (`wg.Add`) → GracefulStop gRPC with force-stop fallback (`wg.Add`) → close stream listeners (TCP/Socket/RPC) + cancel their accept loops → close packet listeners (UDP) → `wg.Wait()` for HTTP/gRPC goroutines → drain `done` channels for stream/gRPC/UDP listener goroutines
- Health checker `Stop()` waits up to 5s for the check goroutine to finish
- Per-IP rate limiter entries are cleaned up every 10 minutes (`cleanupInterval`)
- `httputil.ReverseProxy` creates a new transport per-request, so parallel Go benchmarks can exhaust macOS ephemeral ports — use `hey`/`wrk` for real parallel throughput
- `config.example.yaml` must contain all 7 protocols and all 4 balancers or `TestConfigLoadFromExampleYAML` fails
- gRPC proxy caches client connections per backend address via `sync.Map` — connections are reused across requests
- TCP proxy pools backend connections globally (`globalPool`); Unix socket connections are never pooled
- SSE works through the HTTP proxy (httputil.ReverseProxy flushes responses) — no dedicated SSE proxy type
- `errRateLimitFmt` is defined in both `proxy/errors.go` and `ratelimit/limiter.go` to avoid circular imports
