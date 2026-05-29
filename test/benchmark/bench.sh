#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# bench.sh — External HTTP load-testing script for the reverse proxy.
#
# Builds the proxy binary, starts a simple Go HTTP backend, starts the proxy,
# runs load tests with hey/wrk/ab, collects results, and cleans up.
#
# Usage:
#   ./test/benchmark/bench.sh              # defaults: -n 10000 -c 50 -d 10s
#   ./test/benchmark/bench.sh -n 50000 -c 100
#   ./test/benchmark/bench.sh -d 30s -p 19080
#
# Flags:
#   -n INT    Number of requests (hey/ab). Default: 10000
#   -c INT    Concurrency / connections. Default: 50
#   -d DUR    Duration for wrk (Go duration). Default: 10s
#   -p PORT   Proxy listen port. Default: 18080
#   -b PORT   Backend listen port. Default: 18001
#
# Requirements:
#   - Go toolchain (to build proxy and backend)
#   - At least one load-test tool: hey, wrk, or ab (ApacheBench)
#     * hey:  go install github.com/rakyll/hey@latest
#     * wrk:  https://github.com/wg/wrk#building
#     * ab:   apt install apache2-utils / brew install httpd
# ---------------------------------------------------------------------------

set -euo pipefail

# ── Detect project root from script location ──────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# ── Defaults ──────────────────────────────────────────────────────────────
NUM_REQUESTS=10000
CONCURRENCY=50
WRK_DURATION="10s"
PROXY_PORT=18080
BACKEND_PORT=18001

# ── Parse flags ───────────────────────────────────────────────────────────
while getopts "n:c:d:p:b:" opt; do
  case "$opt" in
    n) NUM_REQUESTS="$OPTARG" ;;
    c) CONCURRENCY="$OPTARG" ;;
    d) WRK_DURATION="$OPTARG" ;;
    p) PROXY_PORT="$OPTARG" ;;
    b) BACKEND_PORT="$OPTARG" ;;
    *) echo "Usage: $0 [-n requests] [-c concurrency] [-d wrk_duration] [-p proxy_port] [-b backend_port]" >&2; exit 1 ;;
  esac
done

# ── Temp file paths ───────────────────────────────────────────────────────
PROXY_BIN="/tmp/reverse-proxy-bench"
BACKEND_BIN="/tmp/bench-backend"
CONFIG_FILE="/tmp/bench-config.yaml"
PROXY_LOG="/tmp/bench-proxy.log"
BACKEND_LOG="/tmp/bench-backend.log"

# ── PIDs (set during startup, used by cleanup) ────────────────────────────
PROXY_PID=""
BACKEND_PID=""

# ── Cleanup on exit / interrupt ───────────────────────────────────────────
cleanup() {
  echo ""
  echo "── Cleanup ──────────────────────────────────────────────"
  if [[ -n "${PROXY_PID:-}" ]]; then
    kill "${PROXY_PID}" 2>/dev/null || true
    wait "${PROXY_PID}" 2>/dev/null || true
    echo "  proxy (PID ${PROXY_PID}) stopped"
  fi
  if [[ -n "${BACKEND_PID:-}" ]]; then
    kill "${BACKEND_PID}" 2>/dev/null || true
    wait "${BACKEND_PID}" 2>/dev/null || true
    echo "  backend (PID ${BACKEND_PID}) stopped"
  fi
  rm -f "${PROXY_BIN}" "${BACKEND_BIN}" "${CONFIG_FILE}" "${PROXY_LOG}" "${BACKEND_LOG}"
  echo "  temp files removed"
  echo "── Done ─────────────────────────────────────────────────"
}
trap cleanup EXIT INT TERM

# ── Detect load-test tool ─────────────────────────────────────────────────
detect_tool() {
  if command -v hey &>/dev/null; then
    echo "hey"
  elif command -v wrk &>/dev/null; then
    echo "wrk"
  elif command -v ab &>/dev/null; then
    echo "ab"
  else
    echo ""
  fi
}

TOOL=$(detect_tool)
if [[ -z "${TOOL}" ]]; then
  echo "ERROR: No load-test tool found. Install one of:"
  echo ""
  echo "  hey  — go install github.com/rakyll/hey@latest"
  echo "  wrk  — https://github.com/wg/wrk#building"
  echo "  ab   — apt install apache2-utils  /  brew install httpd"
  echo ""
  exit 1
fi
echo "── Load-test tool: ${TOOL} ────────────────────────────────────────"

# ── Build proxy binary ────────────────────────────────────────────────────
echo "── Building reverse proxy ────────────────────────────────────────"
(cd "${PROJECT_ROOT}" && go build -o "${PROXY_BIN}" ./cmd/reverse-proxy)
echo "  built: ${PROXY_BIN}"

