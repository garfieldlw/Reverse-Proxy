package balancer

import "github.com/garfieldlw/reverse-proxy/internal/backend"

// LeastConnections selects the backend with the fewest active connections.
type LeastConnections struct{}

// Select returns the backend with the minimum number of active connections.
// If there is a tie, the first backend found with the minimum is returned.
func (lc *LeastConnections) Select(backends []*backend.Backend) (*backend.Backend, error) {
	if len(backends) == 0 {
		return nil, ErrNoBackends
	}

	best := backends[0]
	minConns := best.GetActiveConns()

	for _, b := range backends[1:] {
		conns := b.GetActiveConns()
		if conns < minConns {
			minConns = conns
			best = b
		}
	}

	return best, nil
}
