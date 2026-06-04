package proxy

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/garfieldlw/reverse-proxy/internal/middleware/ratelimit"
)

// contextKey is used for per-request context values in the reverse proxy.
type contextKey string

const (
	backendKey   contextKey = "selected_backend"
	startTimeKey contextKey = "start_time"
)

// HTTPProxy is an HTTP reverse proxy that load balances across backends.
type HTTPProxy struct {
	pool         *backend.Pool
	balancer     balancer.Balancer
	limiter      *ratelimit.Limiter
	logger       *slog.Logger
	reverseProxy *httputil.ReverseProxy
	transport    *http.Transport
}

// NewHTTPProxy creates a new HTTP reverse proxy handler.
func NewHTTPProxy(pool *backend.Pool, bal balancer.Balancer, limiter *ratelimit.Limiter, logger *slog.Logger, transportCfg config.TransportConfig) *HTTPProxy {
	idleTimeout, _ := time.ParseDuration(transportCfg.IdleConnTimeout)
	dialTimeout, _ := time.ParseDuration(transportCfg.DialTimeout)
	if idleTimeout == 0 {
		idleTimeout = 90 * time.Second
	}
	if dialTimeout == 0 {
		dialTimeout = 10 * time.Second
	}

	p := &HTTPProxy{
		pool:     pool,
		balancer: bal,
		limiter:  limiter,
		logger:   logger,
		transport: &http.Transport{
			MaxIdleConns:        transportCfg.MaxIdleConns,
			MaxIdleConnsPerHost: transportCfg.MaxIdleConnsPerHost,
			IdleConnTimeout:     idleTimeout,
			DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
		},
	}

	p.reverseProxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			b := req.Context().Value(backendKey).(*backend.Backend)
			req.URL.Scheme = b.URL.Scheme
			req.URL.Host = b.URL.Host
			req.URL.Path = singleJoiningSlash(b.URL.Path, req.URL.Path)
			req.URL.RawPath = ""
			req.Host = b.URL.Host

			// Forward original Host as X-Forwarded-Host.
			if req.Header.Get("X-Forwarded-Host") == "" {
				req.Header.Set("X-Forwarded-Host", req.Host)
			}

			// Add X-Forwarded-For with client IP.
			clientIP, _, ipErr := net.SplitHostPort(req.RemoteAddr)
			if ipErr != nil {
				clientIP = req.RemoteAddr
			}
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			start := resp.Request.Context().Value(startTimeKey).(time.Time)
			b := resp.Request.Context().Value(backendKey).(*backend.Backend)
			p.logger.Info("proxy request",
				"method", resp.Request.Method,
				"path", resp.Request.URL.Path,
				"upstream", b.RawURL,
				"status", resp.StatusCode,
				"duration", time.Since(start),
			)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			b := req.Context().Value(backendKey).(*backend.Backend)
			p.logger.Error("proxy error",
				"method", req.Method,
				"path", req.URL.Path,
				"upstream", b.RawURL,
				"error", err,
			)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		w.Write(errBytesBadGateway)
		},
	}

	p.reverseProxy.Transport = p.transport

	return p
}

// ServeHTTP handles HTTP requests by selecting a healthy backend and proxying.
func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	healthy := p.pool.GetHealthyBackends()
	b, err := p.balancer.Select(healthy)
	if err != nil {
		p.logger.Error("no backends available", "method", r.Method, "path", r.URL.Path, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(errBytesNoBackends)
		return
	}

	b.IncConns()
	defer b.DecConns()

	ctx := context.WithValue(r.Context(), backendKey, b)
	ctx = context.WithValue(ctx, startTimeKey, time.Now())
	p.reverseProxy.ServeHTTP(w, r.WithContext(ctx))
}

// Handler returns an http.Handler that wraps the proxy with recovery and rate limiting.
func (p *HTTPProxy) Handler() http.Handler {
	handler := http.HandlerFunc(p.ServeHTTP)

	// Apply rate limiting middleware if limiter is provided and enabled.
	var final http.Handler = handler
	if p.limiter != nil {
		final = p.limiter.Middleware(final)
	}

	// Apply recovery middleware (outermost — catches panics from everything).
	return recoveryMiddleware(final, p.logger)
}

// recoveryMiddleware recovers from panics in HTTP handlers.
func recoveryMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered", "error", rec)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write(errBytesInternalError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// singleJoiningSlash joins two path segments with exactly one slash between them.
// This mirrors the unexported singleJoiningSlash in httputil.ReverseProxy.
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
