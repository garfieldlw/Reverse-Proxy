package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

func TestNewServer(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "http-test",
					Protocol:   "http",
					Listen:     ":0",
					BackendPool: "test-pool",
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "test-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "test-pool",
				Balancer: "round_robin",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:0", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{
			Enabled:  false,
			Burst:    200,
		},
		Logging: config.LoggingConfig{
			Level:  "info",
			Format: "json",
			Output: "stdout",
		},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if srv == nil {
		t.Fatal("NewServer() returned nil server")
	}
	if len(srv.pools) != 1 {
		t.Errorf("expected 1 pool, got %d", len(srv.pools))
	}
	if _, ok := srv.pools["test-pool"]; !ok {
		t.Error("expected pool 'test-pool' to exist")
	}
	if len(srv.balancers) != 1 {
		t.Errorf("expected 1 balancer, got %d", len(srv.balancers))
	}
	if len(srv.httpServers) != 1 {
		t.Errorf("expected 1 http server, got %d", len(srv.httpServers))
	}
}

func TestNewServerWithHealthCheck(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "http-hc",
					Protocol:   "http",
					Listen:     ":0",
					BackendPool: "hc-pool",
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "hc-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "hc-pool",
				Balancer: "round_robin",
				HealthCheck: config.HealthCheckConfig{
					Enabled:            true,
					Interval:           "10s",
					Timeout:            "5s",
					Path:               "/health",
					UnhealthyThreshold: 3,
					HealthyThreshold:   2,
				},
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:1", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if len(srv.healthCheckers) != 1 {
		t.Errorf("expected 1 health checker, got %d", len(srv.healthCheckers))
	}

	// Shutdown to stop health checkers.
	if err := srv.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestNewServerInvalidBalancer(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "http-bad",
					Protocol:   "http",
					Listen:     ":0",
					BackendPool: "bad-pool",
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "bad-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "bad-pool",
				Balancer: "invalid_strategy",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:0", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	_, err := NewServer(cfg, logger)
	if err == nil {
		t.Fatal("expected error for invalid balancer strategy, got nil")
	}
}

func TestNewServerUnknownPool(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "http-unknown",
					Protocol:   "http",
					Listen:     ":0",
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "nonexistent-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "real-pool",
				Balancer: "round_robin",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:0", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	_, err := NewServer(cfg, logger)
	if err == nil {
		t.Fatal("expected error for unknown pool reference, got nil")
	}
}

func TestNewServerDefaultListener(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: ":0",
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "default-pool",
				Balancer: "round_robin",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:0", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if len(srv.httpServers) != 1 {
		t.Errorf("expected 1 default http server, got %d", len(srv.httpServers))
	}
}

