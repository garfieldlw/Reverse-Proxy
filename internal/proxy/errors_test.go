package proxy

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestPreEncodedErrorsAreValidJSON(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantKey string
		wantVal string
	}{
		{"no backends", errBytesNoBackends, "error", "no backends available"},
		{"bad gateway", errBytesBadGateway, "error", "bad gateway"},
		{"internal error", errBytesInternalError, "error", "internal server error"},
		{"no healthy", errBytesNoHealthy, "error", "no healthy backends available"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m map[string]string
			if err := json.Unmarshal(tt.data, &m); err != nil {
				t.Fatalf("invalid JSON: %v\n data: %q", err, tt.data)
			}
			if m[tt.wantKey] != tt.wantVal {
				t.Errorf("got %q, want %q", m[tt.wantKey], tt.wantVal)
			}
		})
	}
}

func TestRateLimitFmtProducesValidJSON(t *testing.T) {
	data := []byte(fmt.Sprintf(errRateLimitFmt, 5))
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("invalid JSON: %v\n data: %q", err, data)
	}
	if m["error"] != "rate limit exceeded" {
		t.Errorf("got error=%v, want 'rate limit exceeded'", m["error"])
	}
	if m["retry_after"] != float64(5) {
		t.Errorf("got retry_after=%v, want 5", m["retry_after"])
	}
}
