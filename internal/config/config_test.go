package config

import (
	"os"
	"path/filepath"
	"testing"
)

// validYAML is a complete, valid configuration for testing Load.
const validYAML = `
server:
  listen: ":9090"
  tls:
    enabled: false
  listeners:
    - name: http-main
      protocol: http
      listen: ":8080"
      routes:
        - match: "/api/*"
          backend_pool: api-pool
    - name: ws-main
      protocol: websocket
      listen: ":8081"
      backend_pool: ws-pool
    - name: grpc-main
      protocol: grpc
      listen: ":50051"
      backend_pool: grpc-pool
    - name: tcp-main
      protocol: tcp
      listen: ":3306"
      backend_pool: tcp-pool

backend_pools:
  - name: api-pool
    balancer: round_robin
    health_check:
      enabled: true
      interval: "15s"
      timeout: "3s"
      path: "/health"
      unhealthy_threshold: 3
      healthy_threshold: 2
    backends:
      - url: "http://127.0.0.1:8001"
        weight: 3
      - url: "http://127.0.0.1:8002"
        weight: 2
  - name: ws-pool
    balancer: least_connections
    health_check:
      enabled: false
    backends:
      - url: "ws://127.0.0.1:9001"
  - name: grpc-pool
    balancer: random
    backends:
      - url: "grpc://127.0.0.1:50052"
  - name: tcp-pool
    balancer: weighted_round_robin
    backends:
      - url: "tcp://127.0.0.1:3307"

rate_limit:
  enabled: true
  requests_per_second: 100.5
  burst: 300
  per_ip: true

logging:
  level: debug
  format: text
  output: stderr
`

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp YAML: %v", err)
	}
	return path
}

