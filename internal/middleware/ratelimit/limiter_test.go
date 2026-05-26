package ratelimit

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

func TestGlobalRateLimit(t *testing.T) {
	cfg := config.RateLimitConfig{
		Enabled:          true,
		RequestsPerSecond: 1,
		Burst:            3,
		PerIP:            false,
	}
	limiter := New(cfg, slog.Default())

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := limiter.Middleware(okHandler)

	// Send burst+1 requests; the last one should be rate limited.
	allowed := 0
	rateLimited := 0
	for i := 0; i < cfg.Burst+1; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			allowed++
		} else if rec.Code == http.StatusTooManyRequests {
			rateLimited++
		}
	}

	if allowed != cfg.Burst {
		t.Errorf("expected %d allowed requests, got %d", cfg.Burst, allowed)
	}
	if rateLimited != 1 {
		t.Errorf("expected 1 rate limited request, got %d", rateLimited)
	}
}

func TestPerIPRateLimit(t *testing.T) {
	cfg := config.RateLimitConfig{
		Enabled:          true,
		RequestsPerSecond: 1,
		Burst:            2,
		PerIP:            true,
	}
	limiter := New(cfg, slog.Default())

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := limiter.Middleware(okHandler)

	// Each IP should get its own bucket.
	ips := []string{"1.1.1.1:1111", "2.2.2.2:2222"}
	for _, ip := range ips {
		allowed := 0
		rateLimited := 0
		for i := 0; i < cfg.Burst+1; i++ {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.RemoteAddr = ip
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				allowed++
			} else if rec.Code == http.StatusTooManyRequests {
				rateLimited++
			}
		}
		if allowed != cfg.Burst {
			t.Errorf("IP %s: expected %d allowed, got %d", ip, cfg.Burst, allowed)
		}
		if rateLimited != 1 {
			t.Errorf("IP %s: expected 1 rate limited, got %d", ip, rateLimited)
		}
	}
}

func TestBurstExceeded(t *testing.T) {
	cfg := config.RateLimitConfig{
		Enabled:          true,
		RequestsPerSecond: 1,
		Burst:            1,
		PerIP:            false,
	}
	limiter := New(cfg, slog.Default())

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := limiter.Middleware(okHandler)

	// First request should succeed.
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("first request: expected 200, got %d", rec.Code)
	}

	// Second request should be rate limited.
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request: expected 429, got %d", rec.Code)
	}
}

func Test429ResponseFormat(t *testing.T) {
	cfg := config.RateLimitConfig{
		Enabled:          true,
		RequestsPerSecond: 1,
		Burst:            1,
		PerIP:            false,
	}
	limiter := New(cfg, slog.Default())

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := limiter.Middleware(okHandler)

	// Exhaust the burst.
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Next request should get 429.
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

	// Check Retry-After header.
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Error("expected Retry-After header to be set")
	}

	// Check Content-Type.
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	// Check JSON body.
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse JSON body: %v", err)
	}
	if body["error"] != "rate limit exceeded" {
		t.Errorf("expected error 'rate limit exceeded', got %v", body["error"])
	}
	if _, ok := body["retry_after"]; !ok {
		t.Error("expected retry_after field in response body")
	}
}

func TestDisabledLimiter(t *testing.T) {
	cfg := config.RateLimitConfig{
		Enabled:          false,
		RequestsPerSecond: 1,
		Burst:            1,
		PerIP:            false,
	}
	limiter := New(cfg, slog.Default())

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := limiter.Middleware(okHandler)

	// All requests should pass through when disabled.
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: expected 200, got %d", i, rec.Code)
		}
	}
}

func TestIPExtraction(t *testing.T) {
	cfg := config.RateLimitConfig{
		Enabled:          true,
		RequestsPerSecond: 1,
		Burst:            10,
		PerIP:            true,
	}
	limiter := New(cfg, slog.Default())

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		xri        string
		wantIP     string
	}{
		{
			name:       "RemoteAddr only",
			remoteAddr: "10.0.0.1:5678",
			wantIP:     "10.0.0.1",
		},
		{
			name:       "X-Forwarded-For takes precedence",
			remoteAddr: "10.0.0.1:5678",
			xff:        "9.9.9.9, 8.8.8.8",
			wantIP:     "9.9.9.9",
		},
		{
			name:       "X-Real-IP takes precedence over RemoteAddr",
			remoteAddr: "10.0.0.1:5678",
			xri:        "7.7.7.7",
			wantIP:     "7.7.7.7",
		},
		{
			name:       "X-Forwarded-For takes precedence over X-Real-IP",
			remoteAddr: "10.0.0.1:5678",
			xff:        "9.9.9.9",
			xri:        "7.7.7.7",
			wantIP:     "9.9.9.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				req.Header.Set("X-Real-IP", tt.xri)
			}

			got := limiter.extractIP(req)
			if got != tt.wantIP {
				t.Errorf("extractIP() = %q, want %q", got, tt.wantIP)
			}
		})
	}
}

func TestConcurrentAccess(t *testing.T) {
	cfg := config.RateLimitConfig{
		Enabled:          true,
		RequestsPerSecond: 1000,
		Burst:            1000,
		PerIP:            true,
	}
	limiter := New(cfg, slog.Default())

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := limiter.Middleware(okHandler)

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ip := "10.0.0."
			// Use a few different IPs.
			switch idx % 3 {
			case 0:
				ip += "1:1234"
			case 1:
				ip += "2:5678"
			case 2:
				ip += "3:9012"
			}
			for j := 0; j < 10; j++ {
				req := httptest.NewRequest(http.MethodGet, "/test", nil)
				req.RemoteAddr = ip
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				// We just want to ensure no panics or data races occur.
				if rec.Code != http.StatusOK && rec.Code != http.StatusTooManyRequests {
					select {
					case errors <- nil:
					default:
					}
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent access error: %v", err)
		}
	}
}
