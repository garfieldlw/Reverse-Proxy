package backend

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

func TestHealthCheckHTTP(t *testing.T) {
	// Start a test HTTP server that returns 200 by default.
	var statusCode atomic.Int32
	statusCode.Store(http.StatusOK)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(statusCode.Load()))
	}))
	defer server.Close()

	pool := &Pool{
		Name:     "http-test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: server.URL, status: StatusHealthy},
		},
		logger: slog.Default(),
	}
	// Parse the URL for the backend.
	u := mustParseURL(t, server.URL)
	pool.Backends[0].URL = u

	hcCfg := config.HealthCheckConfig{
		Enabled:            true,
		Interval:           "200ms",
		Timeout:            "1s",
		Path:               "/health",
		UnhealthyThreshold: 2,
		HealthyThreshold:   2,
	}

	hc := NewHealthChecker(pool, hcCfg, slog.Default())
	hc.Start()
	defer hc.Stop()

	// Backend should stay healthy while server returns 200.
	waitFor(t, 2*time.Second, 50*time.Millisecond, func() bool {
		return pool.Backends[0].GetConsecutivePasses() >= 1
	})

	if !pool.Backends[0].IsHealthy() {
		t.Fatal("expected backend to remain healthy with 200 responses")
	}

	// Make server return 500.
	statusCode.Store(http.StatusInternalServerError)

	// Wait for backend to become unhealthy.
	waitFor(t, 3*time.Second, 50*time.Millisecond, func() bool {
		return !pool.Backends[0].IsHealthy()
	})

	if pool.Backends[0].IsHealthy() {
		t.Fatal("expected backend to become unhealthy with 500 responses")
	}

	// Make server return 200 again.
	statusCode.Store(http.StatusOK)

	// Wait for backend to become healthy again.
	waitFor(t, 3*time.Second, 50*time.Millisecond, func() bool {
		return pool.Backends[0].IsHealthy()
	})

	if !pool.Backends[0].IsHealthy() {
		t.Fatal("expected backend to become healthy again with 200 responses")
	}
}

func TestHealthCheckTCP(t *testing.T) {
	// Start a TCP listener.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	addr := listener.Addr().String()
	rawURL := "http://" + addr

	pool := &Pool{
		Name:     "tcp-test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: rawURL, status: StatusHealthy},
		},
		logger: slog.Default(),
	}
	pool.Backends[0].URL = mustParseURL(t, rawURL)

	hcCfg := config.HealthCheckConfig{
		Enabled:            true,
		Interval:           "200ms",
		Timeout:            "1s",
		Path:               "", // empty path means TCP check
		UnhealthyThreshold: 2,
		HealthyThreshold:   2,
	}

	hc := NewHealthChecker(pool, hcCfg, slog.Default())
	hc.Start()
	defer hc.Stop()

	// Accept connections in background so the dial succeeds.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	// Backend should stay healthy while listener is up.
	waitFor(t, 2*time.Second, 50*time.Millisecond, func() bool {
		return pool.Backends[0].GetConsecutivePasses() >= 1
	})

	if !pool.Backends[0].IsHealthy() {
		t.Fatal("expected backend to remain healthy with TCP listener up")
	}

	// Close the listener to simulate failure.
	listener.Close()

	// Wait for backend to become unhealthy.
	waitFor(t, 3*time.Second, 50*time.Millisecond, func() bool {
		return !pool.Backends[0].IsHealthy()
	})

	if pool.Backends[0].IsHealthy() {
		t.Fatal("expected backend to become unhealthy with TCP listener down")
	}
}

func TestHealthCheckThresholds(t *testing.T) {
	// Test that the exact threshold values are respected.
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		// First 5 requests return 500, then 200.
		if count <= 5 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pool := &Pool{
		Name:     "threshold-test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: server.URL, status: StatusHealthy},
		},
		logger: slog.Default(),
	}
	pool.Backends[0].URL = mustParseURL(t, server.URL)

	hcCfg := config.HealthCheckConfig{
		Enabled:            true,
		Interval:           "100ms",
		Timeout:            "1s",
		Path:               "/health",
		UnhealthyThreshold: 3,
		HealthyThreshold:   2,
	}

	hc := NewHealthChecker(pool, hcCfg, slog.Default())
	hc.Start()
	defer hc.Stop()

	// Wait for backend to become unhealthy (needs 3 consecutive failures).
	waitFor(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return !pool.Backends[0].IsHealthy()
	})

	if pool.Backends[0].IsHealthy() {
		t.Fatal("expected backend to become unhealthy after 3 consecutive failures")
	}

	// Now server returns 200. Wait for backend to become healthy (needs 2 consecutive passes).
	waitFor(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return pool.Backends[0].IsHealthy()
	})

	if !pool.Backends[0].IsHealthy() {
		t.Fatal("expected backend to become healthy after 2 consecutive passes")
	}
}

func TestHealthCheckStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pool := &Pool{
		Name:     "stop-test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: server.URL, status: StatusHealthy},
		},
		logger: slog.Default(),
	}
	pool.Backends[0].URL = mustParseURL(t, server.URL)

	hcCfg := config.HealthCheckConfig{
		Enabled:            true,
		Interval:           "100ms",
		Timeout:            "1s",
		Path:               "/health",
		UnhealthyThreshold: 3,
		HealthyThreshold:   2,
	}

	hc := NewHealthChecker(pool, hcCfg, slog.Default())
	hc.Start()

	// Let it run for a bit.
	time.Sleep(300 * time.Millisecond)

	// Stop should complete cleanly without hanging.
	done := make(chan struct{})
	go func() {
		hc.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success - Stop completed.
	case <-time.After(6 * time.Second):
		t.Fatal("Stop() did not complete within timeout")
	}
}

func TestHealthCheckContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pool := &Pool{
		Name:     "ctx-test",
		Balancer: "round_robin",
		Backends: []*Backend{
			{RawURL: server.URL, status: StatusHealthy},
		},
		logger: slog.Default(),
	}
	pool.Backends[0].URL = mustParseURL(t, server.URL)

	hcCfg := config.HealthCheckConfig{
		Enabled:            true,
		Interval:           "100ms",
		Timeout:            "1s",
		Path:               "/health",
		UnhealthyThreshold: 3,
		HealthyThreshold:   2,
	}

	hc := NewHealthChecker(pool, hcCfg, slog.Default())
	hc.Start()

	// Cancel via context directly.
	time.Sleep(200 * time.Millisecond)
	hc.cancel()

	// Wait for the goroutine to finish.
	select {
	case <-hc.done:
		// Success.
	case <-time.After(3 * time.Second):
		t.Fatal("health checker goroutine did not finish after context cancellation")
	}
}

// waitFor repeatedly checks condition at the given interval until it returns true
// or the timeout elapses.
func waitFor(t *testing.T, timeout, interval time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
}

// mustParseURL parses a URL string or fails the test.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("failed to parse URL %q: %v", raw, err)
	}
	return u
}

// Ensure context import is used.
var _ = context.TODO

func TestHealthCheckUnix(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "health.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	poolCfg := config.BackendPoolConfig{
		Name:     "unix-test",
		Balancer: "round_robin",
		HealthCheck: config.HealthCheckConfig{
			Enabled:  true,
			Interval: "1s",
			Timeout:  "500ms",
		},
		Backends: []config.BackendConfig{
			{URL: "unix:" + socketPath, Weight: 1},
		},
	}

	pool, err := NewPool(poolCfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	checker := NewHealthChecker(pool, poolCfg.HealthCheck, slog.Default())
	checker.checkAll()

	backends := pool.GetHealthyBackends()
	if len(backends) != 1 {
		t.Errorf("expected 1 healthy backend, got %d", len(backends))
	}
}

func TestHealthCheckUnixFailed(t *testing.T) {
	poolCfg := config.BackendPoolConfig{
		Name:     "unix-fail-test",
		Balancer: "round_robin",
		HealthCheck: config.HealthCheckConfig{
			Enabled:            true,
			Interval:           "1s",
			Timeout:            "500ms",
			UnhealthyThreshold: 1,
			HealthyThreshold:   1,
		},
		Backends: []config.BackendConfig{
			{URL: "unix:/nonexistent/path.sock", Weight: 1},
		},
	}

	pool, err := NewPool(poolCfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	checker := NewHealthChecker(pool, poolCfg.HealthCheck, slog.Default())
	checker.checkAll()

	backends := pool.GetHealthyBackends()
	if len(backends) != 0 {
		t.Errorf("expected 0 healthy backends, got %d", len(backends))
	}
}
