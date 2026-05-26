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
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/garfieldlw/reverse-proxy/internal/logger"
	"github.com/garfieldlw/reverse-proxy/internal/server"
)

// TestHTTPProxyEndToEnd verifies the full wiring: YAML config → config.Load →
// server.NewServer → Start → HTTP request through proxy → backend response → Shutdown.
func TestHTTPProxyEndToEnd(t *testing.T) {
	responseBody := "hello from backend"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(responseBody))
	}))
	defer backend.Close()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	cfgContent := fmt.Sprintf(`server:
  listeners:
    - name: "test-http"
      protocol: "http"
      listen: "%s"
      routes:
        - match: "/"
          backend_pool: "test-pool"
backend_pools:
  - name: "test-pool"
    balancer: "round_robin"
    health_check:
      enabled: false
    backends:
      - url: "%s"
        weight: 1
rate_limit:
  enabled: false
logging:
  level: "info"
  format: "json"
  output: "stdout"
`, proxyAddr, backend.URL)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	if err := logger.Init(cfg.Logging); err != nil {
		t.Fatalf("logger.Init() error = %v", err)
	}

	srv, err := server.NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatalf("server.NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("server.Start() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/", proxyAddr))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if string(body) != responseBody {
		t.Errorf("expected body %q, got %q", responseBody, string(body))
	}

	if err := srv.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("server.Shutdown() error = %v", err)
	}
}

// TestHTTPProxyWithRateLimit verifies that rate limiting is wired correctly
// by sending requests through a rate-limited proxy.
func TestHTTPProxyWithRateLimit(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	proxyAddr := proxyLn.Addr().String()
	proxyLn.Close()

	cfgContent := fmt.Sprintf(`server:
  listeners:
    - name: "ratelimit-http"
      protocol: "http"
      listen: "%s"
      routes:
        - match: "/"
          backend_pool: "rl-pool"
backend_pools:
  - name: "rl-pool"
    balancer: "round_robin"
    health_check:
      enabled: false
    backends:
      - url: "%s"
        weight: 1
rate_limit:
  enabled: true
  requests_per_second: 1000.0
  burst: 5
  per_ip: true
logging:
  level: "warn"
  format: "json"
  output: "stdout"
`, proxyAddr, backend.URL)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	if err := logger.Init(cfg.Logging); err != nil {
		t.Fatalf("logger.Init() error = %v", err)
	}

	srv, err := server.NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatalf("server.NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("server.Start() error = %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/", proxyAddr))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	if err := srv.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("server.Shutdown() error = %v", err)
	}
}

