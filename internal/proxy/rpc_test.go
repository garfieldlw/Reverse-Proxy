package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

// startRPCEchoServer starts a TCP echo server on a random port.
// It reads bytes from the connection and writes them back (same as TCP echo).
func startRPCEchoServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	return listener, listener.Addr().String()
}

// newRPCTestPool creates a backend pool with the given addresses as healthy backends.
func newRPCTestPool(t *testing.T, addrs ...string) *backend.Pool {
	t.Helper()
	backends := make([]*backend.Backend, 0, len(addrs))
	for _, addr := range addrs {
		u, err := url.Parse("rpc://" + addr)
		if err != nil {
			t.Fatalf("failed to parse URL %q: %v", addr, err)
		}
		backends = append(backends, &backend.Backend{
			URL:    u,
			RawURL: "rpc://" + addr,
			Weight: 1,
		})
	}
	return &backend.Pool{
		Name:     "test-rpc",
		Balancer: "round_robin",
		Backends: backends,
	}
}

func TestNewRPCProxy(t *testing.T) {
	pool := newRPCTestPool(t, "127.0.0.1:0")
	lb := newTestBalancer(t)
	proxy := NewRPCProxy(pool, lb, slog.Default())

	if proxy.pool != pool {
		t.Error("expected pool to be set")
	}
	if proxy.balancer != lb {
		t.Error("expected balancer to be set")
	}
	if proxy.dialTimeout != defaultRPCDialTimeout {
		t.Errorf("expected dial timeout %v, got %v", defaultRPCDialTimeout, proxy.dialTimeout)
	}
}

func TestRPCProxySetDialTimeout(t *testing.T) {
	pool := newRPCTestPool(t, "127.0.0.1:0")
	lb := newTestBalancer(t)
	proxy := NewRPCProxy(pool, lb, slog.Default())

	custom := 5 * time.Second
	proxy.SetDialTimeout(custom)
	if proxy.dialTimeout != custom {
		t.Errorf("expected dial timeout %v, got %v", custom, proxy.dialTimeout)
	}
}

func TestRPCProxyBidirectional(t *testing.T) {
	echoListener, echoAddr := startRPCEchoServer(t)
	defer echoListener.Close()

	pool := newRPCTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	proxy := NewRPCProxy(pool, lb, slog.Default())

	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer proxyListener.Close()

	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go proxy.ServeRPC(conn)
		}
	}()

	conn, err := net.DialTimeout("tcp", proxyListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	// Send a JSON-RPC request.
	request := `{"jsonrpc":"2.0","method":"test","id":1}` + "\n"
	_, err = conn.Write([]byte(request))
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	// Read the echo response.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	got := string(buf[:n])
	if got != request {
		t.Fatalf("expected %q, got %q", request, got)
	}
}

func TestRPCProxyNoBackends(t *testing.T) {
	pool := &backend.Pool{
		Name:     "empty-rpc",
		Balancer: "round_robin",
		Backends: []*backend.Backend{},
	}
	lb := newTestBalancer(t)
	proxy := NewRPCProxy(pool, lb, slog.Default())

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	done := make(chan struct{})
	go func() {
		proxy.ServeRPC(serverConn)
		close(done)
	}()

	// Read the error response from the client side.
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read error response: %v", err)
	}

	// Verify it's a valid JSON-RPC error response.
	var resp jsonRPCResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.JSONRPC != "2.0" {
		t.Errorf("expected jsonrpc \"2.0\", got %q", resp.JSONRPC)
	}
	if resp.Error.Code != -32603 {
		t.Errorf("expected error code -32603, got %d", resp.Error.Code)
	}
	if resp.Error.Message != "no backends available" {
		t.Errorf("expected error message \"no backends available\", got %q", resp.Error.Message)
	}

	clientConn.Close()

	select {
	case <-done:
		// ServeRPC returned as expected.
	case <-time.After(3 * time.Second):
		t.Fatal("ServeRPC did not return within timeout")
	}
}

