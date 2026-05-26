package proxy

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
)

// startUDPEchoServer starts a UDP echo server on a random port.
// It reads packets and echoes them back to the sender.
func startUDPEchoServer(t *testing.T) (net.PacketConn, string) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			conn.WriteTo(buf[:n], addr) // Echo back
		}
	}()
	return conn, conn.LocalAddr().String()
}

// newUDPTestPool creates a backend pool with the given addresses as healthy backends.
func newUDPTestPool(t *testing.T, addrs ...string) *backend.Pool {
	t.Helper()
	backends := make([]*backend.Backend, 0, len(addrs))
	for _, addr := range addrs {
		u, err := url.Parse("udp://" + addr)
		if err != nil {
			t.Fatalf("failed to parse URL %q: %v", addr, err)
		}
		backends = append(backends, &backend.Backend{
			URL:    u,
			RawURL: "udp://" + addr,
			Weight: 1,
		})
	}
	return &backend.Pool{
		Name:     "test-udp",
		Balancer: "round_robin",
		Backends: backends,
	}
}

func TestNewUDPProxy(t *testing.T) {
	pool := newUDPTestPool(t, "127.0.0.1:1")
	lb := newTestBalancer(t)
	p := NewUDPProxy(pool, lb, slog.Default())

	if p.pool != pool {
		t.Error("expected pool to be set")
	}
	if p.balancer != lb {
		t.Error("expected balancer to be set")
	}
	if p.dialTimeout != defaultUDPDialTimeout {
		t.Errorf("expected dialTimeout %v, got %v", defaultUDPDialTimeout, p.dialTimeout)
	}
	if p.sessionTimeout != defaultUDPSessionTimeout {
		t.Errorf("expected sessionTimeout %v, got %v", defaultUDPSessionTimeout, p.sessionTimeout)
	}
	if p.sessions == nil {
		t.Error("expected sessions map to be initialized")
	}
	if p.SessionCount() != 0 {
		t.Errorf("expected 0 sessions, got %d", p.SessionCount())
	}
}

func TestUDPProxySetTimeouts(t *testing.T) {
	pool := newUDPTestPool(t, "127.0.0.1:1")
	lb := newTestBalancer(t)
	p := NewUDPProxy(pool, lb, slog.Default())

	p.SetDialTimeout(5 * time.Second)
	if p.dialTimeout != 5*time.Second {
		t.Errorf("expected dialTimeout 5s, got %v", p.dialTimeout)
	}

	p.SetSessionTimeout(15 * time.Second)
	if p.sessionTimeout != 15*time.Second {
		t.Errorf("expected sessionTimeout 15s, got %v", p.sessionTimeout)
	}
}

func TestUDPProxySessionCleanup(t *testing.T) {
	echoConn, echoAddr := startUDPEchoServer(t)
	defer echoConn.Close()

	pool := newUDPTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	p := NewUDPProxy(pool, lb, slog.Default())
	p.SetSessionTimeout(2 * time.Second)

	proxyConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.ServePacketConn(ctx, proxyConn)
	}()

	// Send a packet to create a session
	clientConn, err := net.Dial("udp", proxyConn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	clientConn.Write([]byte("hello"))
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 65535)
	_, err = clientConn.Read(buf)
	if err != nil {
		t.Fatalf("expected echo response, got error: %v", err)
	}

	// Verify session was created
	if p.SessionCount() < 1 {
		t.Fatal("expected at least 1 session after sending packet")
	}

	// Wait for session to expire (longer than sessionTimeout)
	time.Sleep(3 * time.Second)

	// Session should be cleaned up
	if p.SessionCount() != 0 {
		t.Errorf("expected 0 sessions after timeout, got %d", p.SessionCount())
	}
}

func TestUDPProxyBidirectional(t *testing.T) {
	echoConn, echoAddr := startUDPEchoServer(t)
	defer echoConn.Close()

	pool := newUDPTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	p := NewUDPProxy(pool, lb, slog.Default())
	p.SetSessionTimeout(5 * time.Second)

	proxyConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.ServePacketConn(ctx, proxyConn)
	}()

	// Send a packet through the proxy
	clientConn, err := net.Dial("udp", proxyConn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	testMsg := []byte("hello udp proxy")
	_, err = clientConn.Write(testMsg)
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	// Read the echo response
	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 65535)
	n, err := clientConn.Read(buf)
	if err != nil {
		t.Fatalf("expected echo response, got error: %v", err)
	}
	if string(buf[:n]) != string(testMsg) {
		t.Errorf("expected %q, got %q", testMsg, buf[:n])
	}

	// Verify session was created
	if p.SessionCount() < 1 {
		t.Error("expected at least 1 session")
	}

	// Send a second packet through the same client connection
	testMsg2 := []byte("second packet")
	_, err = clientConn.Write(testMsg2)
	if err != nil {
		t.Fatalf("failed to write second packet: %v", err)
	}

	clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = clientConn.Read(buf)
	if err != nil {
		t.Fatalf("expected echo response for second packet, got error: %v", err)
	}
	if string(buf[:n]) != string(testMsg2) {
		t.Errorf("expected %q, got %q", testMsg2, buf[:n])
	}
}

