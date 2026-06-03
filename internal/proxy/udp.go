package proxy

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
)

const (
	defaultUDPDialTimeout   = 10 * time.Second
	defaultUDPSessionTimeout = 30 * time.Second
	udpMaxPacketSize        = 65535
)

// UDPProxy is a UDP packet proxy with session-based routing.
// It maps client addresses to backend connections so response packets
// can be routed back to the correct client.
type UDPProxy struct {
	pool           *backend.Pool
	balancer       balancer.Balancer
	logger         *slog.Logger
	dialTimeout    time.Duration
	sessionTimeout time.Duration
	mu             sync.Mutex
	sessions       map[string]*udpSession // clientAddr key -> session
}

type udpSession struct {
	backendConn net.Conn
	backend     *backend.Backend
	lastActive  time.Time
	mu          sync.Mutex
}

func (s *udpSession) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *udpSession) lastActiveTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActive
}

// NewUDPProxy creates a new UDP reverse proxy.
func NewUDPProxy(pool *backend.Pool, balancer balancer.Balancer, logger *slog.Logger) *UDPProxy {
	return &UDPProxy{
		pool:           pool,
		balancer:       balancer,
		logger:         logger,
		dialTimeout:    defaultUDPDialTimeout,
		sessionTimeout: defaultUDPSessionTimeout,
		sessions:       make(map[string]*udpSession),
	}
}

// SetSessionTimeout sets the idle timeout for UDP sessions.
func (p *UDPProxy) SetSessionTimeout(d time.Duration) {
	p.sessionTimeout = d
}

// SetDialTimeout sets the timeout for dialing backend connections.
func (p *UDPProxy) SetDialTimeout(d time.Duration) {
	p.dialTimeout = d
}

// ServePacketConn reads packets from a net.PacketConn and forwards them
// to backends, routing responses back to the originating clients.
// It blocks until the context is cancelled or the connection is closed.
func (p *UDPProxy) ServePacketConn(ctx context.Context, conn net.PacketConn) error {
	// Start session cleanup goroutine
	cleanupCtx, cleanupCancel := context.WithCancel(ctx)
	defer cleanupCancel()

	go p.cleanupSessions(cleanupCtx)

	buf := make([]byte, udpMaxPacketSize)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Set a read deadline so we can check context periodically
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, clientAddr, err := conn.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // Check context and retry
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.logger.Error("udp read error", "error", err)
			continue
		}

		go p.handlePacket(ctx, conn, clientAddr, buf[:n])
	}
}

// handlePacket processes a single incoming UDP packet.
func (p *UDPProxy) handlePacket(ctx context.Context, conn net.PacketConn, clientAddr net.Addr, data []byte) {
	key := clientAddr.String()

	p.mu.Lock()
	session, exists := p.sessions[key]
	if !exists {
		// Create new session under the write lock to prevent TOCTOU races.
		healthy := p.pool.GetHealthyBackends()
		b, err := p.balancer.Select(healthy)
		if err != nil {
			p.mu.Unlock()
			p.logger.Error("no backends available for udp", "client", clientAddr, "error", err)
			return
		}

		b.IncConns()

		dialer := net.Dialer{Timeout: p.dialTimeout}
		backendConn, err := dialer.DialContext(ctx, "udp", b.URL.Host)
		if err != nil {
			b.DecConns()
			p.mu.Unlock()
			p.logger.Error("udp dial backend failed", "backend", b.RawURL, "error", err)
			return
		}

		session = &udpSession{
			backendConn: backendConn,
			backend:     b,
			lastActive:  time.Now(),
		}

		p.sessions[key] = session
		p.mu.Unlock()

		// Start goroutine to read responses from backend and send back to client
		go p.relayFromBackend(conn, clientAddr, session)
	} else {
		p.mu.Unlock()
	}

	// Update last active time
	session.touch()

	// Forward packet to backend
	_, err := session.backendConn.Write(data)
	if err != nil {
		p.logger.Error("udp write to backend failed", "backend", session.backend.RawURL, "error", err)
		p.closeSession(key, session)
	}
}

// relayFromBackend reads responses from a backend UDP connection and sends
// them back to the client.
func (p *UDPProxy) relayFromBackend(conn net.PacketConn, clientAddr net.Addr, session *udpSession) {
	buf := make([]byte, udpMaxPacketSize)
	for {
		// Set read deadline based on session timeout
		session.backendConn.SetReadDeadline(time.Now().Add(p.sessionTimeout))
		n, err := session.backendConn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Check if session is still active
				if time.Since(session.lastActiveTime()) > p.sessionTimeout {
					break
				}
				continue
			}
			// Connection closed or error
			break
		}

		session.touch()

		_, err = conn.WriteTo(buf[:n], clientAddr)
		if err != nil {
			p.logger.Error("udp write to client failed", "client", clientAddr, "error", err)
			break
		}
	}

	p.closeSession(clientAddr.String(), session)
}

// closeSession removes a session and releases resources.
func (p *UDPProxy) closeSession(key string, session *udpSession) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.sessions[key]; exists {
		session.backendConn.Close()
		session.backend.DecConns()
		delete(p.sessions, key)
	}
}

// cleanupSessions periodically removes expired sessions.
func (p *UDPProxy) cleanupSessions(ctx context.Context) {
	ticker := time.NewTicker(p.sessionTimeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Close all remaining sessions
			p.mu.Lock()
			for key, session := range p.sessions {
				session.backendConn.Close()
				session.backend.DecConns()
				delete(p.sessions, key)
			}
			p.mu.Unlock()
			return
		case <-ticker.C:
			p.mu.Lock()
			for key, session := range p.sessions {
				if time.Since(session.lastActiveTime()) > p.sessionTimeout {
					session.backendConn.Close()
					session.backend.DecConns()
					delete(p.sessions, key)
					p.logger.Debug("udp session expired", "client", key)
				}
			}
			p.mu.Unlock()
		}
	}
}

// SessionCount returns the number of active UDP sessions (for testing).
func (p *UDPProxy) SessionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}
