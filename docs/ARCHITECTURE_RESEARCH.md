# Go Reverse Proxy Architecture Research

Production architecture patterns from Traefik, Fabio, Tyk, and Caddy for building a feature-rich reverse proxy with multi-protocol proxying, load balancing, rate limiting, health checking, TLS termination, and YAML config.

---

## Table of Contents

1. [Architecture Overviews](#1-architecture-overviews)
2. [Protocol Multiplexing](#2-protocol-multiplexing)
3. [Middleware Chains](#3-middleware-chains)
4. [Configuration Structure](#4-configuration-structure)
5. [Load Balancing](#5-load-balancing)
6. [Rate Limiting](#6-rate-limiting)
7. [Health Checking](#7-health-checking)
8. [Key Design Patterns Worth Adopting](#8-key-design-patterns-worth-adopting)
9. [Recommended Architecture for Our Proxy](#9-recommended-architecture-for-our-proxy)

---

## 1. Architecture Overviews

### Traefik (62k+ stars)

**Core Pipeline**: EntryPoint -> Router -> Middleware Chain -> Service (Load Balancer -> Backends)

Traefik separates its architecture into **static** (startup) and **dynamic** (runtime) configuration. EntryPoints define listening sockets (TCP/UDP). Each protocol has its own handler tree — HTTP, TCP, and UDP are completely separate pipelines that share only the configuration watcher.

```
+-------------------------------------------------------------+
|                     Traefik Server                           |
|  +--------------+  +--------------+  +--------------+       |
|  | TCPEndPoint  |  | UDPEndPoint  |  | HTTPPoint    |       |
|  +------+-------+  +------+-------+  +-----+--------+       |
|         |                  |                |                |
|  +------v-------+  +------v-------+  +-----v--------+       |
|  | TCP Router   |  | UDP Router   |  | HTTP Router  |       |
|  |  + Chain     |  |  + Handler   |  |  + Chain     |       |
|  +------+-------+  +------+-------+  +-----+--------+       |
|         |                  |                |                |
|  +------v-------+  +------v-------+  +-----v--------+       |
|  | TCP Service  |  | UDP Service  |  | HTTP Service |       |
|  | (WRR LB)     |  |              |  | (WRR LB)     |       |
|  +------+-------+  +------+-------+  +-----+--------+       |
|         |                  |                |                |
|  +------v-------+  +------v-------+  +-----v--------+       |
|  | TCP Proxy    |  | UDP Proxy    |  | HTTP Proxy   |       |
|  | (bi-copy)    |  |              |  | (revproxy)   |       |
|  +--------------+  +--------------+  +--------------+       |
|                                                              |
|  +--------------------------------------------------------+ |
|  |       ConfigurationWatcher (Provider Pattern)          | |
|  |  Docker/K8s/File/Consul -> dynamic.Message -> channel  | |
|  +--------------------------------------------------------+ |
+-------------------------------------------------------------+
```

**Key source**: `pkg/server/server.go` — `Server` struct holds `TCPEntryPoints`, `UDPEntryPoints`, and `ConfigurationWatcher`.

### Fabio (7k+ stars)

**Core Pipeline**: Listener -> Routing Table (atomic swap) -> Target Selection (weighted) -> Proxy

Fabio is Consul-centric. Routes come from Consul service registrations and are stored in an atomic `Table` that can be swapped without locks on the read path. HTTP uses `httputil.ReverseProxy`; TCP uses SNI-based routing via `inetaf/tcpproxy`.

```
+--------------------------------------------+
|              Fabio Server                   |
|                                             |
|  +-------------+  +--------------------+   |
|  | HTTP/HTTPS  |  | TCP+SNI Listener   |   |
|  |  Listener   |  |  (no TLS decrypt)  |   |
|  +------+------+  +--------+-----------+   |
|         |                   |              |
|  +------v------+  +--------v-----------+   |
|  | Route Table |  | SNI Proxy          |   |
|  | (atomic     |  |  Peek ClientHello  |   |
|  |  .Value)    |  |  -> Lookup SNI     |   |
|  +------+------+  |  -> bidirectional  |   |
|         |         |    copy            |   |
|  +------v------+  +--------------------+   |
|  | httputil    |                            |
|  | .Reverse    |  +--------------------+   |
|  |   Proxy     |  | Consul Watcher     |   |
|  +-------------+  | -> atomic table    |   |
|                    |    swap            |   |
|                    +--------------------+   |
+--------------------------------------------+
```

**Key source**: `route/table.go` — `atomic.Value` for lock-free routing table swaps.

### Tyk (9k+ stars)

**Core Pipeline**: HTTP Mux -> Per-API Middleware Chain (alice) -> Forked ReverseProxy (with LB/caching/discovery) -> Backend

Tyk is an API Gateway first, reverse proxy second. It forks Go's `net/http/httputil/reverseproxy.go` to add caching, load balancing, and service discovery directly into the proxy layer. Middleware chains are built per-API-spec using `justinas/alice`.

```
+----------------------------------------------------+
|                  Tyk Gateway                        |
|                                                     |
|  +----------------------------------------------+  |
|  |  ProxyMuxer (swappable mux.Router)           |  |
|  |    +-- /api1/ -> ChainObject1                 |  |
|  |    |     +-- VersionCheck                     |  |
|  |    |     +-- CORS                             |  |
|  |    |     +-- AuthKey / JWT / OAuth            |  |
|  |    |     +-- RateLimitAndQuotaCheck           |  |
|  |    |     +-- TransformMiddleware              |  |
|  |    |     +-- TykReverseProxy (forked)         |  |
|  |    |           +-- Load Balancing             |  |
|  |    |           +-- Service Discovery          |  |
|  |    |           +-- Caching                    |  |
|  |    +-- /api2/ -> ChainObject2 ...             |  |
|  +----------------------------------------------+  |
|                                                     |
|  +--------------+  +---------------------------+   |
|  | Redis Store  |  | Health Checker (ticker)   |   |
|  | (rate limit, |  |  -> component liveness    |   |
|  |  sessions,   |  |  -> datastore connectivity|   |
|  |  caching)    |  +---------------------------+   |
|  +--------------+                                  |
+----------------------------------------------------+
```

**Key source**: `gateway/api_loader.go` — `processSpec()` builds per-API `alice.New(chainArray...).Then(DummyProxyHandler)`.

### Caddy (62k+ stars)

**Core Pipeline**: JSON Config -> App (Module) -> Server -> Route (Matcher + Handler Chain) -> reverse_proxy Handler (LB + Health Check + Circuit Breaker + Transport)

Caddy is a modular platform where everything is a `caddy.Module`. The HTTP app owns `Server` instances, each with a list of `Route`s. Routes have matchers and handler chains. The `reverse_proxy` handler is itself a module (`http.handlers.reverse_proxy`) with pluggable transports, load balancing policies, circuit breakers, and upstream sources.

```
+------------------------------------------------------+
|                  Caddy Platform                       |
|  +------------------------------------------------+  |
|  |  Config (JSON native, adapters for YAML/etc)   |  |
|  |    +-- apps: { "http": HTTPApp, "tls": TLSApp }|  |
|  +-----------------------+------------------------+  |
|                          |                           |
|  +-----------------------v------------------------+  |
|  |  HTTP App                                      |  |
|  |    +-- servers: { "srv0": Server }             |  |
|  |          +-- routes: [Route]                   |  |
|  |                +-- match: MatcherSets          |  |
|  |                +-- handle: [MiddlewareHandler] |  |
|  |                      +-- encode                |  |
|  |                      +-- templates             |  |
|  |                      +-- reverse_proxy         |  |
|  |                            +-- transport (mod) |  |
|  |                            +-- load_balancing  |  |
|  |                            |   +-- selection   |  |
|  |                            |       policy (mod)|  |
|  |                            +-- health_checks   |  |
|  |                            |   +-- active      |  |
|  |                            |   +-- passive     |  |
|  |                            +-- circuit_breaker |  |
|  |                            +-- upstreams (mod) |  |
|  +------------------------------------------------+  |
|  +------------------------------------------------+  |
|  |  Module Registry (caddy.RegisterModule)        |  |
|  |    Namespace: http.handlers.*                  |  |
|  |    Namespace: http.matchers.*                  |  |
|  |    Namespace: http.reverse_proxy.transport.*   |  |
|  |    Namespace: http.reverse_proxy.selection_*   |  |
|  +------------------------------------------------+  |
+------------------------------------------------------+
```

**Key source**: `modules.go` — `Module` interface with `CaddyModule() ModuleInfo`; `reverseproxy.go` — `Handler` struct with pluggable `Transport`, `LoadBalancing.SelectionPolicy`, `CircuitBreaker`, `DynamicUpstreams`.

---

## 2. Protocol Multiplexing

### Traefik: Separate Handler Trees Per Protocol

Traefik creates **completely independent handler trees** for HTTP, TCP, and UDP. Each has its own Router, Middleware, and Service concepts.

**TCP Handler Interface** (`pkg/tcp/handler.go`):
```go
type Handler interface {
    ServeTCP(conn WriteCloser)
}

type WriteCloser interface {
    net.Conn
    CloseWrite() error
}
```

**TCP Proxy** (`pkg/tcp/proxy.go`):
```go
func (p *Proxy) ServeTCP(conn WriteCloser) {
    // Dial backend
    backend, err := p.nextServer()
    // Bidirectional copy in two goroutines
    go io.Copy(conn, backend)       // backend -> client
    go io.Copy(backend, conn)       // client -> backend
}
```

**WebSocket**: Handled within the HTTP tree — Traefik detects `Upgrade: websocket` and lets the HTTP reverse proxy handle the hijack.

**Key insight**: Traefik's approach is the cleanest for multi-protocol support. Each protocol gets its own `Handler` interface, `Chain` (middleware), `Router`, and `Service`. This avoids the complexity of a single handler trying to multiplex protocols.

### Fabio: SNI-Based TCP Routing Without Decryption

Fabio's TCP routing is SNI-based — it peeks the TLS ClientHello to extract the hostname, then routes based on SNI without terminating TLS.

**SNI Proxy** (`proxy/tcp/sni_proxy.go`):
```go
func (s *SNIProxy) ServeTCP(in net.Conn) {
    // 1. Peek TLS ClientHello
    hello, err := s.readClientHello(in)
    // 2. Extract SNI hostname
    host := hello.ServerName
    // 3. Lookup route
    target := s.Lookup(host)
    // 4. Dial backend
    out, err := net.Dial("tcp", target)
    // 5. Replay the ClientHello bytes
    out.Write(hello.Raw)
    // 6. Bidirectional copy
    go io.Copy(out, in)
    go io.Copy(in, out)
}
```

**Key insight**: SNI-based routing is ideal for TCP+TLS passthrough where you don't need to decrypt traffic. This is a simpler model than Traefik's full TCP middleware chain.

### Tyk: HTTP-Only with WebSocket via Gorilla

Tyk is primarily HTTP-focused. WebSocket support is added via `gorilla/websocket` in the forked reverse proxy.

**Key source**: `gateway/reverse_proxy.go` — The forked `ReverseProxy` detects WebSocket upgrade requests and uses `gorilla/websocket` for the dial-and-proxy.

**Key insight**: Tyk's approach works for API gateways but doesn't support raw TCP proxying. Not suitable if you need TCP/UDP multiplexing.

### Caddy: HTTP-Centric with Module Extensibility

Caddy's reverse proxy is HTTP-only at the handler level, but its module system allows extending with custom transports. WebSocket is handled natively via HTTP upgrade detection in the `ServeHTTP` method.

**WebSocket handling** (`reverseproxy.go`):
```go
// HTTP/2 and HTTP/3 extended CONNECT for WebSocket
if r.ProtoMajor == 2 && r.Method == http.MethodConnect && r.Header.Get(":protocol") == "websocket" {
    clonedReq.Header.Del(":protocol")
    clonedReq.Method = http.MethodGet
    clonedReq.Header.Set("Upgrade", "websocket")
    clonedReq.Header.Set("Connection", "Upgrade")
}
```

Caddy also supports `StreamTimeout`, `StreamCloseDelay`, and `StreamBufferSize` for controlling bidirectional streams.

**Key insight**: Caddy's module system (`http.reverse_proxy.transport.*` namespace) allows plugging in custom transports (e.g., HTTP/2, FastCGI) without modifying core.

### Pattern Summary

| Project | HTTP | WebSocket | Raw TCP | UDP | TLS Passthrough |
|---------|------|-----------|---------|-----|-----------------|
| Traefik | Yes - Separate tree | Yes - Via HTTP upgrade | Yes - Separate tree + Handler interface | Yes - Separate tree | Yes - Via TLS config |
| Fabio | Yes - httputil.ReverseProxy | No | Yes - SNI-based routing | No | Yes - SNI peek |
| Tyk | Yes - Forked reverseproxy | Yes - gorilla/websocket | No | No | No |
| Caddy | Yes - Module handler | Yes - HTTP upgrade + H2/H3 | No (via modules) | No | Yes - Via TLS app |

**Recommendation**: Follow Traefik's pattern of **separate handler trees per protocol** with a shared configuration watcher. Add Fabio's SNI-peek pattern for TLS passthrough.

---

## 3. Middleware Chains

### Traefik: Alice-Based Middleware with Protocol-Specific Chains

Traefik uses `containous/alice` (a fork of `justinas/alice`) for HTTP middleware chaining. For TCP, it implements its own chain type.

**HTTP Middleware Builder** (`pkg/server/middleware/middlewares.go`):
```go
func (b *Builder) BuildMiddleware(ctx context, middlewareName string) (alice.Constructor, error) {
    // Maps middleware name -> constructor
    // 30+ middleware types: auth, circuitbreaker, compress, ratelimit, retry, etc.
    buildFunc, ok := b.constructors[middlewareName]
    return buildFunc(ctx)
}
```

**TCP Middleware Chain** (`pkg/tcp/chain.go`):
```go
type Constructor func(Handler) (Handler, error)

type Chain struct {
    constructors []Constructor
}

func (c Chain) Then(h Handler) (Handler, error) {
    // Wraps handlers right-to-left
    for i := len(c.constructors) - 1; i >= 0; i-- {
        h, err = c.constructors[i](h)
    }
    return h, nil
}
```

**Key insight**: Having protocol-specific middleware interfaces (HTTP: `http.Handler`, TCP: `tcp.Handler`) is essential for multi-protocol support. The chain pattern is identical — just different interfaces.

### Tyk: Per-API Middleware Chains with Alice

Tyk builds a unique middleware chain for each API definition. The chain is constructed in `processSpec()` with a fixed ordering.

**Chain Assembly** (`gateway/api_loader.go`):
```go
// Build chain array
var chainArray []alice.Constructor
var authArray []alice.Constructor

// Pre-auth middleware
gw.mwAppendEnabled(&chainArray, &VersionCheck{BaseMiddleware: baseMid.Copy()})
gw.mwAppendEnabled(&chainArray, &CORSMiddleware{BaseMiddleware: baseMid.Copy()})
gw.mwAppendEnabled(&chainArray, &IPWhiteListMiddleware{BaseMiddleware: baseMid.Copy()})
gw.mwAppendEnabled(&chainArray, &IPBlackListMiddleware{BaseMiddleware: baseMid.Copy()})
gw.mwAppendEnabled(&chainArray, &OrganizationMonitor{BaseMiddleware: baseMid.Copy()})
gw.mwAppendEnabled(&chainArray, &RequestSizeLimitMiddleware{baseMid.Copy()})

// Auth middleware (separate array for OR-wrapping)
gw.mwAppendEnabled(&authArray, &Oauth2KeyExists{baseMid.Copy()})
gw.mwAppendEnabled(&authArray, &JWTMiddleware{BaseMiddleware: baseMid.Copy()})
gw.mwAppendEnabled(&authArray, &AuthKey{baseMid.Copy()})

// Post-auth middleware
gw.mwAppendEnabled(&chainArray, &RateLimitAndQuotaCheck{BaseMiddleware: baseMid.Copy()})
gw.mwAppendEnabled(&chainArray, &TransformMiddleware{baseMid.Copy()})
gw.mwAppendEnabled(&chainArray, &RedisCacheMiddleware{BaseMiddleware: baseMid.Copy()})

// Assemble: chain = pre-auth + auth + post-auth -> proxy
chain = alice.New(chainArray...).Then(&DummyProxyHandler{SH: SuccessHandler{baseMid.Copy()}, Gw: gw})
```

**TykMiddleware Interface** (`gateway/middleware.go`):
```go
type TykMiddleware interface {
    Init()
    ProcessRequest(w http.ResponseWriter, r *http.Request, conf interface{}) (error, int)
    EnabledForSpec() bool
    Name() string
    Unload()
}
```

**Key insight**: Tyk's `mwAppendEnabled` pattern — only append middleware if `EnabledForSpec()` returns true — is a clean way to build conditional chains per API. The separation of `chainArray` (pre/post-auth) and `authArray` (auth only) enables OR-wrapping for multi-auth.

### Caddy: Route-Based Handler Chains

Caddy's middleware is expressed as handler lists within `Route` structs. Handlers are chained in order — request flows down, response flows up.

**Route Structure** (`modules/caddyhttp/routes.go`):
```go
type Route struct {
    Group       string               `json:"group,omitempty"`
    MatcherSets RawMatcherSets       `json:"match,omitempty"`
    HandlersRaw []json.RawMessage    `json:"handle,omitempty"`
    Terminal    bool                 `json:"terminal,omitempty"`
}
```

**MiddlewareHandler Interface**:
```go
type MiddlewareHandler interface {
    ServeHTTP(w http.ResponseWriter, r *http.Request, next Handler) error
    caddy.Module
}
```

**Key insight**: Caddy's `next Handler` parameter (explicit chaining) is more flexible than `alice`'s implicit wrapping. Each handler explicitly calls `next.ServeHTTP()` or terminates the chain. The `Terminal` flag on routes prevents further route evaluation.

### Fabio: No Middleware Chain

Fabio has no middleware chain concept. It's a pure L4/L7 proxy with routing only. This is by design — Fabio delegates concerns like auth and rate limiting to backend services.

### Pattern Summary

| Project | Chain Library | Protocol-Specific | Per-Route Chains | Conditional Assembly |
|---------|--------------|-------------------|------------------|---------------------|
| Traefik | alice (fork) | Yes - HTTP + TCP | Yes - Per router | Yes - Via config |
| Tyk | alice | No - HTTP only | Yes - Per API spec | Yes - mwAppendEnabled |
| Caddy | Custom (Module) | No - HTTP only | Yes - Per route | Yes - Via matchers |
| Fabio | None | N/A | No | No |

**Recommendation**: Use `alice` for HTTP middleware chains (proven in Traefik + Tyk). Implement a separate `Constructor func(Handler) (Handler, error)` pattern for TCP middleware (like Traefik). Support both config-driven and code-driven chain assembly.

---

## 4. Configuration Structure

### Traefik: Static + Dynamic Split

Traefik separates config into **static** (entrypoints, providers, log level — set at startup) and **dynamic** (routers, services, middlewares — can change at runtime).

**Dynamic Config** (`pkg/config/dynamic/config.go`):
```go
type Configuration struct {
    HTTP *HTTPConfiguration `json:"http,omitempty"`
    TCP  *TCPConfiguration  `json:"tcp,omitempty"`
    UDP  *UDPConfiguration  `json:"udp,omitempty"`
    TLS  *TLSConfiguration  `json:"tls,omitempty"`
}

type HTTPConfiguration struct {
    Routers     map[string]*Router     `json:"routers,omitempty"`
    Services    map[string]*Service    `json:"services,omitempty"`
    Middlewares map[string]*Middleware `json:"middlewares,omitempty"`
}

type TCPConfiguration struct {
    Routers     map[string]*TCPRouter     `json:"routers,omitempty"`
    Services    map[string]*TCPService    `json:"services,omitempty"`
    Middlewares map[string]*TCPMiddleware `json:"middlewares,omitempty"`
}
```

**Provider Pattern** (`pkg/server/configurationwatcher.go`):
```go
// Providers push dynamic.Message to a channel
type Message struct {
    ProviderName string
    Configuration *Configuration
}

// ConfigurationWatcher aggregates from all providers
func (c *ConfigurationWatcher) receiveConfigurations() {
    for {
        select {
        case msg := <-c.providerConfigUpdateCh:
            // Aggregate configurations from all providers
            // Apply when all providers have sent their config
        }
    }
}
```

**Key insight**: The static/dynamic split is powerful — static config requires restart, dynamic config can be hot-reloaded from files, Docker, K8s, etc. The provider pattern enables multiple config sources simultaneously.

### Caddy: JSON-Native with Config Adapters

Caddy's native config is JSON. Config adapters (Caddyfile, YAML, TOML, NGINX) convert other formats to JSON. The JSON API allows live config changes.

**Config Structure** (`caddy.go`):
```go
type Config struct {
    Admin     *AdminConfig  `json:"admin,omitempty"`
    Logging   *Logging      `json:"logging,omitempty"`
    StorageRaw json.RawMessage `json:"storage,omitempty" caddy:"namespace=caddy.storage inline_key=module"`
    AppsRaw   ModuleMap     `json:"apps,omitempty"`
}
```

**Module Lifecycle**: `New()` -> Unmarshal JSON -> `Provision()` -> `Validate()` -> Use -> `Cleanup()`

**Key insight**: Caddy's `json.RawMessage` + `caddy:"namespace=..."` struct tags enable fully modular config — any field can be filled by any module in the namespace. This is the most extensible config pattern.

### Tyk: API Definition Per Route

Tyk's config is per-API-definition. Each API has its own spec with middleware, auth, rate limits, etc.

**Key insight**: Tyk's per-API config is natural for API gateways but less suitable for a general-purpose reverse proxy.

### Fabio: Consul-Driven Routes

Fabio's routes come from Consul service tags. Config is minimal — just listener addresses and routing rules.

**Key insight**: Fabio's approach is the simplest but least flexible. Good for service discovery, bad for complex proxy config.

### Pattern Summary

| Project | Config Format | Hot Reload | Multi-Source | Protocol-Separate |
|---------|-------------|------------|-------------|-------------------|
| Traefik | YAML/TOML | Yes - Provider pattern | Yes - Docker/K8s/File/Consul | Yes - HTTP/TCP/UDP/TLS |
| Caddy | JSON (native) + adapters | Yes - JSON API | Yes - API/File/Adapters | No - HTTP only |
| Tyk | JSON/Tyk Dashboard | Yes - Via dashboard | Yes - Dashboard/File | No - HTTP only |
| Fabio | Consul tags + flags | Yes - Consul watch | No - Consul only | No - HTTP only |

**Recommendation**: Follow Traefik's static/dynamic split with YAML as the primary format. Support file-based hot reload (watch for changes). Structure config with protocol-separated sections (HTTP, TCP, UDP, TLS).

---

## 5. Load Balancing

### Traefik: Weighted Round-Robin with Health Status

**TCP WRR** (`pkg/tcp/wrr_load_balancer.go`):
```go
type WRRLoadBalancer struct {
    servers []Server
    // Health status map
}

func (w *WRRLoadBalancer) ServeTCP(conn WriteCloser) {
    server, err := w.nextServer()
    server.Handler.ServeTCP(conn)
}
```

**HTTP**: Same WRR pattern with additional strategies (round-robin, least-connections via `oxy/roundrobin`).

### Fabio: Weighted Random + Round-Robin

**Picker** (`route/picker.go`):
```go
var Picker = map[string]picker{
    "rnd": rndPicker,  // Weighted random
    "rr":  rrPicker,   // Weighted round-robin
}

func rrPicker(r *Route) *Target {
    u := r.wTargets[r.total%uint64(len(r.wTargets))]
    atomic.AddUint64(&r.total, 1)
    return u
}
```

### Caddy: Pluggable Selection Policies via Module System

**Selector Interface** (`reverseproxy.go`):
```go
type Selector interface {
    Select(UpstreamPool, *http.Request, http.ResponseWriter) *Upstream
}

type LoadBalancing struct {
    SelectionPolicyRaw json.RawMessage `json:"selection_policy,omitempty" caddy:"namespace=http.reverse_proxy.selection_policies inline_key=policy"`
    Retries            int             `json:"retries,omitempty"`
    TryDuration        caddy.Duration  `json:"try_duration,omitempty"`
    TryInterval        caddy.Duration  `json:"try_interval,omitempty"`
    RetryMatchRaw      RawMatcherSets  `json:"retry_match,omitempty"`
}
```

Built-in policies: `random`, `round_robin`, `least_conn`, `first`, `ip_hash`.

**Key insight**: Caddy's pluggable `Selector` interface with module namespacing is the most extensible. New LB policies can be added as modules without touching core.

### Tyk: Per-API Load Balancing

Tyk supports round-robin and weighted round-robin per API spec. Configured in the API definition's `Proxy.Targets` list.

### Pattern Summary

| Project | Strategies | Pluggable | Health-Aware | Retry Support |
|---------|-----------|-----------|-------------|---------------|
| Traefik | WRR | No - Hardcoded | Yes - Health status map | Yes |
| Fabio | Random, RR | No - Hardcoded | No | No |
| Caddy | Random, RR, LeastConn, IPHash | Yes - Module system | Yes - Active + Passive | Yes - TryDuration + RetryMatch |
| Tyk | RR, WRR | No - Hardcoded | Yes - Per-target | Yes |

**Recommendation**: Follow Caddy's `Selector` interface pattern — define a `LoadBalancer` interface with `Select(pool, request) *Upstream`. Implement WRR, least-connections, and random as built-in strategies. Support health status integration and retry with configurable duration/interval.

---

## 6. Rate Limiting

### Traefik: Token Bucket with Redis or In-Memory

**Rate Limiter** (`pkg/middlewares/ratelimiter/rate_limiter.go`):
```go
type rateLimiter struct {
    name          string
    rate          rate.Limit       // reqs/s
    maxDelay      time.Duration
    sourceMatcher utils.SourceExtractor
    next          http.Handler
    limiter       limiter          // Interface: Allow(ctx, token) (*Duration, error)
}

func New(ctx, next, config, name) (http.Handler, error) {
    // Source criterion: IP, header, or host
    // Two backends: Redis or in-memory
    if config.Redis != nil {
        limiter = newRedisLimiter(...)    // Distributed
    } else {
        limiter = newInMemoryRateLimiter(...)  // Local
    }
}

func (rl *rateLimiter) ServeHTTP(rw, req) {
    source, _, _ := rl.sourceMatcher.Extract(req)
    rlSource := fmt.Sprintf("%s:%s", rl.name, source)
    delay, err := rl.limiter.Allow(ctx, rlSource)
    if delay == nil {
        http.Error(rw, "No bursty traffic allowed", 429)
        return
    }
    if *delay > rl.maxDelay {
        rl.serveDelayError(ctx, rw, *delay)  // 429 with Retry-After
        return
    }
    time.Sleep(*delay)  // Wait for reservation
    rl.next.ServeHTTP(rw, req)
}
```

**Key insight**: Traefik's `limiter` interface abstraction allows swapping between in-memory (`golang.org/x/time/rate`) and Redis backends. The `maxDelay` concept (wait up to N, then reject) is a sophisticated approach that smooths traffic rather than hard-rejecting.

### Tyk: Session-Based Rate Limiting with Redis

**Rate Limit Middleware** (`gateway/mw_rate_limiting.go`):
```go
func (k *RateLimitAndQuotaCheck) ProcessRequest(w, r, _) (error, int) {
    session := ctxGetSession(r)
    rateLimitKey := ctxGetAuthToken(r)

    reason := k.Gw.SessionLimiter.ForwardMessage(r, session, rateLimitKey, quotaKey, ...)

    switch reason {
    case sessionFailRateLimit:
        // Throttle retry: sleep + re-check
        if throttleRetryLimit > 0 {
            for {
                time.Sleep(throttleInterval)
                reason = k.Gw.SessionLimiter.ForwardMessage(...)
                if reason == sessionFailNone {
                    return k.ProcessRequest(w, r, nil)  // Retry succeeded
                }
            }
        }
        return errors.New("Rate Limit Exceeded"), 429
    case sessionFailQuota:
        return errors.New("Quota exceeded"), 403
    }
}
```

**Key insight**: Tyk's rate limiting is session/key-based (per API key), not IP-based. It also supports **throttle retry** — if rate limited, the middleware can sleep and retry instead of immediately rejecting. This is useful for bursty traffic patterns.

### Caddy: Rate Limiting via External Module

Caddy's core doesn't include rate limiting. It's available via the `caddy-ratelimit` plugin, which implements a distributed rate limiter using a similar token-bucket approach.

### Pattern Summary

| Project | Algorithm | Backend | Source Criterion | Distributed |
|---------|----------|---------|-----------------|-------------|
| Traefik | Token bucket | In-memory or Redis | IP / Header / Host | Yes - Redis |
| Tyk | Token bucket + quota | Redis | API key / IP | Yes - Redis |
| Caddy | Token bucket (plugin) | In-memory or Redis | IP / Header | Yes - Redis |
| Fabio | None | N/A | N/A | N/A |

**Recommendation**: Implement a `RateLimiter` interface with `Allow(ctx, key) (delay, error)`. Support both in-memory (`golang.org/x/time/rate`) and Redis backends. Source criterion should be configurable (IP, header, custom key). Include `Retry-After` header on 429 responses.

---

## 7. Health Checking

### Traefik: Dual-Channel Active Health Checks

**ServiceHealthChecker** (`pkg/healthcheck/healthcheck.go`):
```go
type ServiceHealthChecker struct {
    balancer           StatusSetter
    config             *dynamic.ServerHealthCheck
    interval           time.Duration
    unhealthyInterval  time.Duration
    timeout            time.Duration
    healthyTargets     chan target    // Channel for healthy targets
    unhealthyTargets   chan target    // Channel for unhealthy targets
}

func (shc *ServiceHealthChecker) Launch(ctx) {
    go shc.healthcheck(ctx, shc.unhealthyTargets, shc.unhealthyInterval)
    shc.healthcheck(ctx, shc.healthyTargets, shc.interval)
}

func (shc *ServiceHealthChecker) healthcheck(ctx, targets, interval) {
    ticker := time.NewTicker(interval)
    for {
        select {
        case <-ticker.C:
            for _, target := range targets {
                up := shc.executeHealthCheck(ctx, config, target.targetURL) == nil
                shc.balancer.SetStatus(ctx, target.name, up)
                if up {
                    shc.healthyTargets <- target    // Move to healthy channel
                } else {
                    shc.unhealthyTargets <- target  // Move to unhealthy channel
                }
            }
        }
    }
}
```

**Key insight**: Traefik uses **separate channels** for healthy and unhealthy targets with **different check intervals** — healthy targets are checked less frequently than unhealthy ones. This is efficient: failing backends get faster recovery detection. Supports HTTP and gRPC health checks.

### Caddy: Active + Passive Health Checks

**HealthChecks** (`healthchecks.go`):
```go
type HealthChecks struct {
    Active  *ActiveHealthChecks  `json:"active,omitempty"`
    Passive *PassiveHealthChecks `json:"passive,omitempty"`
}

type ActiveHealthChecks struct {
    URI             string        `json:"uri,omitempty"`
    Port            int           `json:"port,omitempty"`
    Interval        caddy.Duration `json:"interval,omitempty"`  // Default: 30s
    Timeout         caddy.Duration `json:"timeout,omitempty"`   // Default: 5s
    Passes          int           `json:"passes,omitempty"`     // Consecutive passes before healthy
    Fails           int           `json:"fails,omitempty"`      // Consecutive fails before unhealthy
}

type PassiveHealthChecks struct {
    FailDuration   caddy.Duration `json:"fail_duration,omitempty"`
    MaxFails       int           `json:"max_fails,omitempty"`   // Default: 1
}
```

**Key insight**: Caddy's **passive health checks** monitor actual proxied requests for failures — no extra traffic. Active checks run on a timer. The `Passes`/`Fails` thresholds prevent flapping. Passive state is shared globally across handlers.

### Tyk: Component-Level Health Checks

**Health Check** (`gateway/health_check.go`):
```go
func (gw *Gateway) initHealthCheck(ctx) {
    go func(ctx) {
        n := gw.GetConfig().LivenessCheck.CheckDuration  // Default: 10s
        ticker := time.NewTicker(n)
        for {
            select {
            case <-ticker.C:
                gw.gatherHealthChecks()  // Check Redis, dashboard, RPC
            }
        }
    }(ctx)
}
```

**Key insight**: Tyk's health checks are gateway-component-level (Redis, dashboard, RPC), not per-upstream-backend. This is an API gateway concern, not a proxy concern.

### Pattern Summary

| Project | Active | Passive | Different Unhealthy Interval | gRPC | Flapping Protection |
|---------|--------|---------|------------------------------|------|-------------------|
| Traefik | Yes | No | Yes - unhealthyInterval | Yes | No |
| Caddy | Yes | Yes | No | No | Yes - Passes/Fails thresholds |
| Tyk | Yes (component) | No | No | No | No |
| Fabio | No | No | N/A | No | No |

**Recommendation**: Implement both active and passive health checks (Caddy pattern). For active checks, use Traefik's dual-channel pattern with different intervals for healthy/unhealthy targets. Add Caddy's `Passes`/`Fails` thresholds for flapping protection. Support HTTP and TCP health checks.

---

## 8. Key Design Patterns Worth Adopting

### Pattern 1: Protocol-Specific Handler Interfaces (Traefik)

```go
// HTTP
type HTTPHandler interface {
    ServeHTTP(w http.ResponseWriter, r *http.Request)
}

// TCP
type TCPHandler interface {
    ServeTCP(conn net.Conn)
}

// UDP
type UDPHandler interface {
    ServeUDP(conn net.PacketConn, addr net.Addr, buf []byte)
}
```

Each protocol gets its own handler interface, middleware chain, router, and service. This is cleaner than trying to force all protocols through `http.Handler`.

### Pattern 2: Middleware Chain with Constructor Pattern (Traefik + Tyk)

```go
type Constructor func(Handler) (Handler, error)

type Chain struct {
    constructors []Constructor
}

func (c Chain) Then(h Handler) (Handler, error) {
    for i := len(c.constructors) - 1; i >= 0; i-- {
        var err error
        h, err = c.constructors[i](h)
        if err != nil { return nil, err }
    }
    return h, nil
}
```

### Pattern 3: Atomic Configuration Swap (Fabio + Traefik)

```go
type RoutingTable struct {
    atomic.Value
}

func (t *RoutingTable) Update(newTable Table) {
    t.Store(newTable)  // Lock-free swap
}

func (t *RoutingTable) Lookup(host string) Route {
    table := t.Load().(Table)
    return table[host]  // No lock needed on read path
}
```

### Pattern 4: Provider Pattern for Dynamic Config (Traefik)

```go
type Provider interface {
    Provide() ([]ConfigUpdate, error)
}

// FileProvider, DockerProvider, K8sProvider, ConsulProvider...
// All push updates to a shared channel
```

### Pattern 5: Module Registration System (Caddy)

```go
type Module interface {
    CaddyModule() ModuleInfo
}

type ModuleInfo struct {
    ID  ModuleID
    New func() Module
}

// Register in init()
func init() {
    RegisterModule(Handler{})
}

// Module ID: "http.handlers.reverse_proxy"
// Namespace: "http.handlers"
```

### Pattern 6: Pluggable Load Balancing (Caddy)

```go
type Selector interface {
    Select(pool UpstreamPool, r *http.Request) *Upstream
}

// Built-in: RandomSelection, RoundRobinSelection, LeastConnSelection
// Custom: implement Selector interface + register as module
```

### Pattern 7: Dual-Channel Health Checking (Traefik)

```go
healthyTargets   chan target  // Checked at normal interval
unhealthyTargets chan target  // Checked at shorter interval

// After check:
if up {
    healthyTargets <- target
} else {
    unhealthyTargets <- target
}
```

### Pattern 8: Rate Limiter Interface with Swappable Backend (Traefik)

```go
type RateLimiter interface {
    Allow(ctx context.Context, key string) (*time.Duration, error)
}

// In-memory: golang.org/x/time/rate
// Redis: distributed rate limiting
```

### Pattern 9: SNI-Based TCP Routing Without Decryption (Fabio)

```go
func (s *SNIProxy) ServeTCP(in net.Conn) {
    hello := readClientHello(in)  // Peek TLS ClientHello
    target := s.Lookup(hello.ServerName)
    out, _ := net.Dial("tcp", target)
    out.Write(hello.Raw)  // Replay buffered bytes
    go io.Copy(out, in)
    go io.Copy(in, out)
}
```

### Pattern 10: Graceful Shutdown with Stream Delay (Caddy)

```go
StreamCloseDelay caddy.Duration  // Keep streams open during config reload
StreamTimeout    caddy.Duration  // Force-close streams after timeout
```

---

## 9. Recommended Architecture for Our Proxy

Based on the research, here's the recommended architecture:

```
+-------------------------------------------------------------+
|                   Reverse Proxy Server                       |
|                                                              |
|  +--------------------------------------------------------+ |
|  |              Config (YAML, hot-reloadable)              | |
|  |  +----------+ +----------+ +----------+ +----------+  | |
|  |  |  HTTP    | |   TCP    | |   UDP    | |   TLS    |  | |
|  |  | Routers  | | Routers  | | Routers  | | Config   |  | |
|  |  | Services | | Services | | Services | |          |  | |
|  |  | Middlew. | | Middlew. | |          | |          |  | |
|  |  +----------+ +----------+ +----------+ +----------+  | |
|  +----------------------+---------------------------------+ |
|                         | Config Watcher (file notify)     |
|  +----------------------v---------------------------------+ |
|  |              EntryPoint Manager                         | |
|  |  +----------+ +----------+ +----------+                | |
|  |  | HTTP :80 | | TCP :443 | | UDP :53  |                | |
|  |  | HTTP :443| | SNI Proxy| |          |                | |
|  |  +-----+----+ +-----+----+ +-----+----+                | |
|  +--------+-----------+-----------+------------------------+ |
|           |            |            |                        |
|  +--------v------------v------------v------------------------+|
|  |              Protocol Handler Trees                      | |
|  |                                                          | |
|  |  HTTP: Router -> Middleware Chain (alice) -> Service      | |
|  |  TCP:  Router -> TCP Chain -> Service                     | |
|  |  UDP:  Router -> Handler -> Service                       | |
|  +----------------------------------------------------------+|
|                                                             |
|  +--------------------------------------------------------+ |
|  |              Shared Infrastructure                      | |
|  |  +--------------+  +--------------+  +--------------+  | |
|  |  | Load Balancer|  |Health Checker|  | Rate Limiter |  | |
|  |  | (Selector    |  | (Active +    |  | (In-memory + |  | |
|  |  |  interface)  |  |  Passive)    |  |  Redis)      |  | |
|  |  +--------------+  +--------------+  +--------------+  | |
|  +--------------------------------------------------------+ |
+-------------------------------------------------------------+
```

### Key Design Decisions

1. **Protocol-specific handler interfaces** (Traefik pattern) — HTTP, TCP, UDP each get their own `Handler` interface and `Chain` type
2. **YAML config with hot reload** — File watcher triggers atomic config swap (Fabio pattern)
3. **Static + Dynamic config split** (Traefik pattern) — Entrypoints are static; routers/services/middlewares are dynamic
4. **`alice` for HTTP middleware** (Traefik + Tyk pattern) — Proven, simple, composable
5. **`Constructor func(Handler) (Handler, error)` for TCP middleware** (Traefik pattern)
6. **Pluggable load balancing** (Caddy pattern) — `Selector` interface with WRR, least-conn, random, IP-hash
7. **Dual-channel health checking** (Traefik pattern) + **Passive health checks** (Caddy pattern) + **Flapping protection** (Caddy pattern)
8. **Rate limiter interface with swappable backend** (Traefik pattern) — In-memory default, Redis for distributed
9. **SNI-based TCP passthrough** (Fabio pattern) — Route TCP by TLS hostname without decryption
10. **Graceful shutdown with stream delay** (Caddy pattern) — Keep connections alive during config reload

### Proposed Config Structure (YAML)

```yaml
# Static config (requires restart)
entrypoints:
  http:
    address: ":80"
  https:
    address: ":443"
    tls: true
  tcp:
    address: ":5432"

# Dynamic config (hot-reloadable)
http:
  routers:
    my-api:
      rule: "Host(`api.example.com`) && PathPrefix(`/v1`)"
      service: my-api-service
      middlewares:
        - rate-limit
        - auth
  services:
    my-api-service:
      load_balancer:
        strategy: weighted_round_robin
        health_check:
          interval: 30s
          timeout: 5s
          unhealthy_interval: 10s
          path: /health
      upstreams:
        - url: http://10.0.0.1:8080
          weight: 3
        - url: http://10.0.0.2:8080
          weight: 1
  middlewares:
    rate-limit:
      type: rate_limiter
      average: 100
      burst: 50
      source: ip
    auth:
      type: jwt
      secret: "..."

tcp:
  routers:
    postgres:
      rule: "HostSNI(`db.example.com`)"
      service: postgres-service
  services:
    postgres-service:
      load_balancer:
        strategy: round_robin
      upstreams:
        - url: "10.0.0.3:5432"
        - url: "10.0.0.4:5432"

tls:
  certificates:
    - cert: /etc/certs/api.example.com.crt
      key: /etc/certs/api.example.com.key
```

---

## Source References

| Project | Repo | Commit SHA |
|---------|------|-----------|
| Traefik | [traefik/traefik](https://github.com/traefik/traefik) | `eec68dce064f843b4317c4393aaea81b6dea31d6` |
| Fabio | [fabiolb/fabio](https://github.com/fabiolb/fabio) | `1dded71cb448b57713a686824bf8fa4ab843d6fb` |
| Tyk | [TykTechnologies/tyk](https://github.com/TykTechnologies/tyk) | `a1c3b37c8aecf7a90ff357c643b15bb09f0a6905` |
| Caddy | [caddyserver/caddy](https://github.com/caddyserver/caddy) | `44b667a79f48e6163570cd6b32fa806e12625516` |
