package balancer

import (
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

func TestRandomBasic(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 1, 0),
		newTestBackend("http://b", 1, 0),
		newTestBackend("http://c", 1, 0),
	}

	r := &Random{}
	for i := 0; i < 100; i++ {
		b, err := r.Select(backends)
		if err != nil {
			t.Fatalf("Select returned error: %v", err)
		}
		found := false
		for _, bb := range backends {
			if b.RawURL == bb.RawURL {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Select returned unexpected backend %s", b.RawURL)
		}
	}
}

func TestRandomEmpty(t *testing.T) {
	r := &Random{}
	_, err := r.Select([]*backend.Backend{})
	if err != ErrNoBackends {
		t.Errorf("Select(empty) error = %v, want ErrNoBackends", err)
	}
}

func TestRandomDistribution(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 1, 0),
		newTestBackend("http://b", 1, 0),
		newTestBackend("http://c", 1, 0),
	}

	r := &Random{}
	counts := make(map[string]int)
	iterations := 3000

	for i := 0; i < iterations; i++ {
		b, err := r.Select(backends)
		if err != nil {
			t.Fatalf("Select returned error: %v", err)
		}
		counts[b.RawURL]++
	}

	expected := float64(iterations) / float64(len(backends))
	tolerance := expected * 0.30 // 30% tolerance

	for _, url := range []string{"http://a", "http://b", "http://c"} {
		diff := float64(counts[url]) - expected
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			t.Errorf("backend %s selected %d times, expected around %.0f (tolerance %.0f)", url, counts[url], expected, tolerance)
		}
	}
}
