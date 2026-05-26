package proxy

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
)

// defaultDialTimeout is the default timeout for dialing backend connections.
const defaultDialTimeout = 10 * time.Second

// TCPProxy is a TCP stream proxy that load balances across backends.
type TCPProxy struct {
	pool          *backend.Pool
	balancer      balancer.Balancer
	logger        *slog.Logger
	dialTimeout   time.Duration
	listenNetwork string // "tcp" or "unix"
	dialNetwork   string // "tcp" or "unix"
}

// NewTCPProxy creates a new TCP reverse proxy.
func NewTCPProxy(pool *backend.Pool, balancer balancer.Balancer, logger *slog.Logger) *TCPProxy {
	return &TCPProxy{
		pool:          pool,
		balancer:      balancer,
		logger:        logger,
		dialTimeout:   defaultDialTimeout,
		listenNetwork: "tcp",
		dialNetwork:   "tcp",
	}
}

// NewSocketProxy creates a new Unix domain socket reverse proxy.
// It reuses TCPProxy's bidirectional copy logic but uses "unix" network for dialing backends.
func NewSocketProxy(pool *backend.Pool, balancer balancer.Balancer, logger *slog.Logger) *TCPProxy {
	return &TCPProxy{
		pool:          pool,
		balancer:      balancer,
		logger:        logger,
		dialTimeout:   defaultDialTimeout,
		listenNetwork: "unix",
		dialNetwork:   "unix",
	}
}

// SetListenNetwork sets the network type for listening (e.g. "tcp" or "unix").
func (p *TCPProxy) SetListenNetwork(network string) {
	p.listenNetwork = network
}

// SetDialNetwork sets the network type for dialing backends (e.g. "tcp" or "unix").
func (p *TCPProxy) SetDialNetwork(network string) {
	p.dialNetwork = network
}

// GetListenNetwork returns the current listen network type.
func (p *TCPProxy) GetListenNetwork() string {
	return p.listenNetwork
}

// GetDialNetwork returns the current dial network type.
func (p *TCPProxy) GetDialNetwork() string {
	return p.dialNetwork
}

// backendAddr returns the address to dial for a backend based on the network type.
// For "unix" network, uses URL.Path; for "tcp", uses URL.Host.
func (p *TCPProxy) backendAddr(b *backend.Backend) string {
	if p.dialNetwork == "unix" {
		return b.URL.Path
	}
	return b.URL.Host
}

// SetDialTimeout sets the timeout for dialing backend connections.
func (p *TCPProxy) SetDialTimeout(d time.Duration) {
	p.dialTimeout = d
}

// ServeTCP handles a single client TCP connection by proxying it to a backend.
func (p *TCPProxy) ServeTCP(clientConn net.Conn) {
	defer clientConn.Close()

	// Select a healthy backend via the balancer.
	healthy := p.pool.GetHealthyBackends()
	b, err := p.balancer.Select(healthy)
	if err != nil {
		p.logger.Error("no backends available", "error", err, "remote_addr", clientConn.RemoteAddr())
		return
	}

	b.IncConns()
	defer b.DecConns()

	// Dial the backend.
	backendConn, err := net.DialTimeout(p.dialNetwork, p.backendAddr(b), p.dialTimeout)
	if err != nil {
		p.logger.Error("dial backend failed", "network", p.dialNetwork, "backend", b.RawURL, "error", err, "remote_addr", clientConn.RemoteAddr())
		return
	}
	defer backendConn.Close()

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

	p.logger.Info("tcp proxy completed", "network", p.dialNetwork, "backend", b.RawURL, "remote_addr", clientConn.RemoteAddr())
}

// ListenAndServe starts a TCP listener on the given address and accepts
// connections in a loop, proxying each one via ServeTCP.
// It returns when the listener is closed.
func (p *TCPProxy) ListenAndServe(addr string) error {
	listener, err := net.Listen(p.listenNetwork, addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go p.ServeTCP(conn)
	}
}
