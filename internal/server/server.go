package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/garfieldlw/reverse-proxy/internal/middleware/ratelimit"
	"github.com/garfieldlw/reverse-proxy/internal/proxy"
	proxytls "github.com/garfieldlw/reverse-proxy/internal/tls"
	"google.golang.org/grpc"
)

// Server orchestrates all proxy listeners based on configuration.
type Server struct {
	cfg            *config.Config
	logger         *slog.Logger
	pools          map[string]*backend.Pool
	healthCheckers map[string]*backend.HealthChecker
	balancers      map[string]balancer.Balancer
	limiter        *ratelimit.Limiter

	httpServers      []*http.Server
	streamListeners  []*streamListener  // TCP, Socket, RPC
	grpcServers      []*grpcListener    // gRPC
	packetListeners  []*packetListener  // UDP

	mu        sync.Mutex
	started   bool
	startErr  chan error
	startOnce sync.Once
}

// streamListener holds a TCP/Unix listener with its proxy and lifecycle channels.
// Used for TCP, Socket, and RPC protocols which share the same accept-loop pattern.
type streamListener struct {
	proxy  any // *proxy.TCPProxy or *proxy.RPCProxy
	addr   string
	ln     net.Listener
	done   chan error
	ctx    context.Context
	cancel context.CancelFunc
}

// packetListener holds a UDP packet connection with its proxy.
type packetListener struct {
	proxy  *proxy.UDPProxy
	addr   string
	conn   net.PacketConn
	done   chan error
	cancel context.CancelFunc
}

// grpcListener holds a gRPC server with its listener.
type grpcListener struct {
	server *grpc.Server
	ln     net.Listener
	done   chan error
	cancel context.CancelFunc
}

// NewServer creates a new Server from config.
func NewServer(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	s := &Server{
		cfg:            cfg,
		logger:         logger,
		pools:          make(map[string]*backend.Pool),
		healthCheckers: make(map[string]*backend.HealthChecker),
		balancers:      make(map[string]balancer.Balancer),
	}

	// 1. Create backend pools.
	for _, poolCfg := range cfg.BackendPools {
		pool, err := backend.NewPool(poolCfg, logger)
		if err != nil {
			return nil, fmt.Errorf("creating backend pool %q: %w", poolCfg.Name, err)
		}
		s.pools[poolCfg.Name] = pool
	}

	// 2. Create balancers for each pool.
	for _, poolCfg := range cfg.BackendPools {
		bal, err := balancer.New(poolCfg.Balancer)
		if err != nil {
			return nil, fmt.Errorf("creating balancer for pool %q: %w", poolCfg.Name, err)
		}
		s.balancers[poolCfg.Name] = bal
	}

	// 3. Create rate limiter.
	s.limiter = ratelimit.New(cfg.RateLimit, logger)

	// 4. Start health checkers for pools with health check enabled.
	for _, poolCfg := range cfg.BackendPools {
		if poolCfg.HealthCheck.Enabled {
			pool := s.pools[poolCfg.Name]
			checker := backend.NewHealthChecker(pool, poolCfg.HealthCheck, logger)
			checker.Start()
			s.healthCheckers[poolCfg.Name] = checker
		}
	}

	// 5. Create listeners based on cfg.Server.Listeners.
	for _, lc := range cfg.Server.Listeners {
		if err := s.createListener(lc); err != nil {
			// Stop any health checkers already started before returning.
			s.stopHealthCheckers()
			return nil, fmt.Errorf("creating listener %q: %w", lc.Name, err)
		}
	}

	// 6. If no listeners configured but Server.Listen is set, create a default HTTP listener.
	if len(cfg.Server.Listeners) == 0 && cfg.Server.Listen != "" {
		defaultLC := config.ListenerConfig{
			Name:       "default",
			Protocol:   "http",
			Listen:     cfg.Server.Listen,
			TLS:        cfg.Server.TLS,
		}
		// If there's exactly one pool, use it as the default backend.
		if len(cfg.BackendPools) == 1 {
			defaultLC.BackendPool = cfg.BackendPools[0].Name
			defaultLC.Routes = []config.RouteConfig{
				{Match: "/", BackendPool: cfg.BackendPools[0].Name},
			}
		}
		if err := s.createListener(defaultLC); err != nil {
			s.stopHealthCheckers()
			return nil, fmt.Errorf("creating default listener: %w", err)
		}
	}

	total := len(s.httpServers) + len(s.streamListeners) + len(s.grpcServers) + len(s.packetListeners)
	logger.Info("server initialized", "listeners", total, "pools", len(s.pools))

	return s, nil
}

