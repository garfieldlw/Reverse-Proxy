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

const (
	defaultDialTimeout = 10 * time.Second
	maxIdlePerBackend  = 2                     // Max idle connections per backend address
	maxIdleAge         = 30 * time.Second      // Max age of idle connections in pool
	keepAlivePeriod    = 15 * time.Second      // TCP keepalive interval for pooled connections
)

// poolEntry wraps a backend connection with metadata for pool management.
type poolEntry struct {
	conn    net.Conn
	putTime time.Time
}

// backendConnPool manages idle backend TCP connections per backend address.
type backendConnPool struct {
	mu      sync.Mutex
	entries map[string][]*poolEntry // backendAddr -> idle connections
}

// globalPool is the shared backend connection pool for all TCP proxies.
var globalPool = &backendConnPool{
	entries: make(map[string][]*poolEntry),
}

// Get retrieves an idle connection from the pool, or returns nil if none available.
// Stale entries (older than maxIdleAge) are discarded.
func (p *backendConnPool) Get(addr string) net.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := p.entries[addr]
	for len(entries) > 0 {
		entry := entries[len(entries)-1]
		entries = entries[:len(entries)-1]
		if time.Since(entry.putTime) > maxIdleAge {
			entry.conn.Close()
			continue
		}
		p.entries[addr] = entries
		return entry.conn
	}
	// Clean up empty slice
	delete(p.entries, addr)
	return nil
}

// Put returns a connection to the pool for reuse.
// If the pool for this address is full (>= maxIdlePerBackend), the connection is closed.
func (p *backendConnPool) Put(addr string, conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := p.entries[addr]
	if len(entries) >= maxIdlePerBackend {
		conn.Close()
		return
	}
	p.entries[addr] = append(entries, &poolEntry{
		conn:    conn,
		putTime: time.Now(),
	})
}

// DrainBackend removes all pooled connections for a backend address.
// Call this when a backend is marked unhealthy.
func (p *backendConnPool) DrainBackend(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.entries[addr] {
		entry.conn.Close()
	}
	delete(p.entries, addr)
}

// PoolStats returns the number of idle connections per backend address (for testing).
func (p *backendConnPool) PoolStats() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := make(map[string]int, len(p.entries))
	for addr, entries := range p.entries {
		stats[addr] = len(entries)
	}
	return stats
}

// enableKeepAlive enables TCP keepalive with aggressive settings on a connection.
// This helps detect dead connections in the pool passively.
func enableKeepAlive(conn net.Conn) error {
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.SetKeepAlive(true); err != nil {
			return err
		}
		return tc.SetKeepAlivePeriod(keepAlivePeriod)
	}
	return nil
}

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

	healthy := p.pool.GetHealthyBackends()
	b, err := p.balancer.Select(healthy)
	if err != nil {
		p.logger.Error("no backends available", "error", err, "remote_addr", clientConn.RemoteAddr())
		return
	}

	b.IncConns()
	defer b.DecConns()

	addr := p.backendAddr(b)
	backendConn, err := p.getBackendConn(p.dialNetwork, addr)
	if err != nil {
		p.logger.Error("dial backend failed", "network", p.dialNetwork, "backend", b.RawURL, "error", err, "remote_addr", clientConn.RemoteAddr())
		return
	}

	BidirectionalCopy(clientConn, backendConn)

	p.putBackendConn(p.dialNetwork, addr, backendConn)

	p.logger.Info("tcp proxy completed", "network", p.dialNetwork, "backend", b.RawURL, "remote_addr", clientConn.RemoteAddr())
}

// getBackendConn tries to get a pooled backend connection, or creates a new one.
// Only TCP connections are pooled; Unix sockets always dial fresh.
func (p *TCPProxy) getBackendConn(network, addr string) (net.Conn, error) {
	if network == "tcp" {
		if conn := globalPool.Get(addr); conn != nil {
			return conn, nil
		}
	}
	conn, err := net.DialTimeout(network, addr, p.dialTimeout)
	if err != nil {
		return nil, err
	}
	if network == "tcp" {
		_ = enableKeepAlive(conn)
	}
	return conn, nil
}

// putBackendConn returns a backend TCP connection to the pool for reuse,
// or closes it if the connection is for Unix sockets.
func (p *TCPProxy) putBackendConn(network, addr string, conn net.Conn) {
	if network == "tcp" {
		conn.SetDeadline(time.Time{})
		// Check if the connection is still usable by setting a short read deadline.
		// If the backend already closed its side, this will detect it.
		conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		var buf [1]byte
		_, err := conn.Read(buf[:])
		conn.SetReadDeadline(time.Time{})
		if err == nil {
			// Read a byte — backend sent unexpected data, connection is not clean.
			conn.Close()
			return
		}
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			// Timeout means no data available — connection is still open and usable.
			globalPool.Put(addr, conn)
			return
		}
		// Any other error (EOF, connection reset, etc.) means the connection is dead.
		conn.Close()
		return
	}
	conn.Close()
}

// copyBufPool provides reusable 4KB buffers for BidirectionalCopy,
// eliminating the default 2×32KB per-connection heap allocations from io.Copy.
var copyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 4096)
		return &buf
	},
}

// BidirectionalCopy performs bidirectional copy between two connections,
// signaling EOF via CloseWrite on TCP connections when one direction finishes.
func BidirectionalCopy(clientConn, backendConn net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		bufp := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufp)
		io.CopyBuffer(backendConn, clientConn, *bufp)
		if tc, ok := backendConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		bufp := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufp)
		io.CopyBuffer(clientConn, backendConn, *bufp)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
}
