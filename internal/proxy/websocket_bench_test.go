package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/garfieldlw/reverse-proxy/internal/middleware/ratelimit"
	"github.com/gorilla/websocket"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

func benchWSEcho(wsURL string, msg []byte) error {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	return nil
}

func BenchmarkWSProxySingleBackend(b *testing.B) {
	b.ReportAllocs()

	backendSrv := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backendSrv.Close()

	pool := newBackendPool("bench-ws-single", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewWSProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	wsURL := "ws" + strings.TrimPrefix(proxySrv.URL, "http") + "/ws"

	b.ResetTimer()
	for b.Loop() {
		if err := benchWSEcho(wsURL, []byte("x")); err != nil {
			b.Fatalf("echo failed: %v", err)
		}
	}
}

func BenchmarkWSProxyMultiMessage(b *testing.B) {
	b.ReportAllocs()

	backendSrv := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backendSrv.Close()

	pool := newBackendPool("bench-ws-multi-msg", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewWSProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	wsURL := "ws" + strings.TrimPrefix(proxySrv.URL, "http") + "/ws"

	b.ResetTimer()
	for b.Loop() {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		msg := []byte("hello")
		for i := 0; i < 10; i++ {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				b.Fatalf("write: %v", err)
			}
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, _, err := conn.ReadMessage(); err != nil {
				b.Fatalf("read: %v", err)
			}
		}
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	}
}

func BenchmarkWSProxyMessageSizes(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{
		{"1B", 1},
		{"1KB", 1024},
		{"10KB", 10 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()

			payload := bytes.Repeat([]byte("x"), sz.n)

			backendSrv := httptest.NewServer(http.HandlerFunc(echoWSHandler))
			defer backendSrv.Close()

			pool := newBackendPool("bench-ws-size", backendSrv.URL)
			rr := &balancer.RoundRobin{}
			logger := slog.Default()

			proxy := NewWSProxy(pool, rr, nil, logger)
			proxySrv := httptest.NewServer(proxy)
			defer proxySrv.Close()

			wsURL := "ws" + strings.TrimPrefix(proxySrv.URL, "http") + "/ws"

			b.ResetTimer()
			for b.Loop() {
				if err := benchWSEcho(wsURL, payload); err != nil {
					b.Fatalf("echo failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkWSProxyMultiBackend(b *testing.B) {
	b.ReportAllocs()

	var hits [3]atomic.Int64

	backendSrvs := make([]*httptest.Server, 3)
	backendURLs := make([]string, 3)
	for i := range 3 {
		idx := i
		backendSrvs[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			hits[idx].Add(1)
			for {
				mt, msg, err := conn.ReadMessage()
				if err != nil {
					break
				}
				conn.WriteMessage(mt, msg)
			}
		}))
		backendURLs[i] = backendSrvs[i].URL
	}
	defer func() {
		for _, s := range backendSrvs {
			s.Close()
		}
	}()

	pool := newBackendPool("bench-ws-multi", backendURLs...)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewWSProxy(pool, rr, nil, logger)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	wsURL := "ws" + strings.TrimPrefix(proxySrv.URL, "http") + "/ws"

	b.ResetTimer()
	for b.Loop() {
		if err := benchWSEcho(wsURL, []byte("x")); err != nil {
			b.Fatalf("echo failed: %v", err)
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

func BenchmarkWSProxyWithRateLimiter(b *testing.B) {
	b.ReportAllocs()

	backendSrv := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backendSrv.Close()

	pool := newBackendPool("bench-ws-ratelimit", backendSrv.URL)
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	cfg := config.RateLimitConfig{
		Enabled:           true,
		RequestsPerSecond: 10000,
		Burst:             50000,
		PerIP:             false,
	}
	limiter := ratelimit.New(cfg, logger)

	proxy := NewWSProxy(pool, rr, limiter, logger)
	proxySrv := httptest.NewServer(proxy.Handler())
	defer proxySrv.Close()

	wsURL := "ws" + strings.TrimPrefix(proxySrv.URL, "http") + "/ws"

	b.ResetTimer()
	for b.Loop() {
		if err := benchWSEcho(wsURL, []byte("x")); err != nil {
			b.Fatalf("echo failed: %v", err)
		}
	}
}
