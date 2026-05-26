package ratelimit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/config"
	"golang.org/x/time/rate"
)

// cleanupInterval controls how often stale per-IP entries are purged.
const cleanupInterval = 10 * time.Minute

// Limiter is an HTTP middleware that performs rate limiting.
type Limiter struct {
	config         config.RateLimitConfig
	globalLimiter  *rate.Limiter
	ipLimiters     map[string]*rate.Limiter
	ipLastAccess   map[string]time.Time
	mu             sync.RWMutex
	logger         *slog.Logger
	lastCleanup    time.Time
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
		config:       cfg,
		logger:       logger,
		ipLimiters:   make(map[string]*rate.Limiter),
		ipLastAccess: make(map[string]time.Time),
		lastCleanup:  time.Now(),
	}

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
	l.mu.RLock()
	limiter, ok := l.ipLimiters[ip]
	l.mu.RUnlock()

	if ok {
		l.mu.Lock()
		l.ipLastAccess[ip] = time.Now()
		l.mu.Unlock()
		return limiter
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Double-check after acquiring write lock.
	if limiter, ok = l.ipLimiters[ip]; ok {
		l.ipLastAccess[ip] = time.Now()
		return limiter
	}

	limiter = rate.NewLimiter(rate.Limit(l.config.RequestsPerSecond), l.config.Burst)
	l.ipLimiters[ip] = limiter
	l.ipLastAccess[ip] = time.Now()

	l.cleanupStaleEntries()

	return limiter
}

// cleanupStaleEntries removes per-IP limiters that haven't been accessed in cleanupInterval.
// Must be called with l.mu held for writing.
func (l *Limiter) cleanupStaleEntries() {
	now := time.Now()
	if now.Sub(l.lastCleanup) < cleanupInterval {
		return
	}

	l.lastCleanup = now
	threshold := now.Add(-cleanupInterval)

	for ip, lastAccess := range l.ipLastAccess {
		if lastAccess.Before(threshold) {
			delete(l.ipLimiters, ip)
			delete(l.ipLastAccess, ip)
		}
	}
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

	body := map[string]interface{}{
		"error":        "rate limit exceeded",
		"retry_after":  retryAfterSec,
	}
	json.NewEncoder(w).Encode(body)
}
