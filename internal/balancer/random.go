package balancer

import (
	"math/rand/v2"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

// Random selects a backend at random.
type Random struct{}

// Select returns a randomly chosen backend from the list.
func (r *Random) Select(backends []*backend.Backend) (*backend.Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoBackends
	}

	idx := rand.IntN(len(backends))
	return backends[idx], nil
}
