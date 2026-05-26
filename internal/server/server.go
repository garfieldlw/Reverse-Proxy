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

	httpServers   []*http.Server
	tcpProxies    []*tcpListener
	grpcServers   []*grpcListener
	socketProxies []*socketListener
	udpProxies    []*udpListener
	rpcProxies    []*rpcListener

	mu sync.Mutex
	started   bool
	startErr  chan error
	startOnce sync.Once
}

type tcpListener struct {
	proxy  *proxy.TCPProxy
	addr   string
	ln     net.Listener
	done   chan error
	cancel context.CancelFunc
}

type grpcListener struct {
	server *grpc.Server
	ln     net.Listener
	done   chan error
	cancel context.CancelFunc
}

type socketListener struct {
	proxy  *proxy.TCPProxy
	addr   string
	ln     net.Listener
	done   chan error
	cancel context.CancelFunc
}

type udpListener struct {
	proxy  *proxy.UDPProxy
	addr   string
	conn   net.PacketConn
	done   chan error
	cancel context.CancelFunc
}

type rpcListener struct {
	proxy  *proxy.RPCProxy
	addr   string
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
			Name:     "default",
			Protocol: "http",
			Listen:   cfg.Server.Listen,
			TLS:      cfg.Server.TLS,
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

	total := len(s.httpServers) + len(s.tcpProxies) + len(s.grpcServers) + len(s.socketProxies) + len(s.udpProxies) + len(s.rpcProxies)
	logger.Info("server initialized", "listeners", total, "pools", len(s.pools))

	return s, nil
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
			handler = proxy.NewWSProxy(pool, bal, s.limiter, s.logger).Handler()
		} else {
			handler = proxy.NewHTTPProxy(pool, bal, s.limiter, s.logger).Handler()
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
	poolName := lc.BackendPool
	if poolName == "" {
		return fmt.Errorf("tcp listener %q requires backend_pool", lc.Name)
	}

	pool, ok := s.pools[poolName]
	if !ok {
		return fmt.Errorf("tcp listener references unknown backend pool %q", poolName)
	}
	bal, ok := s.balancers[poolName]
	if !ok {
		return fmt.Errorf("no balancer for backend pool %q", poolName)
	}

	tcpProxy := proxy.NewTCPProxy(pool, bal, s.logger)

	ln, err := net.Listen("tcp", lc.Listen)
	if err != nil {
		return fmt.Errorf("tcp listen on %s: %w", lc.Listen, err)
	}

	s.tcpProxies = append(s.tcpProxies, &tcpListener{
		proxy: tcpProxy,
		addr:  lc.Listen,
		ln:    ln,
		done:  make(chan error, 1),
	})
	return nil
}

