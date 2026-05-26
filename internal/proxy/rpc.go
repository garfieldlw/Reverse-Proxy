package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
)

// defaultRPCDialTimeout is the default timeout for dialing backend RPC connections.
const defaultRPCDialTimeout = 10 * time.Second

// jsonRPCError represents a JSON-RPC 2.0 error object.
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// jsonRPCResponse represents a JSON-RPC 2.0 error response.
type jsonRPCResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	Error   jsonRPCError `json:"error"`
	ID      any          `json:"id,omitempty"`
}

// RPCProxy is a JSON-RPC over TCP reverse proxy.
// It forwards JSON-RPC requests to backend servers with load balancing.
// When no backends are available or the backend is unreachable, it returns
// proper JSON-RPC 2.0 error responses to the client.
type RPCProxy struct {
	pool        *backend.Pool
	balancer    balancer.Balancer
	logger      *slog.Logger
	dialTimeout time.Duration
}

// NewRPCProxy creates a new JSON-RPC reverse proxy.
func NewRPCProxy(pool *backend.Pool, balancer balancer.Balancer, logger *slog.Logger) *RPCProxy {
	return &RPCProxy{
		pool:        pool,
		balancer:    balancer,
		logger:      logger,
		dialTimeout: defaultRPCDialTimeout,
	}
}

// SetDialTimeout sets the timeout for dialing backend connections.
func (p *RPCProxy) SetDialTimeout(d time.Duration) {
	p.dialTimeout = d
}

// ServeRPC handles a single client RPC connection by proxying it to a backend.
func (p *RPCProxy) ServeRPC(clientConn net.Conn) {
	defer clientConn.Close()

	// Select a healthy backend via the balancer.
	healthy := p.pool.GetHealthyBackends()
	b, err := p.balancer.Select(healthy)
	if err != nil {
		p.logger.Error("no backends available for rpc", "remote_addr", clientConn.RemoteAddr(), "error", err)
		p.sendRPCError(clientConn, nil, -32603, "no backends available")
		return
	}

	b.IncConns()
	defer b.DecConns()

	// Dial the backend.
	backendConn, err := net.DialTimeout("tcp", b.URL.Host, p.dialTimeout)
	if err != nil {
		p.logger.Error("rpc dial backend failed", "backend", b.RawURL, "error", err, "remote_addr", clientConn.RemoteAddr())
		p.sendRPCError(clientConn, nil, -32603, "backend unreachable")
		return
	}
	defer backendConn.Close()

	p.logger.Debug("rpc proxy connected", "backend", b.RawURL, "remote_addr", clientConn.RemoteAddr())

	// Bidirectional copy.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(backendConn, clientConn)
		if tc, ok := backendConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, backendConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()

	p.logger.Info("rpc proxy completed", "backend", b.RawURL, "remote_addr", clientConn.RemoteAddr())
}

// sendRPCError writes a JSON-RPC 2.0 error response to the connection.
func (p *RPCProxy) sendRPCError(conn net.Conn, id any, code int, message string) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		Error: jsonRPCError{
			Code:    code,
			Message: message,
		},
		ID: id,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		p.logger.Error("failed to marshal rpc error response", "error", err)
		return
	}

	// Append newline for newline-delimited JSON.
	data = append(data, '\n')

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(data); err != nil {
		p.logger.Error("failed to write rpc error response", "error", err)
	}
}

// ListenAndServe starts a TCP listener on the given address and accepts
// connections in a loop, proxying each one via ServeRPC.
// It returns when the listener is closed.
func (p *RPCProxy) ListenAndServe(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go p.ServeRPC(conn)
	}
}