func TestUDPProxyNoBackends(t *testing.T) {
	pool := &backend.Pool{
		Name:     "empty",
		Balancer: "round_robin",
		Backends: []*backend.Backend{},
	}
	lb := newTestBalancer(t)
	p := NewUDPProxy(pool, lb, slog.Default())

	proxyConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.ServePacketConn(ctx, proxyConn)
	}()

	// Send a packet — should be silently dropped (no crash)
	clientConn, err := net.Dial("udp", proxyConn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	clientConn.Write([]byte("test no backends"))

	// No session should be created
	time.Sleep(200 * time.Millisecond)
	if p.SessionCount() != 0 {
		t.Errorf("expected 0 sessions with no backends, got %d", p.SessionCount())
	}

	// Proxy should still be running (not crashed)
	cancel()
	select {
	case <-done:
		// ServePacketConn returned as expected
	case <-time.After(3 * time.Second):
		t.Fatal("ServePacketConn did not return after context cancellation")
	}
}

func TestUDPProxyBackendSelection(t *testing.T) {
	// Start two echo servers
	echoConn1, echoAddr1 := startUDPEchoServer(t)
	defer echoConn1.Close()
	echoConn2, echoAddr2 := startUDPEchoServer(t)
	defer echoConn2.Close()

	pool := newUDPTestPool(t, echoAddr1, echoAddr2)
	lb := newTestBalancer(t)
	p := NewUDPProxy(pool, lb, slog.Default())
	p.SetSessionTimeout(5 * time.Second)

	proxyConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.ServePacketConn(ctx, proxyConn)
	}()

	// Reset active conns
	pool.Backends[0].ActiveConns.Store(0)
	pool.Backends[1].ActiveConns.Store(0)

	// Send packets from different client addresses to trigger round-robin
	for i := 0; i < 4; i++ {
		clientConn, err := net.Dial("udp", proxyConn.LocalAddr().String())
		if err != nil {
			t.Fatalf("failed to dial proxy: %v", err)
		}

		msg := []byte("test")
		_, err = clientConn.Write(msg)
		if err != nil {
			t.Fatalf("failed to write: %v", err)
		}

		clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 65535)
		_, err = clientConn.Read(buf)
		if err != nil {
			t.Fatalf("failed to read echo: %v", err)
		}
		clientConn.Close()
	}

	// With round-robin and 4 requests from different clients,
	// both backends should have been selected at least once
	b1Conns := pool.Backends[0].GetActiveConns()
	b2Conns := pool.Backends[1].GetActiveConns()

	if b1Conns == 0 && b2Conns == 0 {
		t.Fatal("expected at least one backend to be used")
	}

	// At least one backend should have been selected (both ideally with round-robin)
	t.Logf("backend1 conns: %d, backend2 conns: %d", b1Conns, b2Conns)
}

func TestUDPProxyConcurrent(t *testing.T) {
	echoConn, echoAddr := startUDPEchoServer(t)
	defer echoConn.Close()

	pool := newUDPTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	p := NewUDPProxy(pool, lb, slog.Default())
	p.SetSessionTimeout(10 * time.Second)

	proxyConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.ServePacketConn(ctx, proxyConn)

	var wg sync.WaitGroup
	numConcurrent := 10

	for i := 0; i < numConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := net.Dial("udp", proxyConn.LocalAddr().String())
			if err != nil {
				t.Errorf("failed to dial: %v", err)
				return
			}
			defer conn.Close()

			msg := []byte("concurrent test")
			_, err = conn.Write(msg)
			if err != nil {
				t.Errorf("failed to write: %v", err)
				return
			}

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 65535)
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

	if p.SessionCount() < 1 {
		t.Errorf("expected at least 1 session after concurrent test, got %d", p.SessionCount())
	}
}

func TestUDPProxyContextCancellation(t *testing.T) {
	echoConn, echoAddr := startUDPEchoServer(t)
	defer echoConn.Close()

	pool := newUDPTestPool(t, echoAddr)
	lb := newTestBalancer(t)
	p := NewUDPProxy(pool, lb, slog.Default())

	proxyConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyConn.Close()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.ServePacketConn(ctx, proxyConn)
	}()

	// Send a packet to create a session
	clientConn, err := net.Dial("udp", proxyConn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	clientConn.Write([]byte("before cancel"))
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 65535)
	_, err = clientConn.Read(buf)
	if err != nil {
		t.Fatalf("expected echo, got: %v", err)
	}
	clientConn.Close()

	if p.SessionCount() < 1 {
		t.Fatal("expected at least 1 session")
	}

	// Cancel context
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServePacketConn did not return after context cancellation")
	}

	// All sessions should be cleaned up
	if p.SessionCount() != 0 {
		t.Errorf("expected 0 sessions after context cancellation, got %d", p.SessionCount())
	}
}
