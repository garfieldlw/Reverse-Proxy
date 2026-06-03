package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const proxyCodecName = "proxy"

func init() {
	encoding.RegisterCodec(&proxyCodec{})
}

// proxyCodec handles both proto.Message (from real gRPC clients) and raw
// []byte (for transparent forwarding). This allows the proxy server to
// accept standard proto-encoded requests while forwarding raw bytes to backends.
type proxyCodec struct{}

func (c *proxyCodec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case []byte:
		return m, nil
	case proto.Message:
		return proto.Marshal(m)
	default:
		return nil, fmt.Errorf("proxyCodec: unsupported type %T", v)
	}
}

func (c *proxyCodec) Unmarshal(data []byte, v any) error {
	switch m := v.(type) {
	case *[]byte:
		*m = data
		return nil
	case proto.Message:
		return proto.Unmarshal(data, m)
	default:
		return fmt.Errorf("proxyCodec: unsupported type %T", v)
	}
}

func (c *proxyCodec) Name() string {
	return proxyCodecName
}

// GRPCProxy is a gRPC transparent reverse proxy.
// It forwards gRPC requests to backend servers without requiring
// proto service definitions, acting as a pure byte-level proxy.
type GRPCProxy struct {
	pool     *backend.Pool
	balancer balancer.Balancer
	logger   *slog.Logger
	clients  sync.Map // string -> *grpc.ClientConn
}

// NewGRPCProxy creates a new gRPC reverse proxy.
func NewGRPCProxy(pool *backend.Pool, balancer balancer.Balancer, logger *slog.Logger) *GRPCProxy {
	return &GRPCProxy{
		pool:     pool,
		balancer: balancer,
		logger:   logger,
	}
}

// Server returns a *grpc.Server configured with an unknown service handler
// that transparently proxies all gRPC methods to a backend.
func (p *GRPCProxy) Server() *grpc.Server {
	s := grpc.NewServer(
		grpc.ForceServerCodec(&proxyCodec{}),
		grpc.UnknownServiceHandler(p.streamHandler),
	)
	return s
}

// Close closes all cached gRPC client connections.
func (p *GRPCProxy) Close() {
	p.clients.Range(func(key, value any) bool {
		value.(*grpc.ClientConn).Close()
		return true
	})
}

// getClient returns a cached gRPC client connection for the given address,
// creating one if needed.
func (p *GRPCProxy) getClient(targetAddr string) (*grpc.ClientConn, error) {
	if cc, ok := p.clients.Load(targetAddr); ok {
		return cc.(*grpc.ClientConn), nil
	}

	cc, err := grpc.NewClient(targetAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}

	actual, loaded := p.clients.LoadOrStore(targetAddr, cc)
	if loaded {
		cc.Close() // another goroutine beat us, close ours
		return actual.(*grpc.ClientConn), nil
	}

	return cc, nil
}

// streamHandler implements grpc.StreamHandler for transparent proxying.
// It selects a healthy backend, dials it, and forwards messages bidirectionally.
func (p *GRPCProxy) streamHandler(srv any, stream grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Errorf(codes.Internal, "failed to get method from stream")
	}

	backends := p.pool.GetHealthyBackends()
	b, err := p.balancer.Select(backends)
	if err != nil {
		return status.Errorf(codes.Unavailable, "no backends available: %v", err)
	}
	b.IncConns()
	defer b.DecConns()

	p.logger.Debug("gRPC proxy: selected backend",
		"method", method,
		"backend", b.RawURL,
	)

	targetAddr := b.URL.Host
	cc, err := p.getClient(targetAddr)
	if err != nil {
		return status.Errorf(codes.Unavailable, "dial backend %s failed: %v", b.RawURL, err)
	}

	md, ok := metadata.FromIncomingContext(stream.Context())
	outCtx := stream.Context()
	if ok {
		outCtx = metadata.NewOutgoingContext(outCtx, md.Copy())
	}

	backendStream, err := cc.NewStream(outCtx, &grpc.StreamDesc{
		ServerStreams: true,
		ClientStreams: true,
	}, method, grpc.ForceCodec(&proxyCodec{}))
	if err != nil {
		return status.Errorf(codes.Internal, "create backend stream failed: %v", err)
	}

	// Two separate error channels so we can handle each direction independently.
	clientToBackendErr := make(chan error, 1)
	backendToClientErr := make(chan error, 1)

	// client → backend
	go func() {
		buf := []byte{}
		for {
			if err := stream.RecvMsg(&buf); err != nil {
				clientToBackendErr <- err
				return
			}
			if err := backendStream.SendMsg(buf); err != nil {
				clientToBackendErr <- err
				return
			}
		}
	}()

	// backend → client
	go func() {
		buf := []byte{}
		for {
			if err := backendStream.RecvMsg(&buf); err != nil {
				backendToClientErr <- err
				return
			}
			if err := stream.SendMsg(buf); err != nil {
				backendToClientErr <- err
				return
			}
		}
	}()

	// Wait for both directions to complete.
	// For unary RPCs: backend→client finishes first (EOF), then client→backend
	// finishes (EOF after close-send). For streaming RPCs, either side may finish first.
	var clientToBackend, backendToClient error
	for i := 0; i < 2; i++ {
		select {
		case err := <-clientToBackendErr:
			clientToBackend = err
			// When client is done sending, close the send side of the backend stream.
			if err == io.EOF {
				backendStream.CloseSend()
			}
		case err := <-backendToClientErr:
			backendToClient = err
		}
	}

	if trailer := backendStream.Trailer(); len(trailer) > 0 {
		stream.SetTrailer(trailer)
	}

	// If the backend finished with EOF (success), that's the authoritative result.
	if backendToClient == io.EOF {
		return nil
	}
	if backendToClient != nil {
		return backendToClient
	}

	// If only the client side errored (e.g. context cancelled), propagate that.
	if clientToBackend != nil && clientToBackend != io.EOF {
		return clientToBackend
	}

	return nil
}
