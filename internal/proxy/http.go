package proxy

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"github.com/garfieldlw/reverse-proxy/internal/middleware/ratelimit"
)

// HTTPProxy is an HTTP reverse proxy that load balances across backends.
type HTTPProxy struct {
	pool     *backend.Pool
	balancer balancer.Balancer
	limiter  *ratelimit.Limiter
	logger   *slog.Logger
}

// NewHTTPProxy creates a new HTTP reverse proxy handler.
func NewHTTPProxy(pool *backend.Pool, balancer balancer.Balancer, limiter *ratelimit.Limiter, logger *slog.Logger) *HTTPProxy {
	return &HTTPProxy{
		pool:     pool,
		balancer: balancer,
		limiter:  limiter,
		logger:   logger,
	}
}

// ServeHTTP handles HTTP requests by selecting a healthy backend and proxying.
func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	healthy := p.pool.GetHealthyBackends()
	b, err := p.balancer.Select(healthy)
	if err != nil {
		if errors.Is(err, balancer.ErrNoBackends) {
			p.logger.Error("no backends available", "method", r.Method, "path", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "no backends available",
			})
			return
		}
		p.logger.Error("backend selection failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "no backends available",
		})
		return
	}

	b.IncConns()
	defer b.DecConns()

	start := time.Now()

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = b.URL.Scheme
			req.URL.Host = b.URL.Host
			req.URL.Path = singleJoiningSlash(b.URL.Path, req.URL.Path)
			req.URL.RawPath = ""
			req.Host = b.URL.Host

			// Forward original Host as X-Forwarded-Host.
			if req.Header.Get("X-Forwarded-Host") == "" {
				req.Header.Set("X-Forwarded-Host", r.Host)
			}

			// Add X-Forwarded-For with client IP.
			clientIP, _, ipErr := net.SplitHostPort(r.RemoteAddr)
			if ipErr != nil {
				clientIP = r.RemoteAddr
			}
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			p.logger.Info("proxy request",
				"method", r.Method,
				"path", r.URL.Path,
				"upstream", b.RawURL,
				"status", resp.StatusCode,
				"duration", time.Since(start),
			)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			p.logger.Error("proxy error",
				"method", r.Method,
				"path", r.URL.Path,
				"upstream", b.RawURL,
				"error", err,
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "bad gateway",
			})
		},
	}

	rp.ServeHTTP(w, r)
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
				json.NewEncoder(w).Encode(map[string]string{
					"error": "internal server error",
				})
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