// getPoolAndBalancer resolves a backend pool name to its Pool and Balancer.
func (s *Server) getPoolAndBalancer(poolName, protocol, listenerName string) (*backend.Pool, balancer.Balancer, error) {
	if poolName == "" {
		return nil, nil, fmt.Errorf("%s listener %q requires backend_pool", protocol, listenerName)
	}
	pool, ok := s.pools[poolName]
	if !ok {
		return nil, nil, fmt.Errorf("%s listener references unknown backend pool %q", protocol, poolName)
	}
	bal, ok := s.balancers[poolName]
	if !ok {
		return nil, nil, fmt.Errorf("no balancer for backend pool %q", poolName)
	}
	return pool, bal, nil
}

// createListener sets up the appropriate proxy and server for a listener config.
func (s *Server) createListener(lc config.ListenerConfig) error {
	switch lc.Protocol {
	case "http", "websocket":
		return s.createHTTPListener(lc)
	case "tcp":
		return s.createTCPListener(lc)
	case "grpc":
		return s.createGRPCListener(lc)
	case "socket":
		return s.createSocketListener(lc)
	case "udp":
		return s.createUDPListener(lc)
	case "rpc":
		return s.createRPCListener(lc)
	default:
		return fmt.Errorf("unsupported protocol %q", lc.Protocol)
	}
}

// createHTTPListener creates an HTTP or WebSocket listener.
func (s *Server) createHTTPListener(lc config.ListenerConfig) error {
	mux := http.NewServeMux()

	// Build routes from the listener config.
	routes := lc.Routes
	if len(routes) == 0 && lc.BackendPool != "" {
		routes = []config.RouteConfig{
			{Match: "/", BackendPool: lc.BackendPool},
		}
	}

	for _, route := range routes {
		pool, ok := s.pools[route.BackendPool]
		if !ok {
			return fmt.Errorf("route references unknown backend pool %q", route.BackendPool)
		}
		bal, ok := s.balancers[route.BackendPool]
		if !ok {
			return fmt.Errorf("no balancer for backend pool %q", route.BackendPool)
		}

		var handler http.Handler
		if lc.Protocol == "websocket" {
			handler = proxy.NewWSProxy(pool, bal, s.limiter, s.logger, s.cfg.Server.Transport).Handler()
		} else {
			handler = proxy.NewHTTPProxy(pool, bal, s.limiter, s.logger, s.cfg.Server.Transport).Handler()
		}

		mux.Handle(route.Match, handler)
	}

	srv := &http.Server{
		Addr:    lc.Listen,
		Handler: mux,
	}

	// Configure TLS if enabled.
	if lc.TLS.Enabled {
		tlsCfg, err := proxytls.NewTLSConfig(lc.TLS, s.logger)
		if err != nil {
			return fmt.Errorf("configuring TLS: %w", err)
		}
		srv.TLSConfig = tlsCfg
	}

	s.httpServers = append(s.httpServers, srv)
	return nil
}

// createTCPListener creates a TCP proxy listener.
func (s *Server) createTCPListener(lc config.ListenerConfig) error {
	pool, bal, err := s.getPoolAndBalancer(lc.BackendPool, "tcp", lc.Name)
	if err != nil {
		return err
	}

	tcpProxy := proxy.NewTCPProxy(pool, bal, s.logger)

	if s.cfg.Server.Transport.DialTimeout != "" {
		if d, err := time.ParseDuration(s.cfg.Server.Transport.DialTimeout); err == nil {
			tcpProxy.SetDialTimeout(d)
		}
	}

	ln, err := net.Listen("tcp", lc.Listen)
	if err != nil {
		return fmt.Errorf("tcp listen on %s: %w", lc.Listen, err)
	}

	s.streamListeners = append(s.streamListeners, &streamListener{
		proxy: tcpProxy,
		addr:  lc.Listen,
		ln:    ln,
		done:  make(chan error, 1),
	})
	return nil
}

// createGRPCListener creates a gRPC proxy listener.
func (s *Server) createGRPCListener(lc config.ListenerConfig) error {
	pool, bal, err := s.getPoolAndBalancer(lc.BackendPool, "grpc", lc.Name)
	if err != nil {
		return err
	}

	grpcProxy := proxy.NewGRPCProxy(pool, bal, s.logger)
	grpcServer := grpcProxy.Server()

	ln, err := net.Listen("tcp", lc.Listen)
	if err != nil {
		return fmt.Errorf("grpc listen on %s: %w", lc.Listen, err)
	}

	// Configure TLS if enabled.
	if lc.TLS.Enabled {
		tlsCfg, err := proxytls.NewTLSConfig(lc.TLS, s.logger)
		if err != nil {
			ln.Close()
			return fmt.Errorf("configuring gRPC TLS: %w", err)
		}
		// Wrap the net.Listener with TLS for gRPC.
		ln = tls.NewListener(ln, tlsCfg)
	}

	s.grpcServers = append(s.grpcServers, &grpcListener{
		server: grpcServer,
		ln:     ln,
		done:   make(chan error, 1),
	})
	return nil
}

