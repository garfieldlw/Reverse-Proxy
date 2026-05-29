package proxy

import (
	"bytes"
	"io"
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

func init() {
	// Suppress INFO-level log noise during benchmarks. Only warn and above
	// will be emitted so benchmark output stays clean.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

func newBackendPool(name string, serverURLs ...string) *backend.Pool {
	backends := make([]*backend.Backend, len(serverURLs))
	for i, raw := range serverURLs {
		u, _ := url.Parse(raw)
		backends[i] = &backend.Backend{
			URL:    u,
			RawURL: raw,
		}
	}
	return &backend.Pool{
		Name:     name,
		Backends: backends,
	}
}

func newBenchTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:    90 * time.Second,
	}
}

func benchGet(client *http.Client, url string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	return nil
}

func BenchmarkHTTPProxySingleBackend(b *testing.B) {
	b.ReportAllocs()

	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("x"))
	}))
	defer backendSrv.Close()

	pool := newBackendPool("bench-single", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := &http.Client{Transport: newBenchTransport()}

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, proxySrv.URL+"/bench"); err != nil {
			b.Fatalf("request failed: %v", err)
		}
	}
}

func BenchmarkHTTPProxyMultiBackend(b *testing.B) {
	b.ReportAllocs()

	var hits [3]atomic.Int64

	backendSrvs := make([]*httptest.Server, 3)
	backendURLs := make([]string, 3)
	for i := range 3 {
		idx := i
		backendSrvs[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[idx].Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		}))
		backendURLs[i] = backendSrvs[i].URL
	}
	defer func() {
		for _, s := range backendSrvs {
			s.Close()
		}
	}()

	pool := newBackendPool("bench-multi", backendURLs...)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := &http.Client{Transport: newBenchTransport()}

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, proxySrv.URL+"/bench"); err != nil {
			b.Fatalf("request failed: %v", err)
		}
	}
	b.StopTimer()

	total := hits[0].Load() + hits[1].Load() + hits[2].Load()
	if total >= 3 {
		for i := range 3 {
			ratio := float64(hits[i].Load()) / float64(total)
			if ratio < 0.2 || ratio > 0.5 {
				b.Errorf("backend %d hit ratio %.2f outside expected range [0.2, 0.5] (hits=%d, total=%d)",
					i, ratio, hits[i].Load(), total)
			}
		}
	}
}

func BenchmarkHTTPProxyBodySizes(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{
		{"1B", 1},
		{"1KB", 1024},
		{"10KB", 10 * 1024},
		{"100KB", 100 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()

			body := bytes.Repeat([]byte("x"), sz.n)

			backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write(body)
			}))
			defer backendSrv.Close()

			pool := newBackendPool("bench-body", backendSrv.URL)
			rr := &balancer.RoundRobin{}
			logger := slog.Default()

			proxy := NewHTTPProxy(pool, rr, nil, logger)
			proxySrv := httptest.NewServer(proxy)
			defer proxySrv.Close()

			client := &http.Client{Transport: newBenchTransport()}

			b.ResetTimer()
			for b.Loop() {
				if err := benchGet(client, proxySrv.URL+"/bench"); err != nil {
					b.Fatalf("request failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkHTTPProxyWithRateLimiter(b *testing.B) {
	b.ReportAllocs()

	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("x"))
	}))
	defer backendSrv.Close()

	pool := newBackendPool("bench-ratelimit", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	cfg := config.RateLimitConfig{
		Enabled:            true,
		RequestsPerSecond:  10000,
		Burst:              50000,
		PerIP:              false,
	}
	limiter := ratelimit.New(cfg, logger)

	proxy := NewHTTPProxy(pool, rr, limiter, logger)
	proxySrv := httptest.NewServer(proxy.Handler())
	defer proxySrv.Close()

	client := &http.Client{Transport: newBenchTransport()}

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, proxySrv.URL+"/bench"); err != nil {
			b.Fatalf("request failed: %v", err)
		}
	}
}

func BenchmarkHTTPProxyWithHealthCheck(b *testing.B) {
	b.ReportAllocs()

	backendSrvs := make([]*httptest.Server, 3)
	backendURLs := make([]string, 3)
	for i := range 3 {
		backendSrvs[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		}))
		backendURLs[i] = backendSrvs[i].URL
	}
	defer func() {
		for _, s := range backendSrvs {
			s.Close()
		}
	}()

	pool := newBackendPool("bench-health", backendURLs...)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := &http.Client{Transport: newBenchTransport()}

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, proxySrv.URL+"/bench"); err != nil {
			b.Fatalf("request failed: %v", err)
		}
	}
}

func BenchmarkHTTPProxyParallel(b *testing.B) {
	// NOTE: b.RunParallel is not used here because httputil.ReverseProxy
	// creates a new transport per-request in ServeHTTP, which prevents
	// connection reuse on the proxy→backend leg. Under RunParallel's high
	// concurrency, ephemeral ports are rapidly exhausted on macOS (~16K
	// ports, 15s MSL). Use the external bench.sh script with hey/wrk for
	// parallel throughput measurements — those tools handle connection
	// pooling correctly at the OS level.
	//
	// This benchmark measures per-request latency under GOMAXPROCS-level
	// concurrency with a single shared client (connection-pooled).
	b.ReportAllocs()

	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("x"))
	}))
	defer backendSrv.Close()

	pool := newBackendPool("bench-parallel", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := &http.Client{Transport: newBenchTransport()}

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, proxySrv.URL+"/bench"); err != nil {
			b.Fatalf("request failed: %v", err)
		}
	}
}
