package ratelimit

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/config"
	"golang.org/x/time/rate"
)

// cleanupInterval controls how often stale per-IP entries are purged.
const cleanupInterval = 10 * time.Minute

// errRateLimitFmt is a format string for 429 rate limit responses.
const errRateLimitFmt = "{\"error\":\"rate limit exceeded\",\"retry_after\":%d}\n"

// ipEntry holds a per-IP rate limiter and its last access timestamp.
type ipEntry struct {
	limiter    *rate.Limiter
	lastAccess atomic.Int64 // unix nanos
}

// Limiter is an HTTP middleware that performs rate limiting.
type Limiter struct {
	config        config.RateLimitConfig
	globalLimiter *rate.Limiter
	ipLimiters    sync.Map // string -> *ipEntry
	logger        *slog.Logger
	lastCleanup   atomic.Int64 // unix nanos of last cleanup
}

// New creates a new rate limiting middleware.
// If cfg.Enabled is false, it returns a no-op limiter that never blocks requests.
func New(cfg config.RateLimitConfig, logger *slog.Logger) *Limiter {
	if !cfg.Enabled {
		return &Limiter{
			config: cfg,
			logger: logger,
		}
	}

	l := &Limiter{
		config: cfg,
		logger: logger,
	}
	l.lastCleanup.Store(time.Now().UnixNano())

	if !cfg.PerIP {
		l.globalLimiter = rate.NewLimiter(rate.Limit(cfg.RequestsPerSecond), cfg.Burst)
	}

	return l
}

// Middleware returns an HTTP handler that applies rate limiting before delegating to next.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.config.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		ip := l.extractIP(r)

		var limiter *rate.Limiter
		if l.config.PerIP {
			limiter = l.getIPLimiter(ip)
		} else {
			limiter = l.globalLimiter
		}

		if !limiter.Allow() {
			retryAfter := limiter.Reserve().Delay()
			l.writeRateLimitResponse(w, ip, r.URL.Path, retryAfter)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// extractIP returns the client IP, checking X-Forwarded-For and X-Real-IP first.
func (l *Limiter) extractIP(r *http.Request) string {
	// Check X-Forwarded-For header (first IP in the list).
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			ip := strings.TrimSpace(ips[0])
			if ip != "" {
				return ip
			}
		}
	}

	// Check X-Real-IP header.
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}

	// Fall back to RemoteAddr, stripping the port.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// getIPLimiter returns the rate limiter for the given IP, creating one if needed.
func (l *Limiter) getIPLimiter(ip string) *rate.Limiter {
	now := time.Now().UnixNano()
	if v, ok := l.ipLimiters.Load(ip); ok {
		entry := v.(*ipEntry)
		entry.lastAccess.Store(now)
		return entry.limiter
	}

	entry := &ipEntry{
		limiter: rate.NewLimiter(rate.Limit(l.config.RequestsPerSecond), l.config.Burst),
	}
	entry.lastAccess.Store(now)

	if actual, loaded := l.ipLimiters.LoadOrStore(ip, entry); loaded {
		actual.(*ipEntry).lastAccess.Store(now)
		return actual.(*ipEntry).limiter
	}

	l.maybeCleanup()
	return entry.limiter
}

// cleanupStaleEntries removes per-IP limiters that haven't been accessed in cleanupInterval.
// Must be called with l.mu held for writing.
func (l *Limiter) maybeCleanup() {
	now := time.Now()
	last := l.lastCleanup.Load()
	if now.UnixNano()-last < int64(cleanupInterval) {
		return
	}
	if !l.lastCleanup.CompareAndSwap(last, now.UnixNano()) {
		return // another goroutine is cleaning up
	}
	threshold := now.Add(-cleanupInterval).UnixNano()
	l.ipLimiters.Range(func(key, value any) bool {
		entry := value.(*ipEntry)
		if entry.lastAccess.Load() < threshold {
			l.ipLimiters.Delete(key)
		}
		return true
	})
}

// writeRateLimitResponse writes a 429 Too Many Requests response.
func (l *Limiter) writeRateLimitResponse(w http.ResponseWriter, ip, path string, retryAfter time.Duration) {
	retryAfterSec := int(retryAfter.Seconds())
	if retryAfterSec < 1 {
		retryAfterSec = 1
	}

	l.logger.Warn("rate limited",
		"ip", ip,
		"path", path,
		"retry_after", retryAfterSec,
	)

	w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfterSec))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)

	fmt.Fprintf(w, errRateLimitFmt, retryAfterSec)
}
