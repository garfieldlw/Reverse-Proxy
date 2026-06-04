package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/garfieldlw/reverse-proxy/internal/middleware/ratelimit"
)

func TestHTTPProxySuccess(t *testing.T) {
	// Start a test backend server.
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "ok")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from backend"))
	}))
	defer backendServer.Close()

	bu, _ := url.Parse(backendServer.URL)
	b := &backend.Backend{
		URL:    bu,
		RawURL: backendServer.URL,
	}
	pool := &backend.Pool{
		Name:     "test",
		Backends: []*backend.Backend{b},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Test"); got != "ok" {
		t.Errorf("expected X-Test=ok, got %q", got)
	}
}

func TestHTTPProxyNoBackends(t *testing.T) {
	// Create a pool with an unhealthy backend.
	bu, _ := url.Parse("http://127.0.0.1:1")
	b := &backend.Backend{
		URL:    bu,
		RawURL: "http://127.0.0.1:1",
	}
	b.SetStatus(backend.StatusUnhealthy)
	pool := &backend.Pool{
		Name:     "test",
		Backends: []*backend.Backend{b},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body["error"] != "no backends available" {
		t.Errorf("expected error 'no backends available', got %q", body["error"])
	}
}

func TestHTTPProxyBackendSelection(t *testing.T) {
	var hitCount1, hitCount2 atomic.Int64

	// Start two backend servers.
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount1.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend1"))
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount2.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend2"))
	}))
	defer backend2.Close()

	bu1, _ := url.Parse(backend1.URL)
	bu2, _ := url.Parse(backend2.URL)

	pool := &backend.Pool{
		Name: "test",
		Backends: []*backend.Backend{
			{URL: bu1, RawURL: backend1.URL},
			{URL: bu2, RawURL: backend2.URL},
		},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Send 2 requests — round-robin should hit each backend once.
	for i := 0; i < 2; i++ {
		resp, err := http.Get(proxyServer.URL + "/test")
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
	}

	if hitCount1.Load() != 1 {
		t.Errorf("expected backend1 hit count 1, got %d", hitCount1.Load())
	}
	if hitCount2.Load() != 1 {
		t.Errorf("expected backend2 hit count 1, got %d", hitCount2.Load())
	}
}

func TestHTTPProxyConnectionTracking(t *testing.T) {
	// Start a backend that blocks until we signal it.
	proceed := make(chan struct{})
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-proceed
		w.WriteHeader(http.StatusOK)
	}))
	defer backendServer.Close()

	bu, _ := url.Parse(backendServer.URL)
	b := &backend.Backend{
		URL:    bu,
		RawURL: backendServer.URL,
	}
	pool := &backend.Pool{
		Name:     "test",
		Backends: []*backend.Backend{b},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Use a fresh transport to avoid HTTP/1.1 connection reuse which can
	// cause the request to complete before we observe active connections.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	done := make(chan struct{})
	go func() {
		resp, err := client.Get(proxyServer.URL + "/test")
		if err != nil {
			t.Errorf("request failed: %v", err)
		} else {
			resp.Body.Close()
		}
		close(done)
	}()

	// Poll for active connections — the backend blocks so the proxy
	// should have an active connection while waiting.
	var sawActive bool
	for i := 0; i < 200; i++ {
		if b.GetActiveConns() > 0 {
			sawActive = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !sawActive {
		t.Error("expected active connections > 0 during request")
	}

	// Let the backend finish.
	close(proceed)
	<-done

	// After request completes, connections should be back to 0.
	if b.GetActiveConns() != 0 {
		t.Errorf("expected active connections 0 after request, got %d", b.GetActiveConns())
	}
}

func TestHTTPProxyHeaders(t *testing.T) {
	var receivedHost, receivedXFF string

	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Header.Get("X-Forwarded-Host")
		receivedXFF = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	defer backendServer.Close()

	bu, _ := url.Parse(backendServer.URL)
	b := &backend.Backend{
		URL:    bu,
		RawURL: backendServer.URL,
	}
	pool := &backend.Pool{
		Name:     "test",
		Backends: []*backend.Backend{b},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if receivedHost == "" {
		t.Error("expected X-Forwarded-Host header to be set")
	}
	if receivedXFF == "" {
		t.Error("expected X-Forwarded-For header to be set")
	}
}

func TestHTTPProxyError(t *testing.T) {
	// Create a backend pointing to a port that refuses connections.
	bu, _ := url.Parse("http://127.0.0.1:1")
	b := &backend.Backend{
		URL:    bu,
		RawURL: "http://127.0.0.1:1",
	}
	pool := &backend.Pool{
		Name:     "test",
		Backends: []*backend.Backend{b},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body["error"] != "bad gateway" {
		t.Errorf("expected error 'bad gateway', got %q", body["error"])
	}
}

func TestHTTPProxyRecovery(t *testing.T) {
	// Create a proxy that panics in its ServeHTTP by using a custom
	// handler that wraps the proxy and panics before proxying.
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backendServer.Close()

	bu, _ := url.Parse(backendServer.URL)
	b := &backend.Backend{
		URL:    bu,
		RawURL: backendServer.URL,
	}
	pool := &backend.Pool{
		Name:     "test",
		Backends: []*backend.Backend{b},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	_ = NewHTTPProxy(pool, rr, nil, logger, config.TransportConfig{})

	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	// Use recoveryMiddleware which is the outermost layer in Handler().
	handler := recoveryMiddleware(panicHandler, logger)
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	resp, err := client.Get(proxyServer.URL + "/test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body["error"] != "internal server error" {
		t.Errorf("expected error 'internal server error', got %q", body["error"])
	}
}

func TestHTTPProxyHandlerWithRateLimiter(t *testing.T) {
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backendServer.Close()

	bu, _ := url.Parse(backendServer.URL)
	b := &backend.Backend{
		URL:    bu,
		RawURL: backendServer.URL,
	}
	pool := &backend.Pool{
		Name:     "test",
		Backends: []*backend.Backend{b},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	// Create a rate limiter with very low limits.
	cfg := config.RateLimitConfig{
		Enabled:          true,
		RequestsPerSecond: 1,
		Burst:            1,
		PerIP:            false,
	}
	limiter := ratelimit.New(cfg, logger)

	proxy := NewHTTPProxy(pool, rr, limiter, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy.Handler())
	defer proxyServer.Close()

	// First request should succeed.
	resp, err := http.Get(proxyServer.URL + "/test")
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected first request status 200, got %d", resp.StatusCode)
	}
}
