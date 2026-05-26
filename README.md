# Reverse-Proxy

A Go reverse proxy supporting 7 protocols with load balancing, health checks, rate limiting, TLS, and structured logging.

## Supported Protocols

| Protocol | Description |
|----------|-------------|
| HTTP | HTTP reverse proxy with path-based routing |
| WebSocket | WebSocket upgrade and bidirectional message forwarding |
| TCP | Raw TCP stream proxying with bidirectional copy |
| gRPC | Transparent byte-level gRPC proxy (no proto definitions needed) |
| Unix Socket | Unix domain socket proxying (reuses TCP proxy logic) |
| UDP | UDP packet proxying |
| JSON-RPC | JSON-RPC 2.0 over TCP with proper error responses |

## Features

- **YAML Configuration** — Declarative config for listeners, backend pools, and policies
- **Load Balancing** — 4 strategies: round-robin, weighted round-robin, least connections, random
- **Health Checks** — HTTP path checks or TCP/Unix dial checks with configurable thresholds
- **Rate Limiting** — Token-bucket limiter (per-IP or global) using `golang.org/x/time/rate`
- **TLS** — TLS 1.2+ with restricted cipher suites, configurable per listener
- **Structured Logging** — `log/slog` with JSON or text output, configurable level

## Quick Start

```bash
# Build
go build -o bin/reverse-proxy ./cmd/reverse-proxy

# Run with example config
./bin/reverse-proxy -config config.example.yaml

# Or run directly
go run ./cmd/reverse-proxy -config config.example.yaml
```

## Configuration

See [`config.example.yaml`](config.example.yaml) for a full reference with all options and comments.

Minimal example:

```yaml
server:
  listen: ":8080"

backend_pools:
  - name: "web"
    balancer: "round_robin"
    health_check:
      enabled: true
      interval: "10s"
      path: "/health"
    backends:
      - url: "http://127.0.0.1:8001"
      - url: "http://127.0.0.1:8002"

logging:
  level: "info"
  format: "json"
```

Multiple protocol listeners:

```yaml
server:
  listeners:
    - name: "http-proxy"
      protocol: "http"
      listen: ":8080"
      routes:
        - match: "/api/"
          backend_pool: "api-backend"
    - name: "grpc-proxy"
      protocol: "grpc"
      listen: ":50051"
      backend_pool: "grpc-backend"
    - name: "ws-proxy"
      protocol: "websocket"
      listen: ":8081"
      routes:
        - match: "/ws/"
          backend_pool: "ws-backend"
```

### Backend URL Schemes

| Scheme | Example |
|--------|---------|
| `http://` | `http://127.0.0.1:8001` |
| `ws://` | `ws://127.0.0.1:7001` |
| `tcp://` | `tcp://127.0.0.1:3307` |
| `grpc://` | `grpc://127.0.0.1:50052` |
| `unix:` | `unix:/var/run/backend.sock` |
| `udp://` | `udp://127.0.0.1:8126` |
| `rpc://` | `rpc://127.0.0.1:9001` |

### Load Balancing Strategies

| Strategy | Key |
|----------|-----|
| Round Robin | `round_robin` |
| Weighted Round Robin | `weighted_round_robin` (uses `weight` field per backend) |
| Least Connections | `least_connections` |
| Random | `random` |

### Health Checks

- Set `path` to an HTTP endpoint (e.g. `/health`) for HTTP checks
- Leave `path` empty for TCP/Unix dial checks (connection test only)
- `unhealthy_threshold` and `healthy_threshold` control status transitions based on consecutive pass/fail counts

### Rate Limiting

```yaml
rate_limit:
  enabled: true
  requests_per_second: 100.0
  burst: 200
  per_ip: true  # true = per-client-IP; false = global limit
```

### TLS

Enable per-listener or at server level (default listener only):

```yaml
listeners:
  - name: "https-proxy"
    protocol: "http"
    listen: ":8443"
    tls:
      enabled: true
      cert_file: "/path/to/cert.pem"
      key_file: "/path/to/key.pem"
```

Minimum TLS version is 1.2 with restricted cipher suites.

## Testing

```bash
# All tests
go test ./...

# Single package
go test ./internal/proxy/
go test ./internal/balancer/
go test ./internal/config/

# Integration tests (require network)
go test ./test/integration/
```

## Dependencies

- [`gorilla/websocket`](https://github.com/gorilla/websocket) — WebSocket upgrade and framing
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) — Token bucket rate limiter
- [`google.golang.org/grpc`](https://grpc.io/) — gRPC transparent proxy
- [`gopkg.in/yaml.v3`](https://github.com/go-yaml/yaml) — YAML config parsing

## License

See [LICENSE](LICENSE).
