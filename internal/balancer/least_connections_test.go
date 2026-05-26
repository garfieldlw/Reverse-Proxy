package balancer

import (
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

func TestLeastConnectionsBasic(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 1, 5),
		newTestBackend("http://b", 1, 2),
		newTestBackend("http://c", 1, 8),
	}

	lc := &LeastConnections{}
	b, err := lc.Select(backends)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if b.RawURL != "http://b" {
		t.Errorf("Select returned %s (conns=%d), want http://b (conns=2)", b.RawURL, b.GetActiveConns())
	}
}

func TestLeastConnectionsEmpty(t *testing.T) {
	lc := &LeastConnections{}
	_, err := lc.Select([]*backend.Backend{})
	if err != ErrNoBackends {
		t.Errorf("Select(empty) error = %v, want ErrNoBackends", err)
	}
}

func TestLeastConnectionsTie(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 1, 3),
		newTestBackend("http://b", 1, 3),
		newTestBackend("http://c", 1, 5),
	}

	lc := &LeastConnections{}
	b, err := lc.Select(backends)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	// On tie, should return the first found
	if b.RawURL != "http://a" {
		t.Errorf("Select returned %s on tie, want http://a (first with min conns)", b.RawURL)
	}
}
