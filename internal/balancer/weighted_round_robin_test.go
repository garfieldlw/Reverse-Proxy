package balancer

import (
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

func TestWeightedRoundRobinBasic(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 5, 0),
		newTestBackend("http://b", 3, 0),
		newTestBackend("http://c", 2, 0),
	}

	wrr := &WeightedRoundRobin{
		currentWeight: make(map[string]int64),
	}

	counts := make(map[string]int)
	total := 10

	for i := 0; i < total; i++ {
		b, err := wrr.Select(backends)
		if err != nil {
			t.Fatalf("Select returned error: %v", err)
		}
		counts[b.RawURL]++
	}

	// With weights 5:3:2, expect distribution 5:3:2
	if counts["http://a"] != 5 {
		t.Errorf("backend a selected %d times, want 5", counts["http://a"])
	}
	if counts["http://b"] != 3 {
		t.Errorf("backend b selected %d times, want 3", counts["http://b"])
	}
	if counts["http://c"] != 2 {
		t.Errorf("backend c selected %d times, want 2", counts["http://c"])
	}
}

func TestWeightedRoundRobinEmpty(t *testing.T) {
	wrr := &WeightedRoundRobin{
		currentWeight: make(map[string]int64),
	}

	_, err := wrr.Select([]*backend.Backend{})
	if err != ErrNoBackends {
		t.Errorf("Select(empty) error = %v, want ErrNoBackends", err)
	}
}

func TestWeightedRoundRobinEqualWeights(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 3, 0),
		newTestBackend("http://b", 3, 0),
		newTestBackend("http://c", 3, 0),
	}

	wrr := &WeightedRoundRobin{
		currentWeight: make(map[string]int64),
	}

	counts := make(map[string]int)
	for i := 0; i < 9; i++ {
		b, err := wrr.Select(backends)
		if err != nil {
			t.Fatalf("Select returned error: %v", err)
		}
		counts[b.RawURL]++
	}

	for _, url := range []string{"http://a", "http://b", "http://c"} {
		if counts[url] != 3 {
			t.Errorf("backend %s selected %d times, want 3", url, counts[url])
		}
	}
}

func TestWeightedRoundRobinSingle(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 5, 0),
	}

	wrr := &WeightedRoundRobin{
		currentWeight: make(map[string]int64),
	}

	for i := 0; i < 5; i++ {
		b, err := wrr.Select(backends)
		if err != nil {
			t.Fatalf("Select returned error: %v", err)
		}
		if b.RawURL != "http://a" {
			t.Errorf("Select returned %s, want http://a", b.RawURL)
		}
	}
}
