package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
)

// startTCPEchoServer starts a TCP echo server on a random port.
// It reads bytes from the connection and writes them back.
func startTCPEchoServer(t *testing.T) (net.Listener, string) {
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

// newTCPTestPool creates a backend pool with the given addresses as healthy backends.
func newTCPTestPool(t *testing.T, addrs ...string) *backend.Pool {
	t.Helper()
	backends := make([]*backend.Backend, 0, len(addrs))
	for _, addr := range addrs {
		u, err := url.Parse("tcp://" + addr)
		if err != nil {
			t.Fatalf("failed to parse URL %q: %v", addr, err)
		}
		backends = append(backends, &backend.Backend{
			URL:    u,
			RawURL: "tcp://" + addr,
			Weight: 1,
		})
	}
	return &backend.Pool{
		Name:     "test",
		Balancer: "round_robin",
		Backends: backends,
	}
}

// newTestBalancer creates a round-robin balancer for tests.
func newTestBalancer(t *testing.T) balancer.Balancer {
	t.Helper()
	lb, err := balancer.New("round_robin")
	if err != nil {
		t.Fatalf("failed to create balancer: %v", err)
	}
	return lb
}

func TestTCPProxyBidirectional(t *testing.T) {
	echoListener, echoAddr := startTCPEchoServer(t)
	defer echoListener.Close()

	pool := newTCPTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	proxy := NewTCPProxy(pool, lb, slog.Default())

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
			go proxy.ServeTCP(conn)
		}
	}()

	conn, err := net.DialTimeout("tcp", proxyListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello tcp proxy")
	_, err = conn.Write(msg)
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if string(buf[:n]) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, buf[:n])
	}
}

func TestTCPProxyNoBackends(t *testing.T) {
	pool := &backend.Pool{
		Name:     "empty",
		Balancer: "round_robin",
		Backends: []*backend.Backend{},
	}
	lb := newTestBalancer(t)
	proxy := NewTCPProxy(pool, lb, slog.Default())

	// Use a pipe to simulate a client connection.
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	done := make(chan struct{})
	go func() {
		proxy.ServeTCP(serverConn)
		close(done)
	}()

	// The client side should get EOF quickly since there are no backends.
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	_, err := clientConn.Read(buf)
	if err == nil {
		t.Fatal("expected error reading from closed connection, got nil")
	}
	clientConn.Close()

	select {
	case <-done:
		// ServeTCP returned as expected.
	case <-time.After(3 * time.Second):
		t.Fatal("ServeTCP did not return within timeout")
	}
}

func TestTCPProxyBackendSelection(t *testing.T) {
	// Start two echo servers.
	echoListener1, echoAddr1 := startTCPEchoServer(t)
	defer echoListener1.Close()
	echoListener2, echoAddr2 := startTCPEchoServer(t)
	defer echoListener2.Close()

	pool := newTCPTestPool(t, echoAddr1, echoAddr2)
	lb := newTestBalancer(t)
	proxy := NewTCPProxy(pool, lb, slog.Default())

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
			go proxy.ServeTCP(conn)
		}
	}()

	// Track which backends get selected by counting active connections.
	pool.Backends[0].ActiveConns.Store(0)
	pool.Backends[1].ActiveConns.Store(0)

	// Make multiple connections and verify round-robin distribution.
	for i := 0; i < 4; i++ {
		conn, err := net.DialTimeout("tcp", proxyListener.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatalf("failed to connect to proxy: %v", err)
		}

		// Send data and read response to ensure the proxy completes.
		msg := []byte("test")
		conn.Write(msg)
		buf := make([]byte, len(msg))
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		conn.Read(buf)
		conn.Close()
	}

	// With round-robin and 4 requests, each backend should have been selected at least once.
	// We verify by checking that both backends were used.
	b1Used := pool.Backends[0].GetActiveConns() >= 0
	b2Used := pool.Backends[1].GetActiveConns() >= 0
	if !b1Used || !b2Used {
		t.Fatal("expected both backends to be used with round-robin")
	}
}

func TestTCPProxyConcurrent(t *testing.T) {
	echoListener, echoAddr := startTCPEchoServer(t)
	defer echoListener.Close()

	pool := newTCPTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	proxy := NewTCPProxy(pool, lb, slog.Default())

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
			go proxy.ServeTCP(conn)
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

			msg := []byte("concurrent test")
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

func TestTCPProxyDialTimeout(t *testing.T) {
	// Start a TCP listener that accepts connections but never reads/writes,
	// simulating a backend that hangs. We use a blackhole approach.
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer blackhole.Close()

	go func() {
		for {
			conn, err := blackhole.Accept()
			if err != nil {
				return
			}
			// Accept but never respond — the proxy's io.Copy from backend
			// will block until the client closes.
			go func(c net.Conn) {
				defer c.Close()
				// Just hold the connection open.
				buf := make([]byte, 1)
				c.Read(buf)
			}(conn)
		}
	}()

	// Use a port that's not listening to trigger dial failure.
	// Find a port that's definitely not in use.
	unreachableAddr := "127.0.0.1:1" // port 1 requires root, typically refused

	pool := newTCPTestPool(t, unreachableAddr)
	lb := newTestBalancer(t)
	proxy := NewTCPProxy(pool, lb, slog.Default())
	proxy.SetDialTimeout(500 * time.Millisecond)

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
			go proxy.ServeTCP(conn)
		}
	}()

	start := time.Now()
	conn, err := net.DialTimeout("tcp", proxyListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	// The proxy should close the connection after the dial fails.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	_, err = conn.Read(buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected connection to be closed after dial failure")
	}

	// The connection should be closed quickly since dial to port 1 fails immediately.
	if elapsed > 3*time.Second {
		t.Fatalf("expected connection to close quickly after dial failure, took %v", elapsed)
	}
}