func TestNewServerWebSocket(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "ws-test",
					Protocol:   "websocket",
					Listen:     ":0",
					BackendPool: "ws-pool",
					Routes: []config.RouteConfig{
						{Match: "/ws", BackendPool: "ws-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "ws-pool",
				Balancer: "random",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:0", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if len(srv.httpServers) != 1 {
		t.Errorf("expected 1 http server for websocket, got %d", len(srv.httpServers))
	}
}

func TestNewServerTCPListener(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "tcp-test",
					Protocol:   "tcp",
					Listen:     "127.0.0.1:0",
					BackendPool: "tcp-pool",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "tcp-pool",
				Balancer: "least_connections",
				Backends: []config.BackendConfig{
					{URL: "tcp://127.0.0.1:0", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if len(srv.tcpProxies) != 1 {
		t.Errorf("expected 1 tcp proxy, got %d", len(srv.tcpProxies))
	}
}

func TestNewServerGRPCListener(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "grpc-test",
					Protocol:   "grpc",
					Listen:     "127.0.0.1:0",
					BackendPool: "grpc-pool",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "grpc-pool",
				Balancer: "weighted_round_robin",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:0", Weight: 2},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if len(srv.grpcServers) != 1 {
		t.Errorf("expected 1 grpc server, got %d", len(srv.grpcServers))
	}
}

func TestServerShutdown(t *testing.T) {
	logger := slog.Default()

	// Start a real backend so the proxy can actually connect.
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backendSrv.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "shutdown-test",
					Protocol:   "http",
					Listen:     "127.0.0.1:0",
					BackendPool: "shutdown-pool",
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "shutdown-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "shutdown-pool",
				Balancer: "round_robin",
				Backends: []config.BackendConfig{
					{URL: backendSrv.URL, Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	// Shutdown without starting should also work cleanly.
	if err := srv.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestServerStartAndShutdown(t *testing.T) {
	logger := slog.Default()

	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backendSrv.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "start-stop",
					Protocol:   "http",
					Listen:     "127.0.0.1:0",
					BackendPool: "start-pool",
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "start-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "start-pool",
				Balancer: "round_robin",
				Backends: []config.BackendConfig{
					{URL: backendSrv.URL, Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Give listeners a moment to bind.
	time.Sleep(100 * time.Millisecond)

	if err := srv.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestServerDoubleStart(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:       "double-start",
					Protocol:   "http",
					Listen:     "127.0.0.1:0",
					BackendPool: "ds-pool",
					Routes: []config.RouteConfig{
						{Match: "/", BackendPool: "ds-pool"},
					},
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "ds-pool",
				Balancer: "round_robin",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:1", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}

	if err := srv.Start(); err == nil {
		t.Fatal("expected error on double start, got nil")
	}

	// Clean up.
	srv.Shutdown(5 * time.Second)
}

func TestNewServerTCPMissingPool(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:     "tcp-no-pool",
					Protocol: "tcp",
					Listen:   "127.0.0.1:0",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:     "some-pool",
				Balancer: "round_robin",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:0", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	_, err := NewServer(cfg, logger)
	if err == nil {
		t.Fatal("expected error for TCP listener without backend_pool, got nil")
	}
}

func TestNewServerGRPCMissingPool(t *testing.T) {
	logger := slog.Default()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:     "grpc-no-pool",
					Protocol: "grpc",
					Listen:   "127.0.0.1:0",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:      "some-pool",
				Balancer:  "round_robin",
				Backends: []config.BackendConfig{
					{URL: "http://127.0.0.1:0", Weight: 1},
				},
			},
		},
		RateLimit: config.RateLimitConfig{Enabled: false, Burst: 200},
		Logging:   config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	_, err := NewServer(cfg, logger)
	if err == nil {
		t.Fatal("expected error for gRPC listener without backend_pool, got nil")
	}
}

func TestNewServerSocketListener(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "proxy.sock")

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:        "test-socket",
					Protocol:    "socket",
					Listen:      socketPath,
					BackendPool: "socket-pool",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:      "socket-pool",
				Balancer:  "round_robin",
				Backends: []config.BackendConfig{
					{URL: "unix:/tmp/backend.sock", Weight: 1},
				},
			},
		},
		Logging: config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.socketProxies) != 1 {
		t.Error("expected 1 socket proxy")
	}
	srv.Shutdown(5 * time.Second)
}

func TestNewServerUDPListener(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:        "test-udp",
					Protocol:    "udp",
					Listen:      "127.0.0.1:0",
					BackendPool: "udp-pool",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:      "udp-pool",
				Balancer:  "random",
				Backends: []config.BackendConfig{
					{URL: "udp://127.0.0.1:8126", Weight: 1},
				},
			},
		},
		Logging: config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.udpProxies) != 1 {
		t.Error("expected 1 udp proxy")
	}
	srv.Shutdown(5 * time.Second)
}

func TestNewServerRPCListener(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{
					Name:        "test-rpc",
					Protocol:    "rpc",
					Listen:      "127.0.0.1:0",
					BackendPool: "rpc-pool",
				},
			},
		},
		BackendPools: []config.BackendPoolConfig{
			{
				Name:      "rpc-pool",
				Balancer:  "round_robin",
				Backends: []config.BackendConfig{
					{URL: "rpc://127.0.0.1:9001", Weight: 1},
				},
			},
		},
		Logging: config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	srv, err := NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.rpcProxies) != 1 {
		t.Error("expected 1 rpc proxy")
	}
	srv.Shutdown(5 * time.Second)
}

func TestNewServerSocketMissingPool(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{Name: "bad-socket", Protocol: "socket", Listen: "/tmp/test.sock"},
			},
		},
		Logging: config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	_, err := NewServer(cfg, slog.Default())
	if err == nil {
		t.Error("expected error for missing backend_pool")
	}
}

func TestNewServerUDPMissingPool(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{Name: "bad-udp", Protocol: "udp", Listen: ":8125"},
			},
		},
		Logging: config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	_, err := NewServer(cfg, slog.Default())
	if err == nil {
		t.Error("expected error for missing backend_pool")
	}
}

func TestNewServerRPCMissingPool(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listeners: []config.ListenerConfig{
				{Name: "bad-rpc", Protocol: "rpc", Listen: ":9000"},
			},
		},
		Logging: config.LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}

	_, err := NewServer(cfg, slog.Default())
	if err == nil {
		t.Error("expected error for missing backend_pool")
	}
}
