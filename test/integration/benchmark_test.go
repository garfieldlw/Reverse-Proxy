package integration

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/garfieldlw/reverse-proxy/internal/logger"
	"github.com/garfieldlw/reverse-proxy/internal/server"
)

func setupProxyServer(b *testing.B, backendURL string, rateLimitEnabled bool, requestsPerSecond float64, burst int, perIP bool) (string, *server.Server, func()) {
	b.Helper()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("find free port: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	var rateLimitYAML string
	if rateLimitEnabled {
		perIPStr := "false"
		if perIP {
			perIPStr = "true"
		}
		rateLimitYAML = fmt.Sprintf(`rate_limit:
  enabled: true
  requests_per_second: %.1f
  burst: %d
  per_ip: %s
`, requestsPerSecond, burst, perIPStr)
	} else {
		rateLimitYAML = `rate_limit:
  enabled: false
`
	}

	cfgContent := fmt.Sprintf(`server:
  listeners:
    - name: "bench-http"
      protocol: "http"
      listen: "%s"
      routes:
        - match: "/"
          backend_pool: "bench-pool"
backend_pools:
  - name: "bench-pool"
    balancer: "round_robin"
    health_check:
      enabled: false
    backends:
      - url: "%s"
        weight: 1
`+rateLimitYAML+`logging:
  level: "error"
  format: "json"
  output: "stdout"
`, proxyAddr, backendURL)

	tmpDir := b.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		b.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		b.Fatalf("config.Load() error = %v", err)
	}

	if err := logger.Init(cfg.Logging); err != nil {
		b.Fatalf("logger.Init() error = %v", err)
	}

	srv, err := server.NewServer(cfg, slog.Default())
	if err != nil {
		b.Fatalf("server.NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		b.Fatalf("server.Start() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	cleanup := func() {
		if err := srv.Shutdown(5 * time.Second); err != nil {
			b.Logf("server.Shutdown() error = %v", err)
		}
	}

	return proxyAddr, srv, cleanup
}

func newBenchTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
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

func BenchmarkEndToEndHTTPProxy(b *testing.B) {
	b.ReportAllocs()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from backend"))
	}))
	defer backend.Close()

	proxyAddr, _, cleanup := setupProxyServer(b, backend.URL, false, 0, 0, false)
	defer cleanup()

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: newBenchTransport(),
	}
	url := fmt.Sprintf("http://%s/", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, url); err != nil {
			b.Fatalf("GET through proxy: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkEndToEndHTTPProxyMultiBackend(b *testing.B) {
	b.ReportAllocs()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	backend1 := httptest.NewServer(handler)
	defer backend1.Close()
	backend2 := httptest.NewServer(handler)
	defer backend2.Close()
	backend3 := httptest.NewServer(handler)
	defer backend3.Close()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("find free port: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	cfgContent := fmt.Sprintf(`server:
  listeners:
    - name: "bench-http"
      protocol: "http"
      listen: "%s"
      routes:
        - match: "/"
          backend_pool: "bench-pool"
backend_pools:
  - name: "bench-pool"
    balancer: "round_robin"
    health_check:
      enabled: false
    backends:
      - url: "%s"
        weight: 1
      - url: "%s"
        weight: 1
      - url: "%s"
        weight: 1
rate_limit:
  enabled: false
logging:
  level: "error"
  format: "json"
  output: "stdout"
`, proxyAddr, backend1.URL, backend2.URL, backend3.URL)

	tmpDir := b.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		b.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		b.Fatalf("config.Load() error = %v", err)
	}

	if err := logger.Init(cfg.Logging); err != nil {
		b.Fatalf("logger.Init() error = %v", err)
	}

	srv, err := server.NewServer(cfg, slog.Default())
	if err != nil {
		b.Fatalf("server.NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		b.Fatalf("server.Start() error = %v", err)
	}
	defer func() {
		if err := srv.Shutdown(5 * time.Second); err != nil {
			b.Logf("server.Shutdown() error = %v", err)
		}
	}()

	time.Sleep(200 * time.Millisecond)

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: newBenchTransport(),
	}
	url := fmt.Sprintf("http://%s/", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, url); err != nil {
			b.Fatalf("GET through proxy: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkEndToEndHTTPProxyBodySizes(b *testing.B) {
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

			body := strings.Repeat("x", sz.n)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(body))
			}))
			defer backend.Close()

			proxyAddr, _, cleanup := setupProxyServer(b, backend.URL, false, 0, 0, false)
			defer cleanup()

			client := &http.Client{
				Timeout:   10 * time.Second,
				Transport: newBenchTransport(),
			}
			url := fmt.Sprintf("http://%s/", proxyAddr)

			b.ResetTimer()
			for b.Loop() {
				if err := benchGet(client, url); err != nil {
					b.Fatalf("GET through proxy: %v", err)
				}
			}
			b.StopTimer()
		})
	}
}

func BenchmarkEndToEndHTTPProxyWithRateLimit(b *testing.B) {
	b.ReportAllocs()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	proxyAddr, _, cleanup := setupProxyServer(b, backend.URL, true, 10000.0, 50000, true)
	defer cleanup()

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: newBenchTransport(),
	}
	url := fmt.Sprintf("http://%s/", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, url); err != nil {
			b.Fatalf("GET through proxy: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkEndToEndHTTPProxyParallel(b *testing.B) {
	// b.RunParallel is not used because httputil.ReverseProxy creates a new
	// transport per-request, preventing proxy->backend connection reuse.
	// RunParallel exhausts macOS ephemeral ports (~16K, 15s MSL).
	// Use bench.sh with hey/wrk for parallel throughput measurement.
	b.ReportAllocs()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	proxyAddr, _, cleanup := setupProxyServer(b, backend.URL, false, 0, 0, false)
	defer cleanup()

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: newBenchTransport(),
	}
	url := fmt.Sprintf("http://%s/", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		if err := benchGet(client, url); err != nil {
			b.Fatalf("GET through proxy: %v", err)
		}
	}
	b.StopTimer()
}
