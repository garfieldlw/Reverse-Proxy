package balancer

import (
	"net/url"
	"sync"
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

func newTestBackend(rawURL string, weight int, conns int64) *backend.Backend {
	u, _ := url.Parse(rawURL)
	b := &backend.Backend{URL: u, RawURL: rawURL, Weight: weight}
	b.ActiveConns.Store(conns)
	return b
}

func TestRoundRobinBasic(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 1, 0),
		newTestBackend("http://b", 1, 0),
		newTestBackend("http://c", 1, 0),
	}

	rr := &RoundRobin{}
	counts := make(map[string]int)

	for i := 0; i < 6; i++ {
		b, err := rr.Select(backends)
		if err != nil {
			t.Fatalf("Select returned error: %v", err)
		}
		counts[b.RawURL]++
	}

	for _, url := range []string{"http://a", "http://b", "http://c"} {
		if counts[url] != 2 {
			t.Errorf("backend %s selected %d times, want 2", url, counts[url])
		}
	}
}

func TestRoundRobinEmpty(t *testing.T) {
	rr := &RoundRobin{}
	_, err := rr.Select([]*backend.Backend{})
	if err != ErrNoBackends {
		t.Errorf("Select(empty) error = %v, want ErrNoBackends", err)
	}
}

func TestRoundRobinSingle(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 1, 0),
	}

	rr := &RoundRobin{}
	for i := 0; i < 5; i++ {
		b, err := rr.Select(backends)
		if err != nil {
			t.Fatalf("Select returned error: %v", err)
		}
		if b.RawURL != "http://a" {
			t.Errorf("Select returned %s, want http://a", b.RawURL)
		}
	}
}

func TestRoundRobinConcurrent(t *testing.T) {
	backends := []*backend.Backend{
		newTestBackend("http://a", 1, 0),
		newTestBackend("http://b", 1, 0),
		newTestBackend("http://c", 1, 0),
	}

	rr := &RoundRobin{}
	var wg sync.WaitGroup
	counts := make(map[string]int)
	var mu sync.Mutex

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := rr.Select(backends)
			if err != nil {
				t.Errorf("Select returned error: %v", err)
				return
			}
			mu.Lock()
			counts[b.RawURL]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	total := 0
	for _, count := range counts {
		total += count
	}
	if total != 100 {
		t.Errorf("total selections = %d, want 100", total)
	}
}
