package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/url"
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/interop"
	"google.golang.org/grpc/interop/grpc_testing"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

func startGRPCBenchBackend() (string, func()) {
	srv := grpc.NewServer()
	grpc_testing.RegisterTestServiceServer(srv, interop.NewTestServer())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	go srv.Serve(lis)
	return lis.Addr().String(), func() { srv.Stop() }
}

func startGRPCBenchProxy(backendAddrs []string) (*grpc.ClientConn, func()) {
	backends := make([]*backend.Backend, len(backendAddrs))
	for i, addr := range backendAddrs {
		raw := "grpc://" + addr
		u, _ := url.Parse(raw)
		backends[i] = &backend.Backend{URL: u, RawURL: raw}
	}
	pool := &backend.Pool{Name: "bench-grpc", Backends: backends}
	rr := &balancer.RoundRobin{}
	proxy := NewGRPCProxy(pool, rr, slog.Default())
	proxySrv := proxy.Server()
	proxyLis, _ := net.Listen("tcp", "127.0.0.1:0")
	go proxySrv.Serve(proxyLis)
	cc, _ := grpc.NewClient(
		proxyLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	return cc, func() {
		cc.Close()
		proxySrv.Stop()
	}
}

func BenchmarkGRPCProxyUnaryCall(b *testing.B) {
	b.ReportAllocs()

	addr, cleanup := startGRPCBenchBackend()
	defer cleanup()

	cc, proxyCleanup := startGRPCBenchProxy([]string{addr})
	defer proxyCleanup()

	client := grpc_testing.NewTestServiceClient(cc)

	b.ResetTimer()
	for b.Loop() {
		_, err := client.EmptyCall(context.Background(), &grpc_testing.Empty{})
		if err != nil {
			b.Fatalf("EmptyCall: %v", err)
		}
	}
}

func BenchmarkGRPCProxyUnaryPayloadSizes(b *testing.B) {
	sizes := []struct {
		name string
		n    int32
	}{
		{"1B", 1},
		{"1KB", 1024},
		{"10KB", 10240},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()

			addr, cleanup := startGRPCBenchBackend()
			defer cleanup()

			cc, proxyCleanup := startGRPCBenchProxy([]string{addr})
			defer proxyCleanup()

			client := grpc_testing.NewTestServiceClient(cc)
			req := &grpc_testing.SimpleRequest{ResponseSize: sz.n}

			b.ResetTimer()
			for b.Loop() {
				_, err := client.UnaryCall(context.Background(), req)
				if err != nil {
					b.Fatalf("UnaryCall: %v", err)
				}
			}
		})
	}
}

func BenchmarkGRPCProxyServerStreaming(b *testing.B) {
	b.ReportAllocs()

	addr, cleanup := startGRPCBenchBackend()
	defer cleanup()

	cc, proxyCleanup := startGRPCBenchProxy([]string{addr})
	defer proxyCleanup()

	client := grpc_testing.NewTestServiceClient(cc)

	respParams := make([]*grpc_testing.ResponseParameters, 10)
	for i := range respParams {
		respParams[i] = &grpc_testing.ResponseParameters{Size: 1}
	}
	req := &grpc_testing.StreamingOutputCallRequest{
		ResponseParameters: respParams,
	}

	b.ResetTimer()
	for b.Loop() {
		stream, err := client.StreamingOutputCall(context.Background(), req)
		if err != nil {
			b.Fatalf("StreamingOutputCall: %v", err)
		}
		for {
			_, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatalf("stream.Recv: %v", err)
			}
		}
	}
}

func BenchmarkGRPCProxyMultiBackend(b *testing.B) {
	b.ReportAllocs()

	addrs := make([]string, 3)
	cleanups := make([]func(), 3)
	for i := range 3 {
		addrs[i], cleanups[i] = startGRPCBenchBackend()
	}
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	cc, proxyCleanup := startGRPCBenchProxy(addrs)
	defer proxyCleanup()

	client := grpc_testing.NewTestServiceClient(cc)

	b.ResetTimer()
	for b.Loop() {
		_, err := client.EmptyCall(context.Background(), &grpc_testing.Empty{})
		if err != nil {
			b.Fatalf("EmptyCall: %v", err)
		}
	}
}

func BenchmarkGRPCProxyConnTracking(b *testing.B) {
	b.ReportAllocs()

	addr, cleanup := startGRPCBenchBackend()
	defer cleanup()

	raw := "grpc://" + addr
	u, _ := url.Parse(raw)
	pool := &backend.Pool{
		Name:     "bench-conn-track",
		Backends: []*backend.Backend{{URL: u, RawURL: raw}},
	}
	rr := &balancer.RoundRobin{}
	proxy := NewGRPCProxy(pool, rr, slog.Default())
	proxySrv := proxy.Server()
	defer proxySrv.Stop()
	proxyLis, _ := net.Listen("tcp", "127.0.0.1:0")
	go proxySrv.Serve(proxyLis)

	cc, _ := grpc.NewClient(
		proxyLis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	defer cc.Close()

	client := grpc_testing.NewTestServiceClient(cc)

	b.ResetTimer()
	for b.Loop() {
		_, err := client.EmptyCall(context.Background(), &grpc_testing.Empty{})
		if err != nil {
			b.Fatalf("EmptyCall: %v", err)
		}
		// Verify conn tracking after each call
		healthy := pool.GetHealthyBackends()
		if len(healthy) > 0 && healthy[0].GetActiveConns() != 0 {
			b.Fatalf("expected 0 active conns, got %d", healthy[0].GetActiveConns())
		}
	}
}
