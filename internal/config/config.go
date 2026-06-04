package config

import (
	"fmt"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	Server       ServerConfig       `yaml:"server"`
	BackendPools []BackendPoolConfig `yaml:"backend_pools"`
	RateLimit    RateLimitConfig    `yaml:"rate_limit"`
	Logging      LoggingConfig      `yaml:"logging"`
}

// TransportConfig defines connection pool and dial parameters for proxies.
type TransportConfig struct {
	MaxIdleConns        int    `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost int    `yaml:"max_idle_conns_per_host"`
	IdleConnTimeout     string `yaml:"idle_conn_timeout"`
	DialTimeout         string `yaml:"dial_timeout"`
}

// ServerConfig defines the server-level settings.
type ServerConfig struct {
	Listen    string          `yaml:"listen"`
	TLS       TLSConfig       `yaml:"tls"`
	Listeners []ListenerConfig `yaml:"listeners"`
	Transport TransportConfig `yaml:"transport"`
}

// ListenerConfig defines a single listener (protocol-specific).
type ListenerConfig struct {
	Name        string        `yaml:"name"`
	Protocol string `yaml:"protocol"` // http, websocket, tcp, grpc, socket, udp, rpc
	Listen      string        `yaml:"listen"`
	TLS         TLSConfig     `yaml:"tls"`
	Routes      []RouteConfig `yaml:"routes"`
	BackendPool string        `yaml:"backend_pool"` // for tcp/grpc listeners
}

// RouteConfig maps a match pattern to a backend pool.
type RouteConfig struct {
	Match       string `yaml:"match"`
	BackendPool string `yaml:"backend_pool"`
}

// BackendPoolConfig defines a pool of backends with a balancing strategy.
type BackendPoolConfig struct {
	Name        string            `yaml:"name"`
	Balancer    string            `yaml:"balancer"` // round_robin, weighted_round_robin, least_connections, random
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Backends    []BackendConfig   `yaml:"backends"`
}

// BackendConfig defines a single backend target.
type BackendConfig struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight"`
}

// HealthCheckConfig defines health check parameters.
type HealthCheckConfig struct {
	Enabled            bool   `yaml:"enabled"`
	Interval           string `yaml:"interval"`
	Timeout            string `yaml:"timeout"`
	Path               string `yaml:"path"`
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"`
	HealthyThreshold   int    `yaml:"healthy_threshold"`
}

// RateLimitConfig defines rate limiting parameters.
type RateLimitConfig struct {
	Enabled          bool    `yaml:"enabled"`
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst            int     `yaml:"burst"`
	PerIP            bool    `yaml:"per_ip"`
}

// TLSConfig defines TLS settings.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// LoggingConfig defines logging parameters.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // json, text
	Output string `yaml:"output"` // stdout, stderr
}

// Load reads a YAML file from path, parses it into Config, applies defaults,
// validates, and returns the result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config YAML: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyDefaults sets default values for fields that are zero-valued.
func (c *Config) applyDefaults() {
	if c.Server.Listen == "" && len(c.Server.Listeners) == 0 {
		c.Server.Listen = ":8080"
	}

	for i := range c.BackendPools {
		pool := &c.BackendPools[i]
		if pool.HealthCheck.Interval == "" {
			pool.HealthCheck.Interval = "10s"
		}
		if pool.HealthCheck.Timeout == "" {
			pool.HealthCheck.Timeout = "5s"
		}
		if pool.HealthCheck.UnhealthyThreshold == 0 {
			pool.HealthCheck.UnhealthyThreshold = 3
		}
		if pool.HealthCheck.HealthyThreshold == 0 {
			pool.HealthCheck.HealthyThreshold = 2
		}
		for j := range pool.Backends {
			if pool.Backends[j].Weight == 0 {
				pool.Backends[j].Weight = 1
			}
		}
	}

	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Logging.Output == "" {
		c.Logging.Output = "stdout"
	}

	if c.RateLimit.Burst == 0 {
		c.RateLimit.Burst = 200
	}

	if c.Server.Transport.MaxIdleConns == 0 {
		c.Server.Transport.MaxIdleConns = 512
	}
	if c.Server.Transport.MaxIdleConnsPerHost == 0 {
		c.Server.Transport.MaxIdleConnsPerHost = 64
	}
	if c.Server.Transport.IdleConnTimeout == "" {
		c.Server.Transport.IdleConnTimeout = "90s"
	}
	if c.Server.Transport.DialTimeout == "" {
		c.Server.Transport.DialTimeout = "10s"
	}
}

