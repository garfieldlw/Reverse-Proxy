package backend

import (
	"fmt"
	"log/slog"
	"net/url"
	"sync/atomic"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

// Status represents the health status of a backend.
type Status int32

const (
	StatusHealthy   Status = 0
	StatusUnhealthy Status = 1
)

// Backend represents a single backend server.
type Backend struct {
	URL               *url.URL
	RawURL            string
	Weight            int
	ActiveConns       atomic.Int64
	status            atomic.Int32 // stores Status value
	consecutiveFails  atomic.Int64
	consecutivePasses atomic.Int64
}

// IsHealthy returns whether the backend is currently healthy.
func (b *Backend) IsHealthy() bool {
	return Status(b.status.Load()) == StatusHealthy
}

// SetStatus sets the backend health status.
func (b *Backend) SetStatus(s Status) {
	b.status.Store(int32(s))
}

// IncConns increments the active connection count.
func (b *Backend) IncConns() {
	b.ActiveConns.Add(1)
}

// DecConns decrements the active connection count.
func (b *Backend) DecConns() {
	b.ActiveConns.Add(-1)
}

// GetActiveConns returns the current active connection count.
func (b *Backend) GetActiveConns() int64 {
	return b.ActiveConns.Load()
}

// RecordFailure increments the consecutive failure counter and resets passes.
// Returns the new consecutive failure count.
func (b *Backend) RecordFailure() int64 {
	b.consecutivePasses.Store(0)
	return b.consecutiveFails.Add(1)
}

// RecordSuccess increments the consecutive pass counter and resets failures.
// Returns the new consecutive pass count.
func (b *Backend) RecordSuccess() int64 {
	b.consecutiveFails.Store(0)
	return b.consecutivePasses.Add(1)
}

// ResetFails resets the consecutive failure counter to zero.
func (b *Backend) ResetFails() {
	b.consecutiveFails.Store(0)
}

// GetConsecutiveFails returns the current consecutive failure count.
func (b *Backend) GetConsecutiveFails() int64 {
	return b.consecutiveFails.Load()
}

// GetConsecutivePasses returns the current consecutive pass count.
func (b *Backend) GetConsecutivePasses() int64 {
	return b.consecutivePasses.Load()
}

// Pool manages a collection of backends.
type Pool struct {
	Name           string
	Balancer       string
	Backends       []*Backend
	logger         *slog.Logger
	healthyBackends atomic.Pointer[[]*Backend] // cached healthy backends slice
}

// NewPool creates a new backend pool from config.
func NewPool(cfg config.BackendPoolConfig, logger *slog.Logger) (*Pool, error) {
	pool := &Pool{
		Name:      cfg.Name,
		Balancer:  cfg.Balancer,
		Backends:  make([]*Backend, 0, len(cfg.Backends)),
		logger:    logger,
	}

	for _, bc := range cfg.Backends {
		u, err := url.Parse(bc.URL)
		if err != nil {
			return nil, fmt.Errorf("parsing backend URL %q: %w", bc.URL, err)
		}

		b := &Backend{
			URL:    u,
			RawURL: bc.URL,
			Weight: bc.Weight,
		}
		b.status.Store(int32(StatusHealthy))

		pool.Backends = append(pool.Backends, b)
	}

	// Build initial healthy backends cache.
	pool.rebuildHealthyCache()

	return pool, nil
}

// rebuildHealthyCache rebuilds the healthy backends cache and atomically stores it.
func (p *Pool) rebuildHealthyCache() {
	healthy := make([]*Backend, 0, len(p.Backends))
	for _, b := range p.Backends {
		if b.IsHealthy() {
			healthy = append(healthy, b)
		}
	}
	p.healthyBackends.Store(&healthy)
}

// GetHealthyBackends returns a slice of currently healthy backends.
func (p *Pool) GetHealthyBackends() []*Backend {
	if cached := p.healthyBackends.Load(); cached != nil {
		return *cached
	}
	// Fallback for pools created without cache (e.g. tests using struct literals).
	healthy := make([]*Backend, 0, len(p.Backends))
	for _, b := range p.Backends {
		if b.IsHealthy() {
			healthy = append(healthy, b)
		}
	}
	return healthy
}

// GetAllBackends returns all backends in the pool.
func (p *Pool) GetAllBackends() []*Backend {
	result := make([]*Backend, len(p.Backends))
	copy(result, p.Backends)
	return result
}

// MarkBackendUnhealthy marks a backend as unhealthy and logs a warning.
func (p *Pool) MarkBackendUnhealthy(b *Backend) {
	b.SetStatus(StatusUnhealthy)
	p.rebuildHealthyCache()
	p.logger.Warn("backend marked unhealthy",
		"pool", p.Name,
		"backend", b.RawURL,
		"consecutive_fails", b.GetConsecutiveFails(),
	)
}

// MarkBackendHealthy marks a backend as healthy and logs an info message.
func (p *Pool) MarkBackendHealthy(b *Backend) {
	b.SetStatus(StatusHealthy)
	p.rebuildHealthyCache()
	p.logger.Info("backend marked healthy",
		"pool", p.Name,
		"backend", b.RawURL,
		"consecutive_passes", b.GetConsecutivePasses(),
	)
}

// Size returns the total number of backends in the pool.
func (p *Pool) Size() int {
	return len(p.Backends)
}

// HealthyCount returns the number of healthy backends.
func (p *Pool) HealthyCount() int {
	count := 0
	for _, b := range p.Backends {
		if b.IsHealthy() {
			count++
		}
	}
	return count
}

// NewBackendForTest creates a Backend with the given status for testing.
// Production code should use NewPool which sets status automatically.
func NewBackendForTest(rawURL string, s Status) *Backend {
	b := &Backend{RawURL: rawURL}
	b.status.Store(int32(s))
	return b
}

// FindBackendByURL finds a backend by its raw URL string.
func (p *Pool) FindBackendByURL(rawURL string) *Backend {
	for _, b := range p.Backends {
		if b.RawURL == rawURL {
			return b
		}
	}
	return nil
}
