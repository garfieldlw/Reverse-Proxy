package balancer

import (
	"errors"
	"fmt"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

// ErrNoBackends is returned when no healthy backends are available.
var ErrNoBackends = errors.New("no healthy backends available")

// Balancer selects a backend from a list of healthy backends.
type Balancer interface {
	Select(backends []*backend.Backend) (*backend.Backend, error)
}

// New creates a Balancer based on the strategy name.
// Valid strategies: round_robin, weighted_round_robin, least_connections, random
func New(strategy string) (Balancer, error) {
	switch strategy {
	case "round_robin":
		return &RoundRobin{}, nil
	case "weighted_round_robin":
		return &WeightedRoundRobin{
			currentWeight: make(map[string]int64),
		}, nil
	case "least_connections":
		return &LeastConnections{}, nil
	case "random":
		return &Random{}, nil
	default:
		return nil, fmt.Errorf("unknown balancer strategy: %s", strategy)
	}
}
