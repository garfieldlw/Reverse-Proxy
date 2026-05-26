package balancer

import (
	"sync/atomic"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

// RoundRobin selects backends in sequential order using an atomic counter.
type RoundRobin struct {
	counter atomic.Uint64
}

// Select returns the next backend in round-robin order.
func (rr *RoundRobin) Select(backends []*backend.Backend) (*backend.Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoBackends
	}

	idx := rr.counter.Add(1) % uint64(len(backends))
	return backends[idx], nil
}
