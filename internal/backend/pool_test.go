package backend

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

func TestNewPool(t *testing.T) {
	cfg := config.BackendPoolConfig{
		Name:     "test-pool",
		Balancer: "round_robin",
		Backends: []config.BackendConfig{
			{URL: "http://localhost:8081", Weight: 1},
			{URL: "http://localhost:8082", Weight: 2},
			{URL: "http://localhost:8083", Weight: 3},
		},
	}

	pool, err := NewPool(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewPool returned error: %v", err)
	}

	if pool.Name != "test-pool" {
		t.Errorf("expected pool name 'test-pool', got %q", pool.Name)
	}
	if pool.Balancer != "round_robin" {
		t.Errorf("expected balancer 'round_robin', got %q", pool.Balancer)
	}
	if len(pool.Backends) != 3 {
		t.Fatalf("expected 3 backends, got %d", len(pool.Backends))
	}

	for i, b := range pool.Backends {
		if b.Weight != i+1 {
			t.Errorf("backend[%d]: expected weight %d, got %d", i, i+1, b.Weight)
		}
		if !b.IsHealthy() {
			t.Errorf("backend[%d]: expected to be healthy initially", i)
		}
		if b.URL == nil {
			t.Errorf("backend[%d]: URL is nil", i)
		}
	}
}

func TestNewPoolInvalidURL(t *testing.T) {
	cfg := config.BackendPoolConfig{
		Name:     "bad-pool",
		Balancer: "round_robin",
		Backends: []config.BackendConfig{
			{URL: "://invalid-url", Weight: 1},
		},
	}

	_, err := NewPool(cfg, slog.Default())
	if err == nil {
		t.Fatal("expected error for invalid URL, got nil")
	}
}

func TestPoolGetHealthyBackends(t *testing.T) {
	pool := &Pool{
		Name:     "test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: "http://a:80", status: StatusHealthy},
			{RawURL: "http://b:80", status: StatusUnhealthy},
			{RawURL: "http://c:80", status: StatusHealthy},
		},
		logger: slog.Default(),
	}

	healthy := pool.GetHealthyBackends()
	if len(healthy) != 2 {
		t.Fatalf("expected 2 healthy backends, got %d", len(healthy))
	}

	for _, b := range healthy {
		if !b.IsHealthy() {
			t.Errorf("got unhealthy backend in healthy list: %s", b.RawURL)
		}
	}
}

func TestPoolMarkBackend(t *testing.T) {
	pool := &Pool{
		Name:     "test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: "http://a:80", status: StatusHealthy},
		},
		logger: slog.Default(),
	}

	b := pool.Backends[0]
	if !b.IsHealthy() {
		t.Fatal("expected backend to start healthy")
	}

	pool.MarkBackendUnhealthy(b)
	if b.IsHealthy() {
		t.Fatal("expected backend to be unhealthy after MarkBackendUnhealthy")
	}

	pool.MarkBackendHealthy(b)
	if !b.IsHealthy() {
		t.Fatal("expected backend to be healthy after MarkBackendHealthy")
	}
}

func TestBackendActiveConns(t *testing.T) {
	b := &Backend{RawURL: "http://localhost:80", status: StatusHealthy}

	if conns := b.GetActiveConns(); conns != 0 {
		t.Fatalf("expected 0 active conns, got %d", conns)
	}

	b.IncConns()
	b.IncConns()
	b.IncConns()
	if conns := b.GetActiveConns(); conns != 3 {
		t.Fatalf("expected 3 active conns, got %d", conns)
	}

	b.DecConns()
	if conns := b.GetActiveConns(); conns != 2 {
		t.Fatalf("expected 2 active conns, got %d", conns)
	}
}