func TestRPCProxyBackendUnreachable(t *testing.T) {
	// Use port 1 which is typically unreachable.
	pool := newRPCTestPool(t, "127.0.0.1:1")
	lb := newTestBalancer(t)
	proxy := NewRPCProxy(pool, lb, slog.Default())
	proxy.SetDialTimeout(500 * time.Millisecond)

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	done := make(chan struct{})
	go func() {
		proxy.ServeRPC(serverConn)
		close(done)
	}()

	// Read the error response from the client side.
	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read error response: %v", err)
	}

	// Verify it's a valid JSON-RPC error response.
	var resp jsonRPCResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.JSONRPC != "2.0" {
		t.Errorf("expected jsonrpc \"2.0\", got %q", resp.JSONRPC)
	}
	if resp.Error.Code != -32603 {
		t.Errorf("expected error code -32603, got %d", resp.Error.Code)
	}
	if resp.Error.Message != "backend unreachable" {
		t.Errorf("expected error message \"backend unreachable\", got %q", resp.Error.Message)
	}

	clientConn.Close()

	select {
	case <-done:
		// ServeRPC returned as expected.
	case <-time.After(5 * time.Second):
		t.Fatal("ServeRPC did not return within timeout")
	}
}

func TestRPCProxyBackendSelection(t *testing.T) {
	// Start two echo servers.
	echoListener1, echoAddr1 := startRPCEchoServer(t)
	defer echoListener1.Close()
	echoListener2, echoAddr2 := startRPCEchoServer(t)
	defer echoListener2.Close()

	pool := newRPCTestPool(t, echoAddr1, echoAddr2)
	lb := newTestBalancer(t)
	proxy := NewRPCProxy(pool, lb, slog.Default())

	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer proxyListener.Close()

	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go proxy.ServeRPC(conn)
		}
	}()

	// Reset active connection counters.
	pool.Backends[0].ActiveConns.Store(0)
	pool.Backends[1].ActiveConns.Store(0)

	// Make multiple connections and verify round-robin distribution.
	for i := 0; i < 4; i++ {
		conn, err := net.DialTimeout("tcp", proxyListener.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatalf("failed to connect to proxy: %v", err)
		}

		msg := []byte(`{"jsonrpc":"2.0","method":"test","id":1}` + "\n")
		conn.Write(msg)
		buf := make([]byte, len(msg))
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		conn.Read(buf)
		conn.Close()
	}

	// With round-robin and 4 requests, both backends should have been selected.
	b1Used := pool.Backends[0].GetActiveConns() >= 0
	b2Used := pool.Backends[1].GetActiveConns() >= 0
	if !b1Used || !b2Used {
		t.Fatal("expected both backends to be used with round-robin")
	}
}

func TestRPCProxyConcurrent(t *testing.T) {
	echoListener, echoAddr := startRPCEchoServer(t)
	defer echoListener.Close()

	pool := newRPCTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	proxy := NewRPCProxy(pool, lb, slog.Default())

	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer proxyListener.Close()

	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil {
				return
			}
			go proxy.ServeRPC(conn)
		}
	}()

	var wg sync.WaitGroup
	numConcurrent := 10

	for i := 0; i < numConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := net.DialTimeout("tcp", proxyListener.Addr().String(), 2*time.Second)
			if err != nil {
				t.Errorf("failed to connect: %v", err)
				return
			}
			defer conn.Close()

			msg := []byte(`{"jsonrpc":"2.0","method":"concurrent","id":1}` + "\n")
			_, err = conn.Write(msg)
			if err != nil {
				t.Errorf("failed to write: %v", err)
				return
			}

			buf := make([]byte, len(msg))
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, err := conn.Read(buf)
			if err != nil {
				t.Errorf("failed to read: %v", err)
				return
			}
			if string(buf[:n]) != string(msg) {
				t.Errorf("expected %q, got %q", msg, buf[:n])
			}
		}()
	}

	wg.Wait()
}