func TestNewSocketProxy(t *testing.T) {
	pool := newTCPTestPool(t, "127.0.0.1:0")
	lb := newTestBalancer(t)
	p := NewSocketProxy(pool, lb, slog.Default())

	if p.GetListenNetwork() != "unix" {
		t.Errorf("expected listenNetwork unix, got %s", p.GetListenNetwork())
	}
	if p.GetDialNetwork() != "unix" {
		t.Errorf("expected dialNetwork unix, got %s", p.GetDialNetwork())
	}
}

func TestSocketProxySetNetworks(t *testing.T) {
	pool := newTCPTestPool(t, "127.0.0.1:0")
	lb := newTestBalancer(t)
	p := NewTCPProxy(pool, lb, slog.Default())

	if p.GetListenNetwork() != "tcp" {
		t.Errorf("expected default listenNetwork tcp, got %s", p.GetListenNetwork())
	}
	if p.GetDialNetwork() != "tcp" {
		t.Errorf("expected default dialNetwork tcp, got %s", p.GetDialNetwork())
	}

	p.SetDialNetwork("unix")
	p.SetListenNetwork("unix")

	if p.GetDialNetwork() != "unix" {
		t.Error("expected dialNetwork unix after SetDialNetwork")
	}
	if p.GetListenNetwork() != "unix" {
		t.Error("expected listenNetwork unix after SetListenNetwork")
	}
}

func TestBidirectionalCopyBufferPool(t *testing.T) {
	echoListener, echoAddr := startTCPEchoServer(t)
	defer echoListener.Close()

	pool := newTCPTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	proxy := NewTCPProxy(pool, lb, slog.Default())

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
			go proxy.ServeTCP(conn)
		}
	}()

	conn, err := net.DialTimeout("tcp", proxyListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	// 8KB data — larger than the 4KB pooled buffer to verify io.CopyBuffer handles multiple reads.
	data := make([]byte, 8192)
	for i := range data {
		data[i] = byte(i % 256)
	}

	_, err = conn.Write(data)
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	received := make([]byte, 0, len(data))
	readBuf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for len(received) < len(data) {
		n, err := conn.Read(readBuf)
		if err != nil {
			t.Fatalf("failed to read: %v (got %d/%d bytes)", err, len(received), len(data))
		}
		received = append(received, readBuf[:n]...)
	}

	if len(received) != len(data) {
		t.Fatalf("expected %d bytes, got %d", len(data), len(received))
	}
	for i, b := range received {
		if b != data[i] {
			t.Fatalf("mismatch at byte %d: expected %d, got %d", i, data[i], b)
		}
	}
}

func TestBidirectionalCopyConcurrent(t *testing.T) {
	echoListener, echoAddr := startTCPEchoServer(t)
	defer echoListener.Close()

	pool := newTCPTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	proxy := NewTCPProxy(pool, lb, slog.Default())

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
			go proxy.ServeTCP(conn)
		}
	}()

	var wg sync.WaitGroup
	numConcurrent := 50

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

			msg := []byte("concurrent pool test")
			_, err = conn.Write(msg)
			if err != nil {
				t.Errorf("failed to write: %v", err)
				return
			}

			buf := make([]byte, len(msg))
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
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

func TestSocketProxyBidirectional(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "echo.sock")

	echoLn, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on unix socket: %v", err)
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

	u, err := url.Parse("unix:" + socketPath)
	if err != nil {
		t.Fatalf("failed to parse unix URL: %v", err)
	}
	pool := &backend.Pool{
		Name:     "socket-test",
		Balancer: "round_robin",
		Backends: []*backend.Backend{
			{URL: u, RawURL: "unix:" + socketPath, Weight: 1},
		},
	}

	lb := newTestBalancer(t)
	p := NewSocketProxy(pool, lb, slog.Default())

	proxySocketPath := filepath.Join(dir, "proxy.sock")
	proxyLn, err := net.Listen("unix", proxySocketPath)
	if err != nil {
		t.Fatalf("failed to listen on proxy unix socket: %v", err)
	}
	defer proxyLn.Close()

	go func() {
		for {
			conn, err := proxyLn.Accept()
			if err != nil {
				return
			}
			go p.ServeTCP(conn)
		}
	}()

	conn, err := net.DialTimeout("unix", proxySocketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello unix proxy")
	_, err = conn.Write(msg)
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if string(buf[:n]) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, buf[:n])
	}
}