func TestBackendRecordFailureSuccess(t *testing.T) {
	b := &Backend{RawURL: "http://localhost:80", status: StatusHealthy}

	// Record failures
	if fails := b.RecordFailure(); fails != 1 {
		t.Errorf("expected 1 fail, got %d", fails)
	}
	if fails := b.RecordFailure(); fails != 2 {
		t.Errorf("expected 2 fails, got %d", fails)
	}
	if fails := b.RecordFailure(); fails != 3 {
		t.Errorf("expected 3 fails, got %d", fails)
	}

	// RecordSuccess should reset fails and start counting passes
	if passes := b.RecordSuccess(); passes != 1 {
		t.Errorf("expected 1 pass, got %d", passes)
	}
	if b.GetConsecutiveFails() != 0 {
		t.Errorf("expected 0 consecutive fails after success, got %d", b.GetConsecutiveFails())
	}

	// RecordFailure should reset passes and start counting fails again
	if fails := b.RecordFailure(); fails != 1 {
		t.Errorf("expected 1 fail after reset, got %d", fails)
	}
	if b.GetConsecutivePasses() != 0 {
		t.Errorf("expected 0 consecutive passes after failure, got %d", b.GetConsecutivePasses())
	}

	// ResetFails
	b.ResetFails()
	if b.GetConsecutiveFails() != 0 {
		t.Errorf("expected 0 consecutive fails after reset, got %d", b.GetConsecutiveFails())
	}
}

func TestPoolConcurrentAccess(t *testing.T) {
	pool := &Pool{
		Name:     "concurrent-test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: "http://a:80", status: StatusHealthy},
			{RawURL: "http://b:80", status: StatusHealthy},
			{RawURL: "http://c:80", status: StatusHealthy},
		},
		logger: slog.Default(),
	}

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent reads
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.GetHealthyBackends()
			_ = pool.Size()
			_ = pool.HealthyCount()
			_ = pool.GetAllBackends()
			_ = pool.FindBackendByURL("http://a:80")
		}()
	}

	// Concurrent writes (status changes)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b := pool.Backends[idx%3]
			if idx%2 == 0 {
				pool.MarkBackendUnhealthy(b)
			} else {
				pool.MarkBackendHealthy(b)
			}
		}(i)
	}

	// Concurrent backend connection tracking
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b := pool.Backends[idx%3]
			b.IncConns()
			_ = b.GetActiveConns()
			b.DecConns()
		}(i)
	}

	// Concurrent fail/success recording
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b := pool.Backends[idx%3]
			if idx%2 == 0 {
				b.RecordFailure()
			} else {
				b.RecordSuccess()
			}
			_ = b.GetConsecutiveFails()
			_ = b.GetConsecutivePasses()
		}(i)
	}

	wg.Wait()
}

func TestPoolFindBackendByURL(t *testing.T) {
	pool := &Pool{
		Name:     "test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: "http://a:80", status: StatusHealthy},
			{RawURL: "http://b:80", status: StatusHealthy},
		},
		logger: slog.Default(),
	}

	if b := pool.FindBackendByURL("http://a:80"); b == nil || b.RawURL != "http://a:80" {
		t.Error("expected to find backend http://a:80")
	}
	if b := pool.FindBackendByURL("http://nonexistent:80"); b != nil {
		t.Error("expected nil for nonexistent backend URL")
	}
}

func TestPoolSizeAndHealthyCount(t *testing.T) {
	pool := &Pool{
		Name:     "test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: "http://a:80", status: StatusHealthy},
			{RawURL: "http://b:80", status: StatusUnhealthy},
			{RawURL: "http://c:80", status: StatusHealthy},
		},
		logger: slog.Default(),
	}

	if pool.Size() != 3 {
		t.Errorf("expected size 3, got %d", pool.Size())
	}
	if pool.HealthyCount() != 2 {
		t.Errorf("expected healthy count 2, got %d", pool.HealthyCount())
	}
}

func TestBackendURLParsing(t *testing.T) {
	cfg := config.BackendPoolConfig{
		Name:     "url-test",
		Balancer: "round_robin",
		Backends: []config.BackendConfig{
			{URL: "http://example.com:8080", Weight: 1},
		},
	}

	pool, err := NewPool(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewPool returned error: %v", err)
	}

	b := pool.Backends[0]
	if b.URL.Host != "example.com:8080" {
		t.Errorf("expected host 'example.com:8080', got %q", b.URL.Host)
	}
	if b.URL.Scheme != "http" {
		t.Errorf("expected scheme 'http', got %q", b.URL.Scheme)
	}
}

func TestBackendSetStatusDirectly(t *testing.T) {
	b := &Backend{RawURL: "http://localhost:80", status: StatusHealthy}

	if !b.IsHealthy() {
		t.Fatal("expected healthy initially")
	}

	b.SetStatus(StatusUnhealthy)
	if b.IsHealthy() {
		t.Fatal("expected unhealthy after SetStatus")
	}

	b.SetStatus(StatusHealthy)
	if !b.IsHealthy() {
		t.Fatal("expected healthy after SetStatus")
	}
}
