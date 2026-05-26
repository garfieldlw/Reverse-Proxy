# AGENTS.md — Reverse-Proxy

## Project Overview

Go reverse proxy supporting 7 protocols: HTTP, WebSocket, TCP, gRPC, Unix socket, UDP, JSON-RPC.
Features: YAML config, load balancing (4 strategies), health checks, rate limiting, TLS, structured logging.

## Commands

```bash
# Build and run (config path defaults to config.yaml)
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

# Vet
go vet ./...
```

No Makefile, no Dockerfile, no CI config present. No lint or formatter config beyond Go defaults.

## Architecture

```
cmd/reverse-proxy/main.go   → entrypoint: flag parse → config.Load → logger.Init → server.NewServer → Start → signal wait → Shutdown
internal/
  config/      → YAML config loading, validation, defaults (config.go)
  server/      → Server orchestrator: creates pools, balancers, listeners, wires everything (server.go)
  backend/     → Pool (backend collection) + HealthChecker (periodic HTTP/TCP/Unix dial checks)
  balancer/    → Balancer interface + 4 strategies: round_robin, weighted_round_robin, least_connections, random
  proxy/       → Protocol-specific proxy implementations (http.go, websocket.go, tcp.go, grpc.go, udp.go, rpc.go)
  middleware/
    ratelimit/ → Token-bucket rate limiter (per-IP or global) using golang.org/x/time/rate
  logger/      → slog wrapper: JSON/text output, level config, request-ID helpers
  tls/         → TLS config builder (min TLS 1.2, restricted cipher suites)
test/integration/            → End-to-end tests: YAML → Server → real request → response
config.example.yaml          → Full reference config with all protocols and options
```

## Key Patterns

- **Module path**: `github.com/garfieldlw/reverse-proxy` — use this for all imports
- **Go version**: 1.25.0
- **Logging**: `log/slog` structured logging everywhere. Pass `*slog.Logger` into constructors. Use `slog.Default()` after `logger.Init()`. Never use `log` or `fmt` for logging.
- **Error wrapping**: Always use `fmt.Errorf("context: %w", err)` for error chains
- **Config flow**: YAML → `config.Load()` (parse + defaults + validate) → pass `*config.Config` down
- **Concurrency**: `sync.RWMutex` for pool/backend access, `atomic.Int64` for connection counters and consecutive pass/fail counts
- **Proxy construction**: `Server.NewServer()` creates pools → balancers → rate limiter → health checkers → listeners (order matters — pools must exist before balancers reference them)
- **Socket proxy**: Reuses `TCPProxy` with `listenNetwork`/`dialNetwork` set to `"unix"`. `NewSocketProxy()` returns `*TCPProxy`.
- **gRPC proxy**: Transparent byte-level forwarding via custom `proxyCodec` registered in `init()`. Uses `grpc.UnknownServiceHandler` — no proto definitions needed.
- **HTTP/WS handlers**: Wrap with `recoveryMiddleware` (outer) → `limiter.Middleware` (inner) → actual handler
- **Error responses**: JSON bodies with `{"error": "..."}` for HTTP; JSON-RPC 2.0 error objects for RPC

## Config (YAML)

- Example: `config.example.yaml` — always keep it in sync with `config.go` structs
- Default listener: if no `listeners` array but `server.listen` is set → auto-creates HTTP listener on that address
- Backend URL schemes: `http://`, `ws://`, `tcp://`, `grpc://`, `unix:`, `udp://`, `rpc://`
- Health check `path` empty → TCP/Unix dial check instead of HTTP
- Duration strings: Go `time.ParseDuration` format (`"10s"`, `"5s"`, `"30s"`)
- Valid balancers: `round_robin`, `weighted_round_robin`, `least_connections`, `random`
- Valid protocols: `http`, `websocket`, `tcp`, `grpc`, `socket`, `udp`, `rpc`

## Testing

- Unit tests colocated: `*_test.go` next to source files
- Integration tests in `test/integration/` — they start real `Server` instances with `httptest` backends and `net.Listen` on random ports
- Integration tests use hardcoded ports for UDP (38297) and RPC (38298) — may collide if run in parallel
- All tests use `t.TempDir()` for temporary config files
- No mocking framework — tests use real network connections and `httptest.NewServer`
- `TestConfigLoadFromExampleYAML` validates that `config.example.yaml` parses cleanly

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
- Shutdown timeout is hardcoded to 30s in `main.go`; individual test shutdowns use 5s
