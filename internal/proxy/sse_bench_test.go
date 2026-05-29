package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/garfieldlw/reverse-proxy/internal/middleware/ratelimit"
)

// newSSEBackendHandler returns an http.Handler that sends n SSE events with the
// given data payload, flushing after each event so httputil.ReverseProxy
// forwards data promptly under default buffering.
func newSSEBackendHandler(n int, data []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
	})
}

// newSSEClient returns an *http.Client with compression disabled so SSE
// streams are not buffered by gzip transport encoding.
func newSSEClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression:  true,
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 512,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// readSSEEvents opens url, reads the response body line-by-line, and counts
// lines prefixed with "data:". Returns once targetEvents have been seen or the
// body is exhausted.
func readSSEEvents(client *http.Client, url string, targetEvents int) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(resp.Body)
	eventsRead := 0
	for eventsRead < targetEvents {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "data:") {
			eventsRead++
		}
	}
	resp.Body.Close()
	return nil
}

func BenchmarkSSEProxyConnectionEstablishment(b *testing.B) {
	b.ReportAllocs()

	backendSrv := httptest.NewServer(newSSEBackendHandler(1, []byte("hello")))
	defer backendSrv.Close()

	pool := newBackendPool("sse-conn", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := newSSEClient()

	b.ResetTimer()
	for b.Loop() {
		if err := readSSEEvents(client, proxySrv.URL+"/sse", 1); err != nil {
			b.Fatalf("sse read failed: %v", err)
		}
	}
}

func BenchmarkSSEProxyEventThroughput(b *testing.B) {
	b.ReportAllocs()

	backendSrv := httptest.NewServer(newSSEBackendHandler(10, []byte("event-data")))
	defer backendSrv.Close()

	pool := newBackendPool("sse-throughput", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := newSSEClient()

	b.ResetTimer()
	for b.Loop() {
		if err := readSSEEvents(client, proxySrv.URL+"/sse", 10); err != nil {
			b.Fatalf("sse read failed: %v", err)
		}
	}
}

func BenchmarkSSEProxyEventSizes(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{
		{"64B", 64},
		{"1KB", 1024},
		{"10KB", 10 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()

			data := bytes.Repeat([]byte("x"), sz.n)
			backendSrv := httptest.NewServer(newSSEBackendHandler(5, data))
			defer backendSrv.Close()

			pool := newBackendPool("sse-size", backendSrv.URL)
			rr := &balancer.RoundRobin{}
			logger := slog.Default()

			proxy := NewHTTPProxy(pool, rr, nil, logger)
			proxySrv := httptest.NewServer(proxy)
			defer proxySrv.Close()

			client := newSSEClient()

			b.ResetTimer()
			for b.Loop() {
				if err := readSSEEvents(client, proxySrv.URL+"/sse", 5); err != nil {
					b.Fatalf("sse read failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkSSEProxyMultiBackend(b *testing.B) {
	b.ReportAllocs()

	backendSrvs := make([]*httptest.Server, 3)
	backendURLs := make([]string, 3)
	for i := range 3 {
		backendSrvs[i] = httptest.NewServer(newSSEBackendHandler(1, []byte("ok")))
		backendURLs[i] = backendSrvs[i].URL
	}
	defer func() {
		for _, s := range backendSrvs {
			s.Close()
		}
	}()

	pool := newBackendPool("sse-multi", backendURLs...)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewHTTPProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := newSSEClient()

	b.ResetTimer()
	for b.Loop() {
		if err := readSSEEvents(client, proxySrv.URL+"/sse", 1); err != nil {
			b.Fatalf("sse read failed: %v", err)
		}
	}
}

func BenchmarkSSEProxyWithRateLimiter(b *testing.B) {
	b.ReportAllocs()

	backendSrv := httptest.NewServer(newSSEBackendHandler(1, []byte("hello")))
	defer backendSrv.Close()

	pool := newBackendPool("sse-ratelimit", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	cfg := config.RateLimitConfig{
		Enabled:           true,
		RequestsPerSecond: 10000,
		Burst:             50000,
		PerIP:             false,
	}
	limiter := ratelimit.New(cfg, logger)

	proxy := NewHTTPProxy(pool, rr, limiter, logger)
	proxySrv := httptest.NewServer(proxy.Handler())
	defer proxySrv.Close()

	client := newSSEClient()

	b.ResetTimer()
	for b.Loop() {
		if err := readSSEEvents(client, proxySrv.URL+"/sse", 1); err != nil {
			b.Fatalf("sse read failed: %v", err)
		}
	}
}