# ── Build backend binary ──────────────────────────────────────────────────
echo "── Building benchmark backend ────────────────────────────────────"
(cd "${PROJECT_ROOT}" && go build -o "${BACKEND_BIN}" ./test/benchmark/backend.go)
echo "  built: ${BACKEND_BIN}"

# ── Write minimal proxy config ────────────────────────────────────────────
cat > "${CONFIG_FILE}" <<EOF
server:
  listeners:
    - name: "bench-http"
      protocol: "http"
      listen: ":${PROXY_PORT}"
      routes:
        - match: "/"
          backend_pool: "bench-pool"

backend_pools:
  - name: "bench-pool"
    balancer: "round_robin"
    health_check:
      enabled: false
    backends:
      - url: "http://127.0.0.1:${BACKEND_PORT}"

rate_limit:
  enabled: false

logging:
  level: "warn"
  format: "json"
EOF
echo "  config: ${CONFIG_FILE}"

# ── Start backend ─────────────────────────────────────────────────────────
echo "── Starting backend on :${BACKEND_PORT} ──────────────────────────────"
"${BACKEND_BIN}" -port "${BACKEND_PORT}" &> "${BACKEND_LOG}" &
BACKEND_PID=$!

# ── Start proxy ───────────────────────────────────────────────────────────
echo "── Starting proxy on :${PROXY_PORT} ──────────────────────────────────"
"${PROXY_BIN}" -config "${CONFIG_FILE}" &> "${PROXY_LOG}" &
PROXY_PID=$!

# ── Wait for proxy to be ready ────────────────────────────────────────────
echo "  waiting for proxy to accept connections ..."
MAX_WAIT=30
WAITED=0
until curl -sf "http://127.0.0.1:${PROXY_PORT}/" -o /dev/null 2>/dev/null; do
  WAITED=$((WAITED + 1))
  if [[ ${WAITED} -ge ${MAX_WAIT} ]]; then
    echo "ERROR: proxy did not become ready after ${MAX_WAIT}s"
    echo "--- proxy log ---"
    cat "${PROXY_LOG}" 2>/dev/null || true
    echo "--- backend log ---"
    cat "${BACKEND_LOG}" 2>/dev/null || true
    exit 1
  fi
  sleep 1
done
echo "  proxy is ready (waited ${WAITED}s)"

# ── Run load tests ────────────────────────────────────────────────────────
PROXY_URL="http://127.0.0.1:${PROXY_PORT}/"
LARGE_URL="http://127.0.0.1:${PROXY_PORT}/large"

echo ""
echo "══════════════════════════════════════════════════════════════════"
echo "  BENCHMARK: small response (\"ok\" — 2 bytes)"
echo "══════════════════════════════════════════════════════════════════"
echo ""

case "${TOOL}" in
  hey)
    hey -n "${NUM_REQUESTS}" -c "${CONCURRENCY}" "${PROXY_URL}"
    ;;
  wrk)
    wrk -t4 -c"${CONCURRENCY}" -d"${WRK_DURATION}" "${PROXY_URL}"
    ;;
  ab)
    ab -n "${NUM_REQUESTS}" -c "${CONCURRENCY}" "${PROXY_URL}"
    ;;
esac

echo ""
echo "══════════════════════════════════════════════════════════════════"
echo "  BENCHMARK: large response (100KB)"
echo "══════════════════════════════════════════════════════════════════"
echo ""

case "${TOOL}" in
  hey)
    hey -n "${NUM_REQUESTS}" -c "${CONCURRENCY}" "${LARGE_URL}"
    ;;
  wrk)
    wrk -t4 -c"${CONCURRENCY}" -d"${WRK_DURATION}" "${LARGE_URL}"
    ;;
  ab)
    ab -n "${NUM_REQUESTS}" -c "${CONCURRENCY}" "${LARGE_URL}"
    ;;
esac

# ── Summary ───────────────────────────────────────────────────────────────
echo ""
echo "══════════════════════════════════════════════════════════════════"
echo "  SUMMARY"
echo "══════════════════════════════════════════════════════════════════"
echo "  Tool:           ${TOOL}"
echo "  Proxy:          127.0.0.1:${PROXY_PORT}"
echo "  Backend:        127.0.0.1:${BACKEND_PORT}"
echo "  Requests:       ${NUM_REQUESTS}"
echo "  Concurrency:    ${CONCURRENCY}"
if [[ "${TOOL}" == "wrk" ]]; then
  echo "  Duration:       ${WRK_DURATION}"
fi
echo "  Endpoints:      / (2B), /large (100KB)"
echo "  Proxy log:      ${PROXY_LOG}"
echo "  Backend log:    ${BACKEND_LOG}"
echo "══════════════════════════════════════════════════════════════════"
echo ""
echo "Benchmark complete. Processes and temp files will be cleaned up."
