package tls

import (
	"crypto/tls"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/garfieldlw/reverse-proxy/internal/config"
)

var (
	testCertPath string
	testKeyPath  string
	testdataDir  string
)

func TestMain(m *testing.M) {
	// Create temp directory for test certificates
	tmpDir, err := os.MkdirTemp("", "tls-test-")
	if err != nil {
		os.Stderr.WriteString("failed to create temp dir: " + err.Error() + "\n")
		os.Exit(1)
	}
	testdataDir = tmpDir
	testCertPath = filepath.Join(testdataDir, "cert.pem")
	testKeyPath = filepath.Join(testdataDir, "key.pem")

	// Generate self-signed certificate using openssl
	cmd := exec.Command("openssl", "req",
		"-x509",
		"-newkey", "rsa:2048",
		"-keyout", testKeyPath,
		"-out", testCertPath,
		"-days", "1",
		"-nodes",
		"-subj", "/CN=localhost",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		os.Stderr.WriteString("openssl failed: " + err.Error() + ": " + string(output) + "\n")
		os.RemoveAll(testdataDir)
		os.Exit(1)
	}

	code := m.Run()

	os.RemoveAll(testdataDir)
	os.Exit(code)
}

func TestNewTLSConfigDisabled(t *testing.T) {
	cfg := config.TLSConfig{Enabled: false}
	logger := slog.Default()

	result, err := NewTLSConfig(cfg, logger)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil tls.Config when disabled, got non-nil")
	}
}

func TestNewTLSConfigEnabled(t *testing.T) {
	cfg := config.TLSConfig{
		Enabled:  true,
		CertFile: testCertPath,
		KeyFile:  testKeyPath,
	}
	logger := slog.Default()

	result, err := NewTLSConfig(cfg, logger)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil tls.Config, got nil")
	}

	// Verify MinVersion
	if result.MinVersion != tls.VersionTLS12 {
		t.Fatalf("expected MinVersion TLS12 (0x%x), got 0x%x", tls.VersionTLS12, result.MinVersion)
	}

	// Verify Certificates
	if len(result.Certificates) != 1 {
		t.Fatalf("expected 1 certificate, got %d", len(result.Certificates))
	}

	// Verify CurvePreferences includes X25519
	foundX25519 := false
	for _, curve := range result.CurvePreferences {
		if curve == tls.X25519 {
			foundX25519 = true
			break
		}
	}
	if !foundX25519 {
		t.Fatal("expected CurvePreferences to include X25519")
	}

	// Verify CipherSuites are set
	if len(result.CipherSuites) == 0 {
		t.Fatal("expected CipherSuites to be set, got empty")
	}
}

func TestNewTLSConfigMissingCert(t *testing.T) {
	cfg := config.TLSConfig{
		Enabled:  true,
		CertFile: "/nonexistent/path/cert.pem",
		KeyFile:  testKeyPath,
	}
	logger := slog.Default()

	_, err := NewTLSConfig(cfg, logger)
	if err == nil {
		t.Fatal("expected error for missing cert file, got nil")
	}
}

func TestNewTLSConfigMissingKey(t *testing.T) {
	cfg := config.TLSConfig{
		Enabled:  true,
		CertFile: testCertPath,
		KeyFile:  "/nonexistent/path/key.pem",
	}
	logger := slog.Default()

	_, err := NewTLSConfig(cfg, logger)
	if err == nil {
		t.Fatal("expected error for missing key file, got nil")
	}
}

func TestNewTLSConfigInvalidCert(t *testing.T) {
	// Create an empty cert file
	emptyCertPath := filepath.Join(testdataDir, "empty-cert.pem")
	if err := os.WriteFile(emptyCertPath, []byte(""), 0o644); err != nil {
		t.Fatalf("failed to create empty cert file: %v", err)
	}

	cfg := config.TLSConfig{
		Enabled:  true,
		CertFile: emptyCertPath,
		KeyFile:  testKeyPath,
	}
	logger := slog.Default()

	_, err := NewTLSConfig(cfg, logger)
	if err == nil {
		t.Fatal("expected error for invalid cert file, got nil")
	}
}

func TestNewTLSConfigNilLogger(t *testing.T) {
	cfg := config.TLSConfig{
		Enabled:  true,
		CertFile: testCertPath,
		KeyFile:  testKeyPath,
	}

	result, err := NewTLSConfig(cfg, nil)
	if err != nil {
		t.Fatalf("expected no error with nil logger, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil tls.Config, got nil")
	}
}
