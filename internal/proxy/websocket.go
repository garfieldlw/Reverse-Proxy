package proxy

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"github.com/garfieldlw/reverse-proxy/internal/middleware/ratelimit"
	"github.com/gorilla/websocket"
)

// WSProxy is a WebSocket reverse proxy that load balances across backends.
type WSProxy struct {
	pool     *backend.Pool
	balancer balancer.Balancer
	limiter  *ratelimit.Limiter
	logger   *slog.Logger
	upgrader websocket.Upgrader
	dialer   *websocket.Dialer
}

// NewWSProxy creates a new WebSocket reverse proxy handler.
func NewWSProxy(pool *backend.Pool, balancer balancer.Balancer, limiter *ratelimit.Limiter, logger *slog.Logger) *WSProxy {
	return &WSProxy{
		pool:     pool,
		balancer: balancer,
		limiter:  limiter,
		logger:   logger,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		dialer: &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		},
	}
}

// ServeHTTP handles WebSocket upgrade requests and proxies them to a backend.
func (p *WSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	healthy := p.pool.GetHealthyBackends()
	b, err := p.balancer.Select(healthy)
	if err != nil {
		p.logger.Error("no backends available", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(errBytesNoHealthy)
		return
	}

	clientConn, err := p.upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.logger.Error("ws upgrade failed", "error", err)
		return
	}

	b.IncConns()
	defer b.DecConns()
	defer clientConn.Close()

	// Build backend WebSocket URL.
	targetURL := buildWSURL(b.URL, r.URL)
	p.logger.Info("dialing backend", "url", targetURL)

	// Forward request headers, excluding WebSocket-specific ones.
	requestHeaders := http.Header{}
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if lower == "upgrade" || lower == "connection" || lower == "sec-websocket-key" ||
			lower == "sec-websocket-version" || lower == "sec-websocket-extensions" ||
			lower == "sec-websocket-protocol" {
			continue
		}
		for _, v := range values {
			requestHeaders.Add(key, v)
		}
	}

	backendConn, _, err := p.dialer.Dial(targetURL, requestHeaders)
	if err != nil {
		p.logger.Error("ws dial backend failed", "url", targetURL, "error", err)
		return
	}
	defer backendConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// Client → Backend
	go func() {
		defer wg.Done()
		p.copyMessages(clientConn, backendConn, "client_to_backend")
	}()

	// Backend → Client
	go func() {
		defer wg.Done()
		p.copyMessages(backendConn, clientConn, "backend_to_client")
	}()

	wg.Wait()
}

// copyMessages reads messages from src and writes them to dst until an error or close.
func (p *WSProxy) copyMessages(src, dst *websocket.Conn, direction string) {
	for {
		messageType, message, err := src.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				p.logger.Error("ws read error", "direction", direction, "error", err)
			}
			break
		}

		err = dst.WriteMessage(messageType, message)
		if err != nil {
			p.logger.Error("ws write error", "direction", direction, "error", err)
			break
		}
	}

	// Send close frame to destination.
	dst.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

// Handler returns an http.Handler that wraps ServeHTTP with rate limiting and recovery.
func (p *WSProxy) Handler() http.Handler {
	handler := http.Handler(http.HandlerFunc(p.ServeHTTP))

	// Apply rate limiting middleware.
	if p.limiter != nil {
		handler = p.limiter.Middleware(handler)
	}

	// Apply recovery middleware.
	return recoveryMiddleware(handler, p.logger)
}

// buildWSURL converts a backend HTTP URL to a WebSocket URL, preserving the request path and query.
func buildWSURL(backendURL *url.URL, requestURL *url.URL) string {
	scheme := "ws"
	if backendURL.Scheme == "https" {
		scheme = "wss"
	}

	host := backendURL.Host
	path := requestURL.Path
	query := requestURL.RawQuery

	u := url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     path,
		RawQuery: query,
	}
	return u.String()
}