// createSocketListener creates a Unix domain socket proxy listener.
func (s *Server) createSocketListener(lc config.ListenerConfig) error {
	pool, bal, err := s.getPoolAndBalancer(lc.BackendPool, "socket", lc.Name)
	if err != nil {
		return err
	}

	socketProxy := proxy.NewSocketProxy(pool, bal, s.logger)

	if s.cfg.Server.Transport.DialTimeout != "" {
		if d, err := time.ParseDuration(s.cfg.Server.Transport.DialTimeout); err == nil {
			socketProxy.SetDialTimeout(d)
		}
	}

	// Remove existing socket file if present (standard Unix socket practice).
	if _, err := os.Stat(lc.Listen); err == nil {
		os.Remove(lc.Listen)
	}

	ln, err := net.Listen("unix", lc.Listen)
	if err != nil {
		return fmt.Errorf("socket listen on %s: %w", lc.Listen, err)
	}

	s.streamListeners = append(s.streamListeners, &streamListener{
		proxy: socketProxy,
		addr:  lc.Listen,
		ln:    ln,
		done:  make(chan error, 1),
	})
	return nil
}

// createUDPListener creates a UDP proxy listener.
func (s *Server) createUDPListener(lc config.ListenerConfig) error {
	pool, bal, err := s.getPoolAndBalancer(lc.BackendPool, "udp", lc.Name)
	if err != nil {
		return err
	}

	udpProxy := proxy.NewUDPProxy(pool, bal, s.logger)

	// Apply UDP-specific config.
	if s.cfg.Server.UDP.MaxSessions > 0 {
		udpProxy.SetMaxSessions(s.cfg.Server.UDP.MaxSessions)
	}
	if s.cfg.Server.UDP.DialTimeout != "" {
		if d, err := time.ParseDuration(s.cfg.Server.UDP.DialTimeout); err == nil {
			udpProxy.SetDialTimeout(d)
		}
	}
	if s.cfg.Server.UDP.SessionTimeout != "" {
		if d, err := time.ParseDuration(s.cfg.Server.UDP.SessionTimeout); err == nil {
			udpProxy.SetSessionTimeout(d)
		}
	}

	conn, err := net.ListenPacket("udp", lc.Listen)
	if err != nil {
		return fmt.Errorf("udp listen on %s: %w", lc.Listen, err)
	}

	s.packetListeners = append(s.packetListeners, &packetListener{
		proxy: udpProxy,
		addr:  lc.Listen,
		conn:  conn,
		done:  make(chan error, 1),
	})
	return nil
}

// createRPCListener creates an RPC proxy listener.
func (s *Server) createRPCListener(lc config.ListenerConfig) error {
	pool, bal, err := s.getPoolAndBalancer(lc.BackendPool, "rpc", lc.Name)
	if err != nil {
		return err
	}

	rpcProxy := proxy.NewRPCProxy(pool, bal, s.logger)

	if s.cfg.Server.Transport.DialTimeout != "" {
		if d, err := time.ParseDuration(s.cfg.Server.Transport.DialTimeout); err == nil {
			rpcProxy.SetDialTimeout(d)
		}
	}

	ln, err := net.Listen("tcp", lc.Listen)
	if err != nil {
		return fmt.Errorf("rpc listen on %s: %w", lc.Listen, err)
	}

	s.streamListeners = append(s.streamListeners, &streamListener{
		proxy: rpcProxy,
		addr:  lc.Listen,
		ln:    ln,
		done:  make(chan error, 1),
	})
	return nil
}

// Start launches all listeners. It returns an error if any listener fails to start.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("server already started")
	}
	s.started = true
	s.startErr = make(chan error, 1)
	s.mu.Unlock()

	// Start HTTP/WS servers.
	for _, srv := range s.httpServers {
		srv := srv
		go func() {
			s.logger.Info("listener started", "protocol", "http", "addr", srv.Addr)
			var err error
			if srv.TLSConfig != nil {
				err = srv.ListenAndServeTLS("", "")
			} else {
				err = srv.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				select {
				case s.startErr <- fmt.Errorf("http server %s: %w", srv.Addr, err):
				default:
				}
			}
		}()
	}

	// Start stream listeners (TCP, Socket, RPC).
	for _, sl := range s.streamListeners {
		sl := sl
		ctx, cancel := context.WithCancel(context.Background())
		sl.ctx = ctx
		sl.cancel = cancel
		go s.acceptLoop(sl)
	}

	// Start gRPC servers.
	for _, gl := range s.grpcServers {
		gl := gl
		ctx, cancel := context.WithCancel(context.Background())
		gl.cancel = cancel
		go func() {
			s.logger.Info("listener started", "protocol", "grpc", "addr", gl.ln.Addr().String())
			if err := gl.server.Serve(gl.ln); err != nil {
				select {
				case <-ctx.Done():
					gl.done <- nil
				default:
					gl.done <- fmt.Errorf("grpc serve: %w", err)
				}
			}
		}()
	}

	// Start UDP proxies.
	for _, ul := range s.packetListeners {
		ul := ul
		ctx, cancel := context.WithCancel(context.Background())
		ul.cancel = cancel
		go func() {
			s.logger.Info("listener started", "protocol", "udp", "addr", ul.addr)
			ul.proxy.ServePacketConn(ctx, ul.conn)
			ul.done <- nil
		}()
	}

	return nil
}

