// Package logger provides a structured logging wrapper around Go's log/slog.
// It initializes the global slog logger based on configuration and offers
// convenience functions and request-scoped logging helpers.
package logger

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

// Init initializes the global slog logger based on LoggingConfig.
// It sets the default logger used by slog package-level functions.
func Init(cfg config.LoggingConfig) error {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return err
	}

	writer, err := parseOutput(cfg.Output)
	if err != nil {
		return err
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: level == slog.LevelDebug,
	}

	var handler slog.Handler
	switch cfg.Format {
	case "json":
		handler = slog.NewJSONHandler(writer, opts)
	case "text":
		handler = slog.NewTextHandler(writer, opts)
	default:
		return fmt.Errorf("unsupported log format: %q", cfg.Format)
	}

	slog.SetDefault(slog.New(handler))
	return nil
}

// WithRequestID returns a logger with a request_id attribute.
// It extracts the ID from X-Request-ID (or X-Request-Id) header;
// if absent, it generates a new 16-byte hex-encoded ID.
func WithRequestID(r *http.Request) *slog.Logger {
	id := r.Header.Get("X-Request-ID")
	if id == "" {
		id = r.Header.Get("X-Request-Id")
	}
	if id == "" {
		id = generateID()
	}
	return slog.Default().With("request_id", id)
}

// Debug logs at debug level using the default logger.
func Debug(msg string, args ...any) {
	slog.Debug(msg, args...)
}

// Info logs at info level using the default logger.
func Info(msg string, args ...any) {
	slog.Info(msg, args...)
}

// Warn logs at warn level using the default logger.
func Warn(msg string, args ...any) {
	slog.Warn(msg, args...)
}

// Error logs at error level using the default logger.
func Error(msg string, args ...any) {
	slog.Error(msg, args...)
}

// parseLevel converts a config level string to slog.Level.
func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unsupported log level: %q", s)
	}
}

// parseOutput converts a config output string to an io.Writer.
func parseOutput(s string) (*os.File, error) {
	switch s {
	case "stdout":
		return os.Stdout, nil
	case "stderr":
		return os.Stderr, nil
	default:
		return os.Stdout, fmt.Errorf("unsupported log output: %q", s)
	}
}

// generateID creates a new 16-byte hex-encoded random ID (32 characters).
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
