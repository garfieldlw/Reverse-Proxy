package integration

import (
	"bufio"
	"context"
	"fmt"
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
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/interop"
	"google.golang.org/grpc/interop/grpc_testing"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func findFreePort(b *testing.B) string {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("find free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// writeConfigYAML marshals cfg to YAML and writes it to a temp file, returning
// the file path. This avoids raw-string YAML indentation bugs.
func writeConfigYAML(b *testing.B, cfg *config.Config) string {
	b.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		b.Fatalf("yaml.Marshal: %v", err)
	}
	tmpDir := b.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		b.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func startServerFromConfig(b *testing.B, cfg *config.Config) (*server.Server, func()) {
	b.Helper()

	cfgPath := writeConfigYAML(b, cfg)

	loaded, err := config.Load(cfgPath)
	if err != nil {
		b.Fatalf("config.Load() error = %v", err)
	}

	if err := logger.Init(loaded.Logging); err != nil {
		b.Fatalf("logger.Init() error = %v", err)
	}

	srv, err := server.NewServer(loaded, slog.Default())
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

	return srv, cleanup
}

func benchRateLimit(enabled bool, rps float64, burst int, perIP bool) config.RateLimitConfig {
	return config.RateLimitConfig{
		Enabled:            enabled,
		RequestsPerSecond:  rps,
		Burst:              burst,
		PerIP:              perIP,
	}
}

func benchHealthCheck() config.HealthCheckConfig {
	return config.HealthCheckConfig{Enabled: false}
}

// ---------------------------------------------------------------------------
// SSE helpers
// ---------------------------------------------------------------------------

func newSSEBackendHandler(n int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: event-%d\n\n", i)
			w.(http.Flusher).Flush()
		}
	})
}

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

func newSSEClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression:  true,
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 512,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 10 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// WebSocket echo handler
// ---------------------------------------------------------------------------

func echoWSHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.WriteMessage(mt, msg)
	}
}

// ---------------------------------------------------------------------------
// SSE E2E benchmarks
// ---------------------------------------------------------------------------

