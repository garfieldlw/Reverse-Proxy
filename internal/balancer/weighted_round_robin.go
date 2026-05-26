package balancer

import (
	"sync"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

// WeightedRoundRobin implements Nginx-style smooth weighted round-robin balancing.
type WeightedRoundRobin struct {
	mu           sync.Mutex
	currentWeight map[string]int64
}

// Select returns a backend based on smooth weighted round-robin algorithm.
// Each call adds each backend's Weight to its currentWeight, selects the one
// with the highest currentWeight, and subtracts totalWeight from the selected.
func (w *WeightedRoundRobin) Select(backends []*backend.Backend) (*backend.Backend, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(backends) == 0 {
		return nil, ErrNoBackends
	}

	var totalWeight int64
	var best *backend.Backend
	var bestWeight int64

	for _, b := range backends {
		totalWeight += int64(b.Weight)
		w.currentWeight[b.RawURL] += int64(b.Weight)

		if w.currentWeight[b.RawURL] > bestWeight {
			bestWeight = w.currentWeight[b.RawURL]
			best = b
		}
	}

	if best != nil {
		w.currentWeight[best.RawURL] -= totalWeight
	}

	return best, nil
}
