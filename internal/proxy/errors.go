package proxy

// Pre-encoded error response bytes to avoid json.NewEncoder allocations on error paths.
var (
	errBytesNoBackends  = []byte("{\"error\":\"no backends available\"}\n")
	errBytesBadGateway  = []byte("{\"error\":\"bad gateway\"}\n")
	errBytesInternalError = []byte("{\"error\":\"internal server error\"}\n")
	errBytesNoHealthy   = []byte("{\"error\":\"no healthy backends available\"}\n")
)

// errRateLimitFmt is a format string for rate limit 429 responses.
// Usage: fmt.Sprintf(errRateLimitFmt, retryAfterSec)
const errRateLimitFmt = "{\"error\":\"rate limit exceeded\",\"retry_after\":%d}\n"