func BenchmarkEndToEndSSEProxy(b *testing.B) {
	b.ReportAllocs()

	backend := httptest.NewServer(newSSEBackendHandler(5))
	defer backend.Close()

	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:     "bench-sse",
					Protocol: "http",
					Listen:   proxyAddr,
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "bench-sse-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-sse-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: backend.URL, Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(false, 0, 0, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	client := newSSEClient()
	url := fmt.Sprintf("http://%s/events", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		if err := readSSEEvents(client, url, 5); err != nil {
			b.Fatalf("SSE read: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkEndToEndSSEProxyMultiBackend(b *testing.B) {
	b.ReportAllocs()

	backend1 := httptest.NewServer(newSSEBackendHandler(5))
	defer backend1.Close()
	backend2 := httptest.NewServer(newSSEBackendHandler(5))
	defer backend2.Close()
	backend3 := httptest.NewServer(newSSEBackendHandler(5))
	defer backend3.Close()

	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:     "bench-sse",
					Protocol: "http",
					Listen:   proxyAddr,
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "bench-sse-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-sse-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: backend1.URL, Weight: 1},
					{URL: backend2.URL, Weight: 1},
					{URL: backend3.URL, Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(false, 0, 0, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	client := newSSEClient()
	url := fmt.Sprintf("http://%s/events", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		if err := readSSEEvents(client, url, 5); err != nil {
			b.Fatalf("SSE read: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkEndToEndSSEProxyWithRateLimit(b *testing.B) {
	b.ReportAllocs()

	backend := httptest.NewServer(newSSEBackendHandler(5))
	defer backend.Close()

	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:     "bench-sse",
					Protocol: "http",
					Listen:   proxyAddr,
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "bench-sse-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-sse-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: backend.URL, Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(true, 10000, 50000, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	client := newSSEClient()
	url := fmt.Sprintf("http://%s/events", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		if err := readSSEEvents(client, url, 5); err != nil {
			b.Fatalf("SSE read: %v", err)
		}
	}
	b.StopTimer()
}

// ---------------------------------------------------------------------------
// WebSocket E2E benchmarks
// ---------------------------------------------------------------------------

func BenchmarkEndToEndWSProxy(b *testing.B) {
	b.ReportAllocs()

	backend := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backend.Close()

	wsBackendURL := "ws://" + strings.TrimPrefix(backend.URL, "http://")
	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:     "bench-ws",
					Protocol: "websocket",
					Listen:   proxyAddr,
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "bench-ws-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-ws-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: wsBackendURL, Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(false, 0, 0, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	wsProxyURL := fmt.Sprintf("ws://%s/ws", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		conn, _, err := websocket.DefaultDialer.Dial(wsProxyURL, nil)
		if err != nil {
			b.Fatalf("WS dial: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("bench")); err != nil {
			b.Fatalf("WS write: %v", err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil {
			b.Fatalf("WS read: %v", err)
		}
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	}
	b.StopTimer()
}

func BenchmarkEndToEndWSProxyMultiBackend(b *testing.B) {
	b.ReportAllocs()

	backend1 := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backend1.Close()
	backend2 := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backend2.Close()
	backend3 := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backend3.Close()

	wsURL1 := "ws://" + strings.TrimPrefix(backend1.URL, "http://")
	wsURL2 := "ws://" + strings.TrimPrefix(backend2.URL, "http://")
	wsURL3 := "ws://" + strings.TrimPrefix(backend3.URL, "http://")

	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:     "bench-ws",
					Protocol: "websocket",
					Listen:   proxyAddr,
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "bench-ws-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-ws-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: wsURL1, Weight: 1},
					{URL: wsURL2, Weight: 1},
					{URL: wsURL3, Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(false, 0, 0, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	wsProxyURL := fmt.Sprintf("ws://%s/ws", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		conn, _, err := websocket.DefaultDialer.Dial(wsProxyURL, nil)
		if err != nil {
			b.Fatalf("WS dial: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("bench")); err != nil {
			b.Fatalf("WS write: %v", err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil {
			b.Fatalf("WS read: %v", err)
		}
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	}
	b.StopTimer()
}

func BenchmarkEndToEndWSProxyWithRateLimit(b *testing.B) {
	b.ReportAllocs()

	backend := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backend.Close()

	wsBackendURL := "ws://" + strings.TrimPrefix(backend.URL, "http://")
	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:     "bench-ws",
					Protocol: "websocket",
					Listen:   proxyAddr,
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "bench-ws-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-ws-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: wsBackendURL, Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(true, 10000, 50000, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	wsProxyURL := fmt.Sprintf("ws://%s/ws", proxyAddr)

	b.ResetTimer()
	for b.Loop() {
		conn, _, err := websocket.DefaultDialer.Dial(wsProxyURL, nil)
		if err != nil {
			b.Fatalf("WS dial: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("bench")); err != nil {
			b.Fatalf("WS write: %v", err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := conn.ReadMessage(); err != nil {
			b.Fatalf("WS read: %v", err)
		}
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	}
	b.StopTimer()
}

// ---------------------------------------------------------------------------
// gRPC E2E benchmarks
// ---------------------------------------------------------------------------

func BenchmarkEndToEndGRPCProxy(b *testing.B) {
	b.ReportAllocs()

	backendSrv := grpc.NewServer()
	grpc_testing.RegisterTestServiceServer(backendSrv, interop.NewTestServer())
	backendLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	go backendSrv.Serve(backendLis)
	defer backendSrv.Stop()

	backendAddr := backendLis.Addr().String()
	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:        "bench-grpc",
					Protocol:    "grpc",
					Listen:      proxyAddr,
					BackendPool: "bench-grpc-pool",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-grpc-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: "grpc://" + backendAddr, Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(false, 0, 0, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	cc, err := grpc.NewClient(proxyAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		b.Fatalf("dial proxy: %v", err)
	}
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)

	b.ResetTimer()
	for b.Loop() {
		_, err := client.EmptyCall(context.Background(), &grpc_testing.Empty{})
		if err != nil {
			b.Fatalf("EmptyCall: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkEndToEndGRPCProxyMultiBackend(b *testing.B) {
	b.ReportAllocs()

	var backendAddrs []string
	var backendSrvs []*grpc.Server

	for i := 0; i < 3; i++ {
		srv := grpc.NewServer()
		grpc_testing.RegisterTestServiceServer(srv, interop.NewTestServer())
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatalf("listen: %v", err)
		}
		go srv.Serve(lis)
		backendSrvs = append(backendSrvs, srv)
		backendAddrs = append(backendAddrs, lis.Addr().String())
	}
	defer func() {
		for _, s := range backendSrvs {
			s.Stop()
		}
	}()

	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:        "bench-grpc",
					Protocol:    "grpc",
					Listen:      proxyAddr,
					BackendPool: "bench-grpc-pool",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-grpc-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: "grpc://" + backendAddrs[0], Weight: 1},
					{URL: "grpc://" + backendAddrs[1], Weight: 1},
					{URL: "grpc://" + backendAddrs[2], Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(false, 0, 0, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	cc, err := grpc.NewClient(proxyAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		b.Fatalf("dial proxy: %v", err)
	}
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)

	b.ResetTimer()
	for b.Loop() {
		_, err := client.EmptyCall(context.Background(), &grpc_testing.Empty{})
		if err != nil {
			b.Fatalf("EmptyCall: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkEndToEndGRPCProxyWithRateLimit(b *testing.B) {
	b.ReportAllocs()

	backendSrv := grpc.NewServer()
	grpc_testing.RegisterTestServiceServer(backendSrv, interop.NewTestServer())
	backendLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	go backendSrv.Serve(backendLis)
	defer backendSrv.Stop()

	backendAddr := backendLis.Addr().String()
	proxyAddr := findFreePort(b)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:        "bench-grpc",
					Protocol:    "grpc",
					Listen:      proxyAddr,
					BackendPool: "bench-grpc-pool",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:        "bench-grpc-pool",
				Balancer:    "round_robin",
				HealthCheck: benchHealthCheck(),
				Backends: []config.BackendConfig{
					{URL: "grpc://" + backendAddr, Weight: 1},
				},
			},
		},
		RateLimit: benchRateLimit(true, 10000, 50000, false),
		Logging:   config.LoggingConfig{Level: "error", Format: "json", Output: "stdout"},
	}

	_, cleanup := startServerFromConfig(b, cfg)
	defer cleanup()

	cc, err := grpc.NewClient(proxyAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		b.Fatalf("dial proxy: %v", err)
	}
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)

	b.ResetTimer()
	for b.Loop() {
		_, err := client.EmptyCall(context.Background(), &grpc_testing.Empty{})
		if err != nil {
			b.Fatalf("EmptyCall: %v", err)
		}
	}
	b.StopTimer()
}