func TestLoadValidConfig(t *testing.T) {
	path := writeTempYAML(t, validYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	// Server
	if cfg.Server.Listen != ":9090" {
		t.Errorf("Server.Listen = %q, want %q", cfg.Server.Listen, ":9090")
	}
	if len(cfg.Server.Listeners) != 4 {
		t.Fatalf("len(Listeners) = %d, want 4", len(cfg.Server.Listeners))
	}

	// Listener 0: http
	l0 := cfg.Server.Listeners[0]
	if l0.Name != "http-main" {
		t.Errorf("Listeners[0].Name = %q, want %q", l0.Name, "http-main")
	}
	if l0.Protocol != "http" {
		t.Errorf("Listeners[0].Protocol = %q, want %q", l0.Protocol, "http")
	}
	if l0.Listen != ":8080" {
		t.Errorf("Listeners[0].Listen = %q, want %q", l0.Listen, ":8080")
	}
	if len(l0.Routes) != 1 {
		t.Fatalf("Listeners[0] Routes len = %d, want 1", len(l0.Routes))
	}
	if l0.Routes[0].Match != "/api/*" {
		t.Errorf("Listeners[0].Routes[0].Match = %q, want %q", l0.Routes[0].Match, "/api/*")
	}
	if l0.Routes[0].BackendPool != "api-pool" {
		t.Errorf("Listeners[0].Routes[0].BackendPool = %q, want %q", l0.Routes[0].BackendPool, "api-pool")
	}

	// Listener 1: websocket
	l1 := cfg.Server.Listeners[1]
	if l1.Protocol != "websocket" {
		t.Errorf("Listeners[1].Protocol = %q, want %q", l1.Protocol, "websocket")
	}
	if l1.BackendPool != "ws-pool" {
		t.Errorf("Listeners[1].BackendPool = %q, want %q", l1.BackendPool, "ws-pool")
	}

	// Listener 2: grpc
	if cfg.Server.Listeners[2].Protocol != "grpc" {
		t.Errorf("Listeners[2].Protocol = %q, want %q", cfg.Server.Listeners[2].Protocol, "grpc")
	}

	// Listener 3: tcp
	if cfg.Server.Listeners[3].Protocol != "tcp" {
		t.Errorf("Listeners[3].Protocol = %q, want %q", cfg.Server.Listeners[3].Protocol, "tcp")
	}

	// Backend pools
	if len(cfg.BackendPools) != 4 {
		t.Fatalf("len(BackendPools) = %d, want 4", len(cfg.BackendPools))
	}
	p0 := cfg.BackendPools[0]
	if p0.Name != "api-pool" {
		t.Errorf("BackendPools[0].Name = %q, want %q", p0.Name, "api-pool")
	}
	if p0.Balancer != "round_robin" {
		t.Errorf("BackendPools[0].Balancer = %q, want %q", p0.Balancer, "round_robin")
	}
	if !p0.HealthCheck.Enabled {
		t.Error("BackendPools[0].HealthCheck.Enabled = false, want true")
	}
	if p0.HealthCheck.Interval != "15s" {
		t.Errorf("BackendPools[0].HealthCheck.Interval = %q, want %q", p0.HealthCheck.Interval, "15s")
	}
	if p0.HealthCheck.Timeout != "3s" {
		t.Errorf("BackendPools[0].HealthCheck.Timeout = %q, want %q", p0.HealthCheck.Timeout, "3s")
	}
	if p0.HealthCheck.Path != "/health" {
		t.Errorf("BackendPools[0].HealthCheck.Path = %q, want %q", p0.HealthCheck.Path, "/health")
	}
	if len(p0.Backends) != 2 {
		t.Fatalf("BackendPools[0] Backends len = %d, want 2", len(p0.Backends))
	}
	if p0.Backends[0].URL != "http://127.0.0.1:8001" {
		t.Errorf("BackendPools[0].Backends[0].URL = %q, want %q", p0.Backends[0].URL, "http://127.0.0.1:8001")
	}
	if p0.Backends[0].Weight != 3 {
		t.Errorf("BackendPools[0].Backends[0].Weight = %d, want 3", p0.Backends[0].Weight)
	}

	// Rate limit
	if !cfg.RateLimit.Enabled {
		t.Error("RateLimit.Enabled = false, want true")
	}
	if cfg.RateLimit.RequestsPerSecond != 100.5 {
		t.Errorf("RateLimit.RequestsPerSecond = %f, want 100.5", cfg.RateLimit.RequestsPerSecond)
	}
	if cfg.RateLimit.Burst != 300 {
		t.Errorf("RateLimit.Burst = %d, want 300", cfg.RateLimit.Burst)
	}
	if !cfg.RateLimit.PerIP {
		t.Error("RateLimit.PerIP = false, want true")
	}

	// Logging
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, "debug")
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Logging.Format = %q, want %q", cfg.Logging.Format, "text")
	}
	if cfg.Logging.Output != "stderr" {
		t.Errorf("Logging.Output = %q, want %q", cfg.Logging.Output, "stderr")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("Load() with missing file should return error, got nil")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	path := writeTempYAML(t, `
server:
  listen: ":8080"
  backends:
    - url: [invalid
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() with malformed YAML should return error, got nil")
	}
}

func TestValidateInvalidProtocol(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Listen: ":8080",
			Listeners: []ListenerConfig{
				{Name: "bad", Protocol: "ftp", Listen: ":21"},
			},
		},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with invalid protocol should return error")
	}
}

func TestValidateInvalidBalancer(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Listen: ":8080"},
		BackendPools: []BackendPoolConfig{
			{Name: "pool1", Balancer: "magic", Backends: []BackendConfig{{URL: "http://localhost:8001"}}},
		},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with invalid balancer should return error")
	}
}

func TestValidateEmptyBackendURL(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Listen: ":8080"},
		BackendPools: []BackendPoolConfig{
			{Name: "pool1", Balancer: "round_robin", Backends: []BackendConfig{{URL: ""}}},
		},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with empty backend URL should return error")
	}
}

func TestValidateInvalidDuration(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "invalid interval",
			cfg: Config{
				Server: ServerConfig{Listen: ":8080"},
				BackendPools: []BackendPoolConfig{
					{
						Name:     "pool1",
						Balancer: "round_robin",
						HealthCheck: HealthCheckConfig{
							Enabled:  true,
							Interval: "abc",
						},
						Backends: []BackendConfig{{URL: "http://localhost:8001"}},
					},
				},
				Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: true,
		},
		{
			name: "invalid timeout",
			cfg: Config{
				Server: ServerConfig{Listen: ":8080"},
				BackendPools: []BackendPoolConfig{
					{
						Name:     "pool1",
						Balancer: "round_robin",
						HealthCheck: HealthCheckConfig{
							Enabled: true,
							Timeout: "not-a-duration",
						},
						Backends: []BackendConfig{{URL: "http://localhost:8001"}},
					},
				},
				Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: true,
		},
		{
			name: "valid duration",
			cfg: Config{
				Server: ServerConfig{Listen: ":8080"},
				BackendPools: []BackendPoolConfig{
					{
						Name:     "pool1",
						Balancer: "round_robin",
						HealthCheck: HealthCheckConfig{
							Enabled:  true,
							Interval: "10s",
							Timeout:  "5s",
						},
						Backends: []BackendConfig{{URL: "http://localhost:8001"}},
					},
				},
				Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateTLSMissingCert(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "server TLS missing cert_file",
			cfg: Config{
				Server: ServerConfig{
					Listen: ":8080",
					TLS:    TLSConfig{Enabled: true, KeyFile: "key.pem"},
				},
				Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: true,
		},
		{
			name: "server TLS missing key_file",
			cfg: Config{
				Server: ServerConfig{
					Listen: ":8080",
					TLS:    TLSConfig{Enabled: true, CertFile: "cert.pem"},
				},
				Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: true,
		},
		{
			name: "listener TLS missing cert_file",
			cfg: Config{
				Server: ServerConfig{
					Listeners: []ListenerConfig{
						{
							Name:     "tls-listener",
							Protocol: "http",
							Listen:   ":8443",
							TLS:      TLSConfig{Enabled: true, KeyFile: "key.pem"},
						},
					},
				},
				Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: true,
		},
		{
			name: "TLS disabled is fine",
			cfg: Config{
				Server: ServerConfig{
					Listen: ":8080",
					TLS:    TLSConfig{Enabled: false},
				},
				Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: false,
		},
		{
			name: "TLS enabled with both files",
			cfg: Config{
				Server: ServerConfig{
					Listen: ":8443",
					TLS:    TLSConfig{Enabled: true, CertFile: "cert.pem", KeyFile: "key.pem"},
				},
				Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateInvalidLogLevel(t *testing.T) {
	cfg := &Config{
		Server:  ServerConfig{Listen: ":8080"},
		Logging: LoggingConfig{Level: "trace", Format: "json", Output: "stdout"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with invalid log level should return error")
	}
}

func TestValidateInvalidFormat(t *testing.T) {
	cfg := &Config{
		Server:  ServerConfig{Listen: ":8080"},
		Logging: LoggingConfig{Level: "info", Format: "xml", Output: "stdout"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with invalid log format should return error")
	}
}

func TestDefaultValues(t *testing.T) {
	minimalYAML := `
server:
backend_pools:
  - name: default-pool
    balancer: round_robin
    backends:
      - url: "http://localhost:8001"
`
	path := writeTempYAML(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	// Server.Listen defaults to ":8080" when empty and no listeners
	if cfg.Server.Listen != ":8080" {
		t.Errorf("Server.Listen default = %q, want %q", cfg.Server.Listen, ":8080")
	}

	// Health check defaults
	hc := cfg.BackendPools[0].HealthCheck
	if hc.Interval != "10s" {
		t.Errorf("HealthCheck.Interval default = %q, want %q", hc.Interval, "10s")
	}
	if hc.Timeout != "5s" {
		t.Errorf("HealthCheck.Timeout default = %q, want %q", hc.Timeout, "5s")
	}
	if hc.UnhealthyThreshold != 3 {
		t.Errorf("HealthCheck.UnhealthyThreshold default = %d, want 3", hc.UnhealthyThreshold)
	}
	if hc.HealthyThreshold != 2 {
		t.Errorf("HealthCheck.HealthyThreshold default = %d, want 2", hc.HealthyThreshold)
	}

	// Backend weight default
	if cfg.BackendPools[0].Backends[0].Weight != 1 {
		t.Errorf("Backend.Weight default = %d, want 1", cfg.BackendPools[0].Backends[0].Weight)
	}

	// Logging defaults
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level default = %q, want %q", cfg.Logging.Level, "info")
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Logging.Format default = %q, want %q", cfg.Logging.Format, "json")
	}
	if cfg.Logging.Output != "stdout" {
		t.Errorf("Logging.Output default = %q, want %q", cfg.Logging.Output, "stdout")
	}

	// Rate limit burst default
	if cfg.RateLimit.Burst != 200 {
		t.Errorf("RateLimit.Burst default = %d, want 200", cfg.RateLimit.Burst)
	}
}

func TestUniquePoolNames(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Listen: ":8080"},
		BackendPools: []BackendPoolConfig{
			{Name: "dup", Balancer: "round_robin", Backends: []BackendConfig{{URL: "http://a"}}},
			{Name: "dup", Balancer: "round_robin", Backends: []BackendConfig{{URL: "http://b"}}},
		},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with duplicate pool names should return error")
	}
}

func TestValidateRateLimit(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "enabled with zero RPS",
			cfg: Config{
				Server:   ServerConfig{Listen: ":8080"},
				RateLimit: RateLimitConfig{Enabled: true, RequestsPerSecond: 0, Burst: 10},
				Logging:  LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: true,
		},
		{
			name: "enabled with negative RPS",
			cfg: Config{
				Server:   ServerConfig{Listen: ":8080"},
				RateLimit: RateLimitConfig{Enabled: true, RequestsPerSecond: -5, Burst: 10},
				Logging:  LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: true,
		},
		{
			name: "enabled with zero burst",
			cfg: Config{
				Server:   ServerConfig{Listen: ":8080"},
				RateLimit: RateLimitConfig{Enabled: true, RequestsPerSecond: 10, Burst: 0},
				Logging:  LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: true,
		},
		{
			name: "enabled with valid values",
			cfg: Config{
				Server:   ServerConfig{Listen: ":8080"},
				RateLimit: RateLimitConfig{Enabled: true, RequestsPerSecond: 10, Burst: 20},
				Logging:  LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: false,
		},
		{
			name: "disabled with zero values is fine",
			cfg: Config{
				Server:   ServerConfig{Listen: ":8080"},
				RateLimit: RateLimitConfig{Enabled: false, RequestsPerSecond: 0, Burst: 0},
				Logging:  LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateServerListenEmpty(t *testing.T) {
	cfg := &Config{
		Server:  ServerConfig{Listen: ""},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() with empty Server.Listen and no listeners should return error")
	}
}

func TestValidateServerListenEmptyWithListeners(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Listen: "",
			Listeners: []ListenerConfig{
				{Name: "http", Protocol: "http", Listen: ":8080"},
			},
		},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate() with empty Server.Listen but listeners present should not error, got: %v", err)
	}
}

func TestValidateSocketProtocol(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Listeners: []ListenerConfig{
				{Name: "sock", Protocol: "socket", Listen: "/var/run/proxy.sock", BackendPool: "test"},
			},
		},
		BackendPools: []BackendPoolConfig{
			{Name: "test", Balancer: "round_robin", Backends: []BackendConfig{{URL: "unix:/tmp/test.sock"}}},
		},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("socket protocol should be valid: %v", err)
	}
}

func TestValidateUDPProtocol(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Listeners: []ListenerConfig{
				{Name: "udp", Protocol: "udp", Listen: ":8125", BackendPool: "test"},
			},
		},
		BackendPools: []BackendPoolConfig{
			{Name: "test", Balancer: "random", Backends: []BackendConfig{{URL: "udp://127.0.0.1:8126"}}},
		},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("udp protocol should be valid: %v", err)
	}
}

func TestValidateRPCProtocol(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Listeners: []ListenerConfig{
				{Name: "rpc", Protocol: "rpc", Listen: ":9000", BackendPool: "test"},
			},
		},
		BackendPools: []BackendPoolConfig{
			{Name: "test", Balancer: "least_connections", Backends: []BackendConfig{{URL: "rpc://127.0.0.1:9001"}}},
		},
		Logging: LoggingConfig{Level: "info", Format: "json", Output: "stdout"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("rpc protocol should be valid: %v", err)
	}
}
