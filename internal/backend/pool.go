package backend

import (
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

// Status represents the health status of a backend.
type Status int

const (
	StatusHealthy   Status = iota
	StatusUnhealthy
)

// Backend represents a single backend server.
type Backend struct {
	URL               *url.URL
	RawURL            string
	Weight            int
	ActiveConns       atomic.Int64
	status            Status
	consecutiveFails  atomic.Int64
	consecutivePasses atomic.Int64
	mu                sync.RWMutex
}

// IsHealthy returns whether the backend is currently healthy.
func (b *Backend) IsHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.status == StatusHealthy
}

// SetStatus sets the backend health status.
func (b *Backend) SetStatus(s Status) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = s
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
	Name     string
	Balancer string
	Backends []*Backend
	mu       sync.RWMutex
	logger   *slog.Logger
}

// NewPool creates a new backend pool from config.
func NewPool(cfg config.BackendPoolConfig, logger *slog.Logger) (*Pool, error) {
	pool := &Pool{
		Name:     cfg.Name,
		Balancer: cfg.Balancer,
		Backends: make([]*Backend, 0, len(cfg.Backends)),
		logger:   logger,
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
			status: StatusHealthy,
		}

		pool.Backends = append(pool.Backends, b)
	}

	return pool, nil
}

// GetHealthyBackends returns a slice of currently healthy backends.
func (p *Pool) GetHealthyBackends() []*Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()

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
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]*Backend, len(p.Backends))
	copy(result, p.Backends)
	return result
}

// MarkBackendUnhealthy marks a backend as unhealthy and logs a warning.
func (p *Pool) MarkBackendUnhealthy(b *Backend) {
	b.SetStatus(StatusUnhealthy)
	p.logger.Warn("backend marked unhealthy",
		"pool", p.Name,
		"backend", b.RawURL,
		"consecutive_fails", b.GetConsecutiveFails(),
	)
}

// MarkBackendHealthy marks a backend as healthy and logs an info message.
func (p *Pool) MarkBackendHealthy(b *Backend) {
	b.SetStatus(StatusHealthy)
	p.logger.Info("backend marked healthy",
		"pool", p.Name,
		"backend", b.RawURL,
		"consecutive_passes", b.GetConsecutivePasses(),
	)
}

// Size returns the total number of backends in the pool.
func (p *Pool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.Backends)
}

// HealthyCount returns the number of healthy backends.
func (p *Pool) HealthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	for _, b := range p.Backends {
		if b.IsHealthy() {
			count++
		}
	}
	return count
}

// FindBackendByURL finds a backend by its raw URL string.
func (p *Pool) FindBackendByURL(rawURL string) *Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, b := range p.Backends {
		if b.RawURL == rawURL {
			return b
		}
	}
	return nil
}