// Validate checks the configuration for errors and returns the first one found.
func (c *Config) Validate() error {
	validProtocols := []string{"http", "websocket", "tcp", "grpc", "socket", "udp", "rpc"}
	validBalancers := []string{"round_robin", "weighted_round_robin", "least_connections", "random"}
	validLogLevels := []string{"debug", "info", "warn", "error"}
	validLogFormats := []string{"json", "text"}

	// Server.Listen must not be empty if no listeners configured
	if c.Server.Listen == "" && len(c.Server.Listeners) == 0 {
		return fmt.Errorf("server.listen must not be empty when no listeners are configured")
	}

	// Validate listeners
	for i, l := range c.Server.Listeners {
		if !slices.Contains(validProtocols, l.Protocol) {
			return fmt.Errorf("listeners[%d].protocol: %q is not valid, must be one of: http, websocket, tcp, grpc, socket, udp, rpc", i, l.Protocol)
		}
		if l.Listen == "" {
			return fmt.Errorf("listeners[%d].listen must not be empty", i)
		}
		if err := l.TLS.validate(); err != nil {
			return fmt.Errorf("listeners[%d].tls: %w", i, err)
		}
		// Non-HTTP protocols require backend_pool
		if l.Protocol != "http" && l.Protocol != "websocket" && l.BackendPool == "" && len(l.Routes) == 0 {
			return fmt.Errorf("listeners[%d]: %s protocol requires backend_pool or routes", i, l.Protocol)
		}
	}

	// Validate server TLS
	if err := c.Server.TLS.validate(); err != nil {
		return fmt.Errorf("server.tls: %w", err)
	}

	// Validate backend pools
	poolNames := make(map[string]bool)
	for i, pool := range c.BackendPools {
		if poolNames[pool.Name] {
			return fmt.Errorf("backend_pools[%d].name: %q is not unique, duplicate pool name", i, pool.Name)
		}
		poolNames[pool.Name] = true

		if !slices.Contains(validBalancers, pool.Balancer) {
			return fmt.Errorf("backend_pools[%d].balancer: %q is not valid, must be one of: round_robin, weighted_round_robin, least_connections, random", i, pool.Balancer)
		}

		// Validate backends
		for j, b := range pool.Backends {
			if b.URL == "" {
				return fmt.Errorf("backend_pools[%d].backends[%d].url must not be empty", i, j)
			}
		}

		// Validate health check
		if pool.HealthCheck.Enabled {
			if pool.HealthCheck.Interval != "" {
				if _, err := time.ParseDuration(pool.HealthCheck.Interval); err != nil {
					return fmt.Errorf("backend_pools[%d].health_check.interval: %q is not a valid duration: %w", i, pool.HealthCheck.Interval, err)
				}
			}
			if pool.HealthCheck.Timeout != "" {
				if _, err := time.ParseDuration(pool.HealthCheck.Timeout); err != nil {
					return fmt.Errorf("backend_pools[%d].health_check.timeout: %q is not a valid duration: %w", i, pool.HealthCheck.Timeout, err)
				}
			}
		}
	}

	// Validate rate limit
	if c.RateLimit.Enabled {
		if c.RateLimit.RequestsPerSecond <= 0 {
			return fmt.Errorf("rate_limit.requests_per_second must be > 0 when rate limiting is enabled")
		}
		if c.RateLimit.Burst < 1 {
			return fmt.Errorf("rate_limit.burst must be >= 1 when rate limiting is enabled")
		}
	}

	// Validate logging
	if !slices.Contains(validLogLevels, c.Logging.Level) {
		return fmt.Errorf("logging.level: %q is not valid, must be one of: debug, info, warn, error", c.Logging.Level)
	}
	if !slices.Contains(validLogFormats, c.Logging.Format) {
		return fmt.Errorf("logging.format: %q is not valid, must be one of: json, text", c.Logging.Format)
	}

	if c.Server.Transport.IdleConnTimeout != "" {
		if _, err := time.ParseDuration(c.Server.Transport.IdleConnTimeout); err != nil {
			return fmt.Errorf("server.transport.idle_conn_timeout: %q is not a valid duration: %w", c.Server.Transport.IdleConnTimeout, err)
		}
	}
	if c.Server.Transport.DialTimeout != "" {
		if _, err := time.ParseDuration(c.Server.Transport.DialTimeout); err != nil {
			return fmt.Errorf("server.transport.dial_timeout: %q is not a valid duration: %w", c.Server.Transport.DialTimeout, err)
		}
	}

	return nil
}

// validate checks TLS configuration.
func (t *TLSConfig) validate() error {
	if t.Enabled {
		if t.CertFile == "" {
			return fmt.Errorf("cert_file must not be empty when TLS is enabled")
		}
		if t.KeyFile == "" {
			return fmt.Errorf("key_file must not be empty when TLS is enabled")
		}
	}
	return nil
}
