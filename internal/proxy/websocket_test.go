package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/backend"
	"github.com/garfieldlw/reverse-proxy/internal/balancer"
	"github.com/garfieldlw/reverse-proxy/internal/config"
	"github.com/gorilla/websocket"
)

func echoWSHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.WriteMessage(mt, msg)
	}
}

func TestWSProxySuccess(t *testing.T) {
	backendServer := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backendServer.Close()

	bu, _ := url.Parse(backendServer.URL)
	b := &backend.Backend{URL: bu, RawURL: backendServer.URL}
	pool := &backend.Pool{Name: "test", Backends: []*backend.Backend{b}}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewWSProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	wsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/ws"
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer clientConn.Close()

	testMsg := []byte("hello websocket proxy")
	err = clientConn.WriteMessage(websocket.TextMessage, testMsg)
	if err != nil {
		t.Fatalf("failed to write message: %v", err)
	}

	clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, received, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read message: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Errorf("expected message type %d, got %d", websocket.TextMessage, mt)
	}
	if string(received) != string(testMsg) {
		t.Errorf("expected %q, got %q", testMsg, received)
	}

	err = clientConn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		t.Logf("close write error (acceptable): %v", err)
	}
}

func TestWSProxyNoBackends(t *testing.T) {
	bu, _ := url.Parse("http://127.0.0.1:1")
	b := &backend.Backend{URL: bu, RawURL: "http://127.0.0.1:1"}
	b.SetStatus(backend.StatusUnhealthy)
	pool := &backend.Pool{Name: "test", Backends: []*backend.Backend{b}}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewWSProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/ws")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body["error"] != "no healthy backends available" {
		t.Errorf("expected error 'no healthy backends available', got %q", body["error"])
	}
}

func TestWSProxyBackendSelection(t *testing.T) {
	var hitCount1, hitCount2 atomic.Int64

	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		hitCount1.Add(1)
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			conn.WriteMessage(mt, append([]byte("b1-"), msg...))
		}
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		hitCount2.Add(1)
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			conn.WriteMessage(mt, append([]byte("b2-"), msg...))
		}
	}))
	defer backend2.Close()

	bu1, _ := url.Parse(backend1.URL)
	bu2, _ := url.Parse(backend2.URL)
	pool := &backend.Pool{
		Name: "test",
		Backends: []*backend.Backend{
			{URL: bu1, RawURL: backend1.URL},
			{URL: bu2, RawURL: backend2.URL},
		},
	}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewWSProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	wsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/ws"

	for i := 0; i < 2; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d failed: %v", i, err)
		}
		err = conn.WriteMessage(websocket.TextMessage, []byte("test"))
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, err = conn.ReadMessage()
		if err != nil {
			t.Fatalf("read %d failed: %v", i, err)
		}
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}

	if hitCount1.Load() != 1 {
		t.Errorf("expected backend1 hit count 1, got %d", hitCount1.Load())
	}
	if hitCount2.Load() != 1 {
		t.Errorf("expected backend2 hit count 1, got %d", hitCount2.Load())
	}
}

func TestWSProxyClose(t *testing.T) {
	backendServer := httptest.NewServer(http.HandlerFunc(echoWSHandler))
	defer backendServer.Close()

	bu, _ := url.Parse(backendServer.URL)
	b := &backend.Backend{URL: bu, RawURL: backendServer.URL}
	pool := &backend.Pool{Name: "test", Backends: []*backend.Backend{b}}
	rr := &balancer.RoundRobin{}
	logger := slog.Default()

	proxy := NewWSProxy(pool, rr, nil, logger, config.TransportConfig{})
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	wsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/ws"
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}

	err = clientConn.WriteMessage(websocket.TextMessage, []byte("before close"))
	if err != nil {
		t.Fatalf("failed to write message: %v", err)
	}
	clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read echo: %v", err)
	}

	err = clientConn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		t.Logf("close write error (acceptable): %v", err)
	}
	clientConn.Close()

	time.Sleep(100 * time.Millisecond)

	if b.GetActiveConns() != 0 {
		t.Errorf("expected active connections 0 after close, got %d", b.GetActiveConns())
	}
}
