// backend.go — Simple HTTP backend server for benchmarking the reverse proxy.
//
// Endpoints:
//   /          — returns "ok" (2 bytes)
//   /large     — returns 100KB of 'x' characters
//   /<size>    — returns <size> bytes of 'x' characters (e.g. /4096 returns 4KB)
//
// Usage:
//   go run ./test/benchmark/backend.go -port 18001
//   go build -o /tmp/bench-backend ./test/benchmark/backend.go && /tmp/bench-backend -port 18001

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

func main() {
	port := flag.Int("port", 18001, "listen port")
	flag.Parse()

	addr := fmt.Sprintf(":%d", *port)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case path == "/":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("ok"))

		case path == "/large":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "102400")
			w.Write(bytesX(102400))

		default:
			// /<size> — return N bytes of 'x'
			sizeStr := strings.TrimPrefix(path, "/")
			size, err := strconv.Atoi(sizeStr)
			if err != nil || size <= 0 || size > 10*1024*1024 {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", strconv.Itoa(size))
			w.Write(bytesX(size))
		}
	})

	log.Printf("benchmark backend listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("backend server error: %v", err)
	}
}

// bytesX returns a slice of n bytes filled with 'x'.
func bytesX(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}
