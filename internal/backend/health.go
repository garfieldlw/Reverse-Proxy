package backend

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

// HealthChecker performs periodic health checks on a pool's backends.
type HealthChecker struct {
	pool        *Pool
	cfg         config.HealthCheckConfig
	client      *http.Client
	dialTimeout time.Duration
	logger      *slog.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewHealthChecker creates a health checker for a pool.
func NewHealthChecker(pool *Pool, cfg config.HealthCheckConfig, logger *slog.Logger) *HealthChecker {
	timeout := parseDurationWithDefault(cfg.Timeout, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())

	return &HealthChecker{
		pool:        pool,
		cfg:         cfg,
		dialTimeout: timeout,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	done: make(chan struct{}),
	}
}

// Start launches the health check goroutine.
func (hc *HealthChecker) Start() {
	go hc.run()
}

// Stop cancels the health check goroutine and waits for it to finish.
func (hc *HealthChecker) Stop() {
	hc.cancel()
	select {
	case <-hc.done:
	case <-time.After(5 * time.Second):
		hc.logger.Warn("health checker stop timed out", "pool", hc.pool.Name)
	}
}

// run is the main health check loop.
func (hc *HealthChecker) run() {
	defer close(hc.done)

	interval := parseDurationWithDefault(hc.cfg.Interval, 10*time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Perform an initial check immediately.
	hc.checkAll()

	for {
		select {
		case <-hc.ctx.Done():
			return
		case <-ticker.C:
			hc.checkAll()
		}
	}
}

// checkAll checks all backends in the pool concurrently.
func (hc *HealthChecker) checkAll() {
	backends := hc.pool.GetAllBackends()

	var wg sync.WaitGroup
	for _, b := range backends {
		wg.Add(1)
		go func(backend *Backend) {
			defer wg.Done()
			hc.checkBackend(backend)
		}(b)
	}
	wg.Wait()

	// Apply thresholds after all checks complete.
	for _, b := range backends {
		hc.applyThresholds(b)
	}
}

// checkBackend performs a single health check on a backend.
func (hc *HealthChecker) checkBackend(b *Backend) {
	if hc.cfg.Path != "" {
		hc.checkHTTP(b)
		return
	}
	switch b.URL.Scheme {
	case "unix":
		hc.checkUnix(b)
	default:
		hc.checkTCP(b)
	}
}

// checkHTTP performs an HTTP health check.
func (hc *HealthChecker) checkHTTP(b *Backend) {
	checkURL := b.URL.String() + hc.cfg.Path

	req, err := http.NewRequestWithContext(hc.ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		b.RecordFailure()
		hc.logger.Debug("health check request creation failed",
			"pool", hc.pool.Name,
			"backend", b.RawURL,
			"error", err,
		)
		return
	}

	resp, err := hc.client.Do(req)
	if err != nil {
		b.RecordFailure()
		hc.logger.Debug("health check failed",
			"pool", hc.pool.Name,
			"backend", b.RawURL,
			"path", hc.cfg.Path,
			"error", err,
		)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		b.RecordSuccess()
		hc.logger.Debug("health check passed",
			"pool", hc.pool.Name,
			"backend", b.RawURL,
			"path", hc.cfg.Path,
			"status", resp.StatusCode,
		)
	} else {
		b.RecordFailure()
		hc.logger.Debug("health check failed with non-2xx status",
			"pool", hc.pool.Name,
			"backend", b.RawURL,
			"path", hc.cfg.Path,
			"status", resp.StatusCode,
		)
	}
}

// checkUnix performs a Unix socket dial health check.
func (hc *HealthChecker) checkUnix(b *Backend) {
	conn, err := net.DialTimeout("unix", b.URL.Path, hc.dialTimeout)
	if err != nil {
		b.RecordFailure()
		hc.logger.Debug("health check Unix dial failed",
			"pool", hc.pool.Name,
			"backend", b.RawURL,
			"error", err,
		)
		return
	}
	conn.Close()

	b.RecordSuccess()
	hc.logger.Debug("health check Unix dial passed",
		"pool", hc.pool.Name,
		"backend", b.RawURL,
	)
}

// checkTCP performs a TCP dial health check.
func (hc *HealthChecker) checkTCP(b *Backend) {
	dialer := net.Dialer{Timeout: hc.dialTimeout}
	conn, err := dialer.DialContext(hc.ctx, "tcp", b.URL.Host)
	if err != nil {
		b.RecordFailure()
		hc.logger.Debug("health check TCP dial failed",
			"pool", hc.pool.Name,
			"backend", b.RawURL,
			"error", err,
		)
		return
	}
	conn.Close()

	b.RecordSuccess()
	hc.logger.Debug("health check TCP dial passed",
		"pool", hc.pool.Name,
		"backend", b.RawURL,
	)
}

// applyThresholds transitions backend status based on consecutive pass/fail counts.
func (hc *HealthChecker) applyThresholds(b *Backend) {
	unhealthyThreshold := hc.cfg.UnhealthyThreshold
	if unhealthyThreshold <= 0 {
		unhealthyThreshold = 3
	}
	healthyThreshold := hc.cfg.HealthyThreshold
	if healthyThreshold <= 0 {
		healthyThreshold = 2
	}

	if b.GetConsecutiveFails() >= int64(unhealthyThreshold) && b.IsHealthy() {
		hc.pool.MarkBackendUnhealthy(b)
	}

	if b.GetConsecutivePasses() >= int64(healthyThreshold) && !b.IsHealthy() {
		hc.pool.MarkBackendHealthy(b)
	}
}

// parseDurationWithDefault parses a duration string, returning the default on failure.
func parseDurationWithDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}