// TestUDPProxyEndToEnd verifies the full wiring for UDP: YAML config →
// config.Load → server.NewServer → Start → UDP packet through proxy →
// backend echo → Shutdown.
func TestUDPProxyEndToEnd(t *testing.T) {
	echoConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoConn.Close()

	backendAddr := echoConn.LocalAddr().String()

	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := echoConn.ReadFrom(buf)
			if err != nil {
				return
			}
			echoConn.WriteTo(buf[:n], addr)
		}
	}()

	proxyAddr := "127.0.0.1:38297"

	cfgContent := fmt.Sprintf(`server:
  listeners:
    - name: "udp-test"
      protocol: "udp"
      listen: "%s"
      backend_pool: "udp-test-pool"
backend_pools:
  - name: "udp-test-pool"
    balancer: "round_robin"
    health_check:
      enabled: false
    backends:
      - url: "udp://%s"
        weight: 1
rate_limit:
  enabled: false
logging:
  level: "info"
  format: "json"
  output: "stdout"
`, proxyAddr, backendAddr)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	if err := logger.Init(cfg.Logging); err != nil {
		t.Fatalf("logger.Init() error = %v", err)
	}

	srv, err := server.NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatalf("server.NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("server.Start() error = %v", err)
	}
	defer srv.Shutdown(5 * time.Second)

	time.Sleep(200 * time.Millisecond)

	clientConn, err := net.Dial("udp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer clientConn.Close()

	if err := clientConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	msg := []byte("hello udp proxy")
	if _, err := clientConn.Write(msg); err != nil {
		t.Fatalf("write to proxy: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("read from proxy: %v", err)
	}

	if string(buf[:n]) != string(msg) {
		t.Errorf("expected %q, got %q", string(msg), string(buf[:n]))
	}
}

// TestRPCProxyEndToEnd verifies the full wiring for JSON-RPC: YAML config →
// config.Load → server.NewServer → Start → TCP echo through proxy → Shutdown.
func TestRPCProxyEndToEnd(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()

	backendAddr := echoLn.Addr().String()

	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	proxyAddr := "127.0.0.1:38298"

	cfgContent := fmt.Sprintf(`server:
  listeners:
    - name: "rpc-test"
      protocol: "rpc"
      listen: "%s"
      backend_pool: "rpc-test-pool"
backend_pools:
  - name: "rpc-test-pool"
    balancer: "round_robin"
    health_check:
      enabled: false
    backends:
      - url: "rpc://%s"
        weight: 1
rate_limit:
  enabled: false
logging:
  level: "info"
  format: "json"
  output: "stdout"
`, proxyAddr, backendAddr)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	if err := logger.Init(cfg.Logging); err != nil {
		t.Fatalf("logger.Init() error = %v", err)
	}

	srv, err := server.NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatalf("server.NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("server.Start() error = %v", err)
	}
	defer srv.Shutdown(5 * time.Second)

	time.Sleep(200 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	msg := []byte(`{"jsonrpc":"2.0","method":"test","id":1}`)
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write to proxy: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read from proxy: %v", err)
	}

	if string(buf[:n]) != string(msg) {
		t.Errorf("expected %q, got %q", string(msg), string(buf[:n]))
	}
}

// TestSocketProxyEndToEnd verifies the full wiring for Unix domain socket:
// YAML config → config.Load → server.NewServer → Start → socket echo through
// proxy → Shutdown.
func TestSocketProxyEndToEnd(t *testing.T) {
	dir := t.TempDir()

	backendSock := filepath.Join(dir, "backend.sock")
	echoLn, err := net.Listen("unix", backendSock)
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()

	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	proxySock := filepath.Join(dir, "proxy.sock")
	cfgContent := fmt.Sprintf(`server:
  listeners:
    - name: "socket-test"
      protocol: "socket"
      listen: "%s"
      backend_pool: "socket-test-pool"
backend_pools:
  - name: "socket-test-pool"
    balancer: "round_robin"
    health_check:
      enabled: false
    backends:
      - url: "unix:%s"
        weight: 1
rate_limit:
  enabled: false
logging:
  level: "info"
  format: "json"
  output: "stdout"
`, proxySock, backendSock)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}

	if err := logger.Init(cfg.Logging); err != nil {
		t.Fatalf("logger.Init() error = %v", err)
	}

	srv, err := server.NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatalf("server.NewServer() error = %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("server.Start() error = %v", err)
	}
	defer srv.Shutdown(5 * time.Second)

	time.Sleep(200 * time.Millisecond)

	conn, err := net.DialTimeout("unix", proxySock, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy socket: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	msg := []byte("hello socket proxy")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write to proxy: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read from proxy: %v", err)
	}

	if string(buf[:n]) != string(msg) {
		t.Errorf("expected %q, got %q", string(msg), string(buf[:n]))
	}
}

// TestConfigLoadFromExampleYAML verifies that the example config file
// can be parsed and validated without errors.
func TestConfigLoadFromExampleYAML(t *testing.T) {
	projectRoot := filepath.Join("..", "..")
	cfgPath := filepath.Join(projectRoot, "config.example.yaml")

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		t.Skipf("config.example.yaml not found at %s", cfgPath)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(config.example.yaml) error = %v", err)
	}

	if len(cfg.BackendPools) == 0 {
		t.Error("expected at least one backend pool")
	}
	if len(cfg.Server.Listeners) == 0 {
		t.Error("expected at least one listener")
	}

	protocols := map[string]bool{}
	for _, l := range cfg.Server.Listeners {
		protocols[l.Protocol] = true
	}
	for _, p := range []string{"http", "websocket", "tcp", "grpc", "socket", "udp", "rpc"} {
		if !protocols[p] {
			t.Errorf("expected listener with protocol %q in example config", p)
		}
	}

	balancers := map[string]bool{}
	for _, pool := range cfg.BackendPools {
		balancers[pool.Balancer] = true
	}
	for _, b := range []string{"round_robin", "weighted_round_robin", "least_connections", "random"} {
		if !balancers[b] {
			t.Errorf("expected backend pool with balancer %q in example config", b)
		}
	}

	if !cfg.RateLimit.Enabled {
		t.Error("expected rate_limit.enabled = true in example config")
	}

	if cfg.Logging.Level != "info" {
		t.Errorf("expected logging.level = info, got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("expected logging.format = json, got %q", cfg.Logging.Format)
	}
}