// acceptLoop runs the accept loop for a stream listener, dispatching connections
// to the appropriate handler based on the proxy type.
func (s *Server) acceptLoop(sl *streamListener) {
	protocol := "tcp"
	switch p := sl.proxy.(type) {
	case *proxy.RPCProxy:
		protocol = "rpc"
	case *proxy.TCPProxy:
		if p.GetListenNetwork() == "unix" {
			protocol = "socket"
		}
	}

	s.logger.Info("listener started", "protocol", protocol, "addr", sl.addr)

	for {
		select {
		case <-sl.ctx.Done():
			sl.done <- nil
			return
		default:
		}

		// Set deadline for graceful shutdown on socket/rpc listeners.
		switch protocol {
		case "socket":
			sl.ln.(*net.UnixListener).SetDeadline(time.Now().Add(1 * time.Second))
		case "rpc":
			sl.ln.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))
		}

		conn, err := sl.ln.Accept()
		if err != nil {
			if sl.ctx.Err() != nil {
				sl.done <- nil
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			sl.done <- fmt.Errorf("%s accept %s: %w", protocol, sl.addr, err)
			return
		}

		switch p := sl.proxy.(type) {
		case *proxy.TCPProxy:
			go p.ServeTCP(conn)
		case *proxy.RPCProxy:
			go p.ServeRPC(conn)
		}
	}
}

// Shutdown performs a graceful shutdown of all listeners within the given timeout.
func (s *Server) Shutdown(timeout time.Duration) error {
	s.logger.Info("server shutting down", "timeout", timeout)

	// 1. Stop all health checkers.
	s.stopHealthCheckers()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var wg sync.WaitGroup

	// 2. Shutdown all HTTP servers.
	for _, srv := range s.httpServers {
		wg.Add(1)
		srv := srv
		go func() {
			defer wg.Done()
			if err := srv.Shutdown(ctx); err != nil {
				s.logger.Error("http server shutdown error", "addr", srv.Addr, "error", err)
			}
		}()
	}

	// 3. GracefulStop gRPC servers.
	for _, gl := range s.grpcServers {
		wg.Add(1)
		gl := gl
		go func() {
			defer wg.Done()
			stopped := make(chan struct{})
			go func() {
				gl.server.GracefulStop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-ctx.Done():
				s.logger.Warn("grpc server graceful stop timed out, stopping forcefully")
				gl.server.Stop()
			}
			if gl.cancel != nil {
				gl.cancel()
			}
		}()
	}

	// 4. Close stream listeners and cancel their accept loops.
	for _, sl := range s.streamListeners {
		if sl.ln != nil {
			sl.ln.Close()
		}
		if sl.cancel != nil {
			sl.cancel()
		}
	}

	// Close packet listeners.
	for _, pl := range s.packetListeners {
		if pl.conn != nil {
			pl.conn.Close()
		}
		if pl.cancel != nil {
			pl.cancel()
		}
	}

	// 5. Wait for all goroutines to complete (with timeout).
	done := make(chan struct{})
	go func() {
		wg.Wait()
		// Also wait for stream and gRPC listener goroutines to finish.
		for _, sl := range s.streamListeners {
			if sl.done != nil {
				<-sl.done
			}
		}
		for _, gl := range s.grpcServers {
			if gl.done != nil {
				<-gl.done
			}
		}
		for _, pl := range s.packetListeners {
			if pl.done != nil {
				<-pl.done
			}
		}
		close(done)
	}()

	select {
	case <-done:
		s.logger.Info("server shutdown complete")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("server shutdown timed out after %v", timeout)
	}
}

// stopHealthCheckers stops all health checkers.
func (s *Server) stopHealthCheckers() {
	for name, checker := range s.healthCheckers {
		checker.Stop()
		s.logger.Debug("health checker stopped", "pool", name)
	}
}
