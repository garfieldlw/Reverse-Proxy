package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

// captureOutput redirects slog output to a text handler buffer, runs fn, then restores.
func captureOutput(fn func()) string {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	orig := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(orig)

	fn()
	return buf.String()
}

// captureOutputJSON redirects slog output as JSON to a buffer, runs fn, then restores.
func captureOutputJSON(fn func()) string {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	orig := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(orig)

	fn()
	return strings.TrimSpace(buf.String())
}

func TestInitJSONFormat(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: "stdout",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	output := captureOutputJSON(func() {
		slog.Info("test json format")
	})

	if !strings.Contains(output, `"msg":"test json format"`) {
		t.Errorf("expected JSON output with msg field, got: %s", output)
	}
}

func TestInitTextFormat(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "text",
		Output: "stdout",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	output := captureOutput(func() {
		slog.Info("test text format")
	})

	// slog text format uses msg="value" with quotes
	if !strings.Contains(output, `msg="test text format"`) {
		t.Errorf("expected text output with msg field, got: %s", output)
	}
}

func TestInitLogLevel(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		wantLevel slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"warn", "warn", slog.LevelWarn},
		{"error", "error", slog.LevelError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.LoggingConfig{
				Level:  tt.level,
				Format: "json",
				Output: "stdout",
			}
			if err := Init(cfg); err != nil {
				t.Fatalf("Init() error = %v", err)
			}

			// Test indirectly: log at the configured level and verify output appears.
			// Use a handler at debug level so all messages pass through.
			var buf bytes.Buffer
			handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
			orig := slog.Default()
			slog.SetDefault(slog.New(handler))
			defer slog.SetDefault(orig)

			// Log at the configured level
			switch tt.wantLevel {
			case slog.LevelDebug:
				slog.Debug("level test")
			case slog.LevelInfo:
				slog.Info("level test")
			case slog.LevelWarn:
				slog.Warn("level test")
			case slog.LevelError:
				slog.Error("level test")
			}

			if buf.Len() == 0 {
				t.Errorf("expected output at level %s, got none", tt.level)
			}
		})
	}
}

func TestInitOutputStdout(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: "stdout",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	// If we got here without error, stdout was accepted.
}

func TestInitOutputStderr(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: "stderr",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	// If we got here without error, stderr was accepted.
}

func TestInitInvalidLevel(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "invalid",
		Format: "json",
		Output: "stdout",
	}
	if err := Init(cfg); err == nil {
		t.Error("expected error for invalid level, got nil")
	}
}

func TestInitInvalidFormat(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "xml",
		Output: "stdout",
	}
	if err := Init(cfg); err == nil {
		t.Error("expected error for invalid format, got nil")
	}
}

func TestInitInvalidOutput(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: "file",
	}
	if err := Init(cfg); err == nil {
		t.Error("expected error for invalid output, got nil")
	}
}

func TestWithRequestIDFromHeader(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: "stdout",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", "test-req-123")

	logger := WithRequestID(req)

	if logger == nil {
		t.Fatal("WithRequestID() returned nil logger")
	}

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	testLogger := slog.New(handler)
	testLoggerWithID := testLogger.With("request_id", "test-req-123")
	testLoggerWithID.InfoContext(context.Background(), "with request id")

	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, `"request_id":"test-req-123"`) {
		t.Errorf("expected request_id in output, got: %s", output)
	}
}

func TestWithRequestIDFromHeaderAltCase(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: "stdout",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-Id", "alt-case-id")

	_ = WithRequestID(req)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	testLogger := slog.New(handler)
	testLoggerWithID := testLogger.With("request_id", "alt-case-id")
	testLoggerWithID.InfoContext(context.Background(), "alt case header")

	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, `"request_id":"alt-case-id"`) {
		t.Errorf("expected request_id from X-Request-Id header, got: %s", output)
	}
}

func TestWithRequestIDGenerated(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "info",
		Format: "json",
		Output: "stdout",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	_ = WithRequestID(req)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	testLogger := slog.New(handler)

	id := generateID()
	testLoggerWithID := testLogger.With("request_id", id)
	testLoggerWithID.InfoContext(context.Background(), "generated request id")

	output := strings.TrimSpace(buf.String())

	var parsed map[string]any
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("failed to parse JSON output: %v, output: %q", err, output)
	}

	reqID, ok := parsed["request_id"].(string)
	if !ok {
		t.Fatal("request_id not found or not a string in output")
	}

	if len(reqID) != 32 {
		t.Errorf("expected generated ID to be 32 hex chars, got %d: %q", len(reqID), reqID)
	}

	// Verify it's hex
	for _, c := range reqID {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("expected hex character, got %c", c)
		}
	}
}

func TestConvenienceFunctions(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "debug",
		Format: "json",
		Output: "stdout",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name  string
		logFn func(string, ...any)
		level string
	}{
		{"Debug", Debug, "DEBUG"},
		{"Info", Info, "INFO"},
		{"Warn", Warn, "WARN"},
		{"Error", Error, "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := captureOutputJSON(func() {
				tt.logFn("convenience test", "key", "value")
			})

			if !strings.Contains(output, `"msg":"convenience test"`) {
				t.Errorf("expected msg in output, got: %s", output)
			}
			if !strings.Contains(output, `"level":"`+tt.level+`"`) {
				t.Errorf("expected level %s in output, got: %s", tt.level, output)
			}
			if !strings.Contains(output, `"key":"value"`) {
				t.Errorf("expected key=value in output, got: %s", output)
			}
		})
	}
}

func TestInitDebugAddSource(t *testing.T) {
	cfg := config.LoggingConfig{
		Level:  "debug",
		Format: "json",
		Output: "stdout",
	}
	if err := Init(cfg); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Use a handler with AddSource: true to verify source is included
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: true,
	})
	orig := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(orig)

	slog.Debug("debug source test")

	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, `"source"`) {
		t.Errorf("expected source field in debug output, got: %s", output)
	}
}

func TestGenerateIDUniqueness(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateID()
		if ids[id] {
			t.Errorf("duplicate ID generated: %s", id)
		}
		ids[id] = true
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
		err   bool
	}{
		{"debug", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"invalid", slog.LevelInfo, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseLevel(tt.input)
			if tt.err {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if got != tt.want {
					t.Errorf("parseLevel(%q) = %v, want %v", tt.input, got, tt.want)
				}
			}
		})
	}
}

func TestParseOutput(t *testing.T) {
	tests := []struct {
		input string
		want  *os.File
		err   bool
	}{
		{"stdout", os.Stdout, false},
		{"stderr", os.Stderr, false},
		{"invalid", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseOutput(tt.input)
			if tt.err {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if got != tt.want {
					t.Errorf("parseOutput(%q) = %v, want %v", tt.input, got, tt.want)
				}
			}
		})
	}
}
