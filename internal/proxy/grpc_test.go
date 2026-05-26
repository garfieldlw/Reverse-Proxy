package proxy

import (
	"errors"
	"log/slog"
	"net"
	"net/url"
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/interop"
	"google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"
)

// mockGRPCBalancer implements balancer.Balancer for testing.
type mockGRPCBalancer struct {
	backend *backend.Backend
	err     error
	called  bool
}

func (m *mockGRPCBalancer) Select(backends []*backend.Backend) (*backend.Backend, error) {
	m.called = true
	if m.err != nil {
		return nil, m.err
	}
	if len(backends) == 0 {
		return nil, balancer.ErrNoBackends
	}
	if m.backend != nil {
		return m.backend, nil
	}
	return backends[0], nil
}

// newGRPCTestPool creates a backend pool with the given URLs as healthy backends.
func newGRPCTestPool(t *testing.T, rawURLs ...string) *backend.Pool {
	t.Helper()
	backends := make([]*backend.Backend, 0, len(rawURLs))
	for _, raw := range rawURLs {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse URL %q: %v", raw, err)
		}
		backends = append(backends, &backend.Backend{
			URL:    u,
			RawURL: raw,
			Weight: 1,
		})
	}
	return &backend.Pool{
		Name:     "test-grpc",
		Balancer: "round_robin",
		Backends: backends,
	}
}

func TestGRPCProxyNoBackends(t *testing.T) {
	pool := &backend.Pool{
		Name:     "test-empty",
		Balancer: "round_robin",
		Backends: []*backend.Backend{},
	}
	b := &mockGRPCBalancer{err: balancer.ErrNoBackends}
	proxy := NewGRPCProxy(pool, b, slog.Default())
	srv := proxy.Server()
	defer srv.Stop()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(lis)

	cc, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)
	_, err = client.EmptyCall(t.Context(), &grpc_testing.Empty{})
	if err == nil {
		t.Fatal("expected error when no backends available, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Errorf("expected Unavailable, got %v: %v", st.Code(), st.Message())
	}
	if !b.called {
		t.Error("expected balancer.Select to be called")
	}
}

func TestGRPCProxyBackendSelection(t *testing.T) {
	pool := newGRPCTestPool(t, "grpc://127.0.0.1:1")
	b := &mockGRPCBalancer{}
	proxy := NewGRPCProxy(pool, b, slog.Default())

	srv := proxy.Server()
	defer srv.Stop()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(lis)

	cc, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)
	_, err = client.EmptyCall(t.Context(), &grpc_testing.Empty{})
	// The call will fail because the backend is unreachable, but the
	// balancer should have been invoked.
	if !b.called {
		t.Error("expected balancer.Select to be called")
	}
}

func TestGRPCProxyServer(t *testing.T) {
	pool := newGRPCTestPool(t, "grpc://127.0.0.1:1")
	b := &mockGRPCBalancer{}
	proxy := NewGRPCProxy(pool, b, slog.Default())

	srv := proxy.Server()
	if srv == nil {
		t.Fatal("Server() returned nil")
	}
	defer srv.Stop()
}

func TestGRPCProxyWithRealBackend(t *testing.T) {
	// Start a real gRPC interop test server as the backend.
	backendSrv := grpc.NewServer()
	grpc_testing.RegisterTestServiceServer(backendSrv, interop.NewTestServer())
	backendLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for backend: %v", err)
	}
	go backendSrv.Serve(backendLis)
	defer backendSrv.Stop()

	// Create a pool pointing at the backend.
	pool := newGRPCTestPool(t, "grpc://"+backendLis.Addr().String())
	b := &mockGRPCBalancer{}
	proxy := NewGRPCProxy(pool, b, slog.Default())

	// Start the proxy server.
	proxySrv := proxy.Server()
	defer proxySrv.Stop()
	proxyLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for proxy: %v", err)
	}
	go proxySrv.Serve(proxyLis)

	// Dial the proxy and make a real call.
	cc, err := grpc.NewClient(proxyLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)
	resp, err := client.EmptyCall(t.Context(), &grpc_testing.Empty{})
	if err != nil {
		t.Fatalf("EmptyCall through proxy: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestGRPCProxyBalancerError(t *testing.T) {
	pool := newGRPCTestPool(t, "grpc://127.0.0.1:1")
	customErr := errors.New("custom balancer error")
	b := &mockGRPCBalancer{err: customErr}
	proxy := NewGRPCProxy(pool, b, slog.Default())

	srv := proxy.Server()
	defer srv.Stop()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(lis)

	cc, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)
	_, err = client.EmptyCall(t.Context(), &grpc_testing.Empty{})
	if err == nil {
		t.Fatal("expected error from balancer failure, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Errorf("expected Unavailable, got %v: %v", st.Code(), st.Message())
	}
}

func TestGRPCProxyConnTracking(t *testing.T) {
	// Start a real backend.
	backendSrv := grpc.NewServer()
	grpc_testing.RegisterTestServiceServer(backendSrv, interop.NewTestServer())
	backendLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for backend: %v", err)
	}
	go backendSrv.Serve(backendLis)
	defer backendSrv.Stop()

	pool := newGRPCTestPool(t, "grpc://"+backendLis.Addr().String())
	b := &mockGRPCBalancer{}
	proxy := NewGRPCProxy(pool, b, slog.Default())

	proxySrv := proxy.Server()
	defer proxySrv.Stop()
	proxyLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for proxy: %v", err)
	}
	go proxySrv.Serve(proxyLis)

	cc, err := grpc.NewClient(proxyLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)
	_, err = client.EmptyCall(t.Context(), &grpc_testing.Empty{})
	if err != nil {
		t.Fatalf("EmptyCall through proxy: %v", err)
	}

	// After the call completes, the active connections should be back to 0.
	healthy := pool.GetHealthyBackends()
	if len(healthy) == 0 {
		t.Fatal("expected at least one healthy backend")
	}
	conns := healthy[0].GetActiveConns()
	if conns != 0 {
		t.Errorf("expected 0 active connections after call, got %d", conns)
	}
}