// createGRPCListener creates a gRPC proxy listener.
func (s *Server) createGRPCListener(lc config.ListenerConfig) error {
	poolName := lc.BackendPool
	if poolName == "" {
		return fmt.Errorf("grpc listener %q requires backend_pool", lc.Name)
	}

	pool, ok := s.pools[poolName]
	if !ok {
		return fmt.Errorf("grpc listener references unknown backend pool %q", poolName)
	}
	bal, ok := s.balancers[poolName]
	if !ok {
		return fmt.Errorf("no balancer for backend pool %q", poolName)
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
	poolName := lc.BackendPool
	if poolName == "" {
		return fmt.Errorf("socket listener %q requires backend_pool", lc.Name)
	}

	pool, ok := s.pools[poolName]
	if !ok {
		return fmt.Errorf("socket listener references unknown backend pool %q", poolName)
	}
	bal, ok := s.balancers[poolName]
	if !ok {
		return fmt.Errorf("no balancer for backend pool %q", poolName)
	}

	socketProxy := proxy.NewSocketProxy(pool, bal, s.logger)

	// Remove existing socket file if present (standard Unix socket practice).
	if _, err := os.Stat(lc.Listen); err == nil {
		os.Remove(lc.Listen)
	}

	ln, err := net.Listen("unix", lc.Listen)
	if err != nil {
		return fmt.Errorf("socket listen on %s: %w", lc.Listen, err)
	}

	s.socketProxies = append(s.socketProxies, &socketListener{
		proxy: socketProxy,
		addr:  lc.Listen,
		ln:    ln,
		done:  make(chan error, 1),
	})
	return nil
}

// createUDPListener creates a UDP proxy listener.
func (s *Server) createUDPListener(lc config.ListenerConfig) error {
	poolName := lc.BackendPool
	if poolName == "" {
		return fmt.Errorf("udp listener %q requires backend_pool", lc.Name)
	}

	pool, ok := s.pools[poolName]
	if !ok {
		return fmt.Errorf("udp listener references unknown backend pool %q", poolName)
	}
	bal, ok := s.balancers[poolName]
	if !ok {
		return fmt.Errorf("no balancer for backend pool %q", poolName)
	}

	udpProxy := proxy.NewUDPProxy(pool, bal, s.logger)

	conn, err := net.ListenPacket("udp", lc.Listen)
	if err != nil {
		return fmt.Errorf("udp listen on %s: %w", lc.Listen, err)
	}

	s.udpProxies = append(s.udpProxies, &udpListener{
		proxy: udpProxy,
		addr:  lc.Listen,
		conn:  conn,
		done:  make(chan error, 1),
	})
	return nil
}

// createRPCListener creates an RPC proxy listener.
func (s *Server) createRPCListener(lc config.ListenerConfig) error {
	poolName := lc.BackendPool
	if poolName == "" {
		return fmt.Errorf("rpc listener %q requires backend_pool", lc.Name)
	}

	pool, ok := s.pools[poolName]
	if !ok {
		return fmt.Errorf("rpc listener references unknown backend pool %q", poolName)
	}
	bal, ok := s.balancers[poolName]
	if !ok {
		return fmt.Errorf("no balancer for backend pool %q", poolName)
	}

	rpcProxy := proxy.NewRPCProxy(pool, bal, s.logger)

	ln, err := net.Listen("tcp", lc.Listen)
	if err != nil {
		return fmt.Errorf("rpc listen on %s: %w", lc.Listen, err)
	}

	s.rpcProxies = append(s.rpcProxies, &rpcListener{
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

	// Start TCP proxies.
	for _, tl := range s.tcpProxies {
		tl := tl
		ctx, cancel := context.WithCancel(context.Background())
		tl.cancel = cancel
		go func() {
			s.logger.Info("listener started", "protocol", "tcp", "addr", tl.addr)
			// Accept connections manually so we can respect context cancellation.
			for {
				select {
				case <-ctx.Done():
					tl.done <- nil
					return
				default:
				}
				conn, err := tl.ln.Accept()
				if err != nil {
					select {
					case <-ctx.Done():
						tl.done <- nil
						return
					default:
						tl.done <- fmt.Errorf("tcp accept %s: %w", tl.addr, err)
						return
					}
				}
				go tl.proxy.ServeTCP(conn)
			}
		}()
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

	// Start socket proxies.
	for _, sl := range s.socketProxies {
		sl := sl
		ctx, cancel := context.WithCancel(context.Background())
		sl.cancel = cancel
		go func() {
			s.logger.Info("listener started", "protocol", "socket", "addr", sl.addr)
			for {
				select {
				case <-ctx.Done():
					sl.done <- nil
					return
				default:
				}
				sl.ln.(*net.UnixListener).SetDeadline(time.Now().Add(1 * time.Second))
				conn, err := sl.ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						sl.done <- nil
						return
					}
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}
					sl.done <- fmt.Errorf("socket accept %s: %w", sl.addr, err)
					return
				}
				go sl.proxy.ServeTCP(conn)
			}
		}()
	}

	// Start UDP proxies.
	for _, ul := range s.udpProxies {
		ul := ul
		ctx, cancel := context.WithCancel(context.Background())
		ul.cancel = cancel
		go func() {
			s.logger.Info("listener started", "protocol", "udp", "addr", ul.addr)
			ul.proxy.ServePacketConn(ctx, ul.conn)
			ul.done <- nil
		}()
	}

	// Start RPC proxies.
	for _, rl := range s.rpcProxies {
		rl := rl
		ctx, cancel := context.WithCancel(context.Background())
		rl.cancel = cancel
		go func() {
			s.logger.Info("listener started", "protocol", "rpc", "addr", rl.addr)
			for {
				select {
				case <-ctx.Done():
					rl.done <- nil
					return
				default:
				}
				rl.ln.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))
				conn, err := rl.ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						rl.done <- nil
						return
					}
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						continue
					}
					rl.done <- fmt.Errorf("rpc accept %s: %w", rl.addr, err)
					return
				}
				go rl.proxy.ServeRPC(conn)
			}
		}()
	}

	return nil
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

	// 4. Close TCP listeners and cancel their accept loops.
	for _, tl := range s.tcpProxies {
		if tl.ln != nil {
			tl.ln.Close()
		}
		if tl.cancel != nil {
			tl.cancel()
		}
	}

	// Close socket listeners.
	for _, sl := range s.socketProxies {
		if sl.ln != nil {
			sl.ln.Close()
		}
		if sl.cancel != nil {
			sl.cancel()
		}
	}

	// Close UDP listeners.
	for _, ul := range s.udpProxies {
		if ul.conn != nil {
			ul.conn.Close()
		}
		if ul.cancel != nil {
			ul.cancel()
		}
	}

	// Close RPC listeners.
	for _, rl := range s.rpcProxies {
		if rl.ln != nil {
			rl.ln.Close()
		}
		if rl.cancel != nil {
			rl.cancel()
		}
	}

	// 5. Wait for all goroutines to complete (with timeout).
	done := make(chan struct{})
	go func() {
		wg.Wait()
		// Also wait for TCP and gRPC listener goroutines to finish.
		for _, tl := range s.tcpProxies {
			if tl.done != nil {
				<-tl.done
			}
		}
		for _, gl := range s.grpcServers {
			if gl.done != nil {
				<-gl.done
			}
		}
		for _, sl := range s.socketProxies {
			if sl.done != nil {
				<-sl.done
			}
		}
		for _, ul := range s.udpProxies {
			if ul.done != nil {
				<-ul.done
			}
		}
		for _, rl := range s.rpcProxies {
			if rl.done != nil {
				<-rl.done
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
