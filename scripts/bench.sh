#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/loadbear-bench.XXXXXX")"
LOADBEAR="$TMP/loadbear"
FAKEBACKEND="$TMP/fakebackend"
LOADGEN="$TMP/loadgen"
DURATION="${DURATION:-10s}"
WARMUP="${WARMUP:-5s}"
CONCURRENCY="${CONCURRENCY:-64}"
RUNS="${RUNS:-2}"
PIDS=()

stop_processes() {
  local pid
  for pid in "${PIDS[@]}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  for pid in "${PIDS[@]}"; do
    wait "$pid" >/dev/null 2>&1 || true
  done
  PIDS=()
}

cleanup() {
  local result=$?
  trap - EXIT INT TERM
  stop_processes
  rm -rf "$TMP"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "Benchmark machine information"
echo "Date: $(date '+%Y-%m-%dT%H:%M:%S%z')"
echo "OS: $(uname -srm)"
CPU_MODEL="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || awk -F ': ' '/model name/ {print $2; exit}' /proc/cpuinfo 2>/dev/null || true)"
CPU_CORES="$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo unknown)"
echo "CPU: ${CPU_MODEL:-unknown}"
echo "Logical CPU cores: $CPU_CORES"
echo "Go: $(go version)"
echo "Duration: $DURATION; warmup: $WARMUP; concurrency: $CONCURRENCY; runs per scenario: $RUNS"
echo

echo "+ go build -o \"$LOADBEAR\" ./cmd/loadbear"
go build -o "$LOADBEAR" ./cmd/loadbear
echo "+ go build -o \"$FAKEBACKEND\" ./cmd/fakebackend"
go build -o "$FAKEBACKEND" ./cmd/fakebackend
echo "+ go build -o \"$LOADGEN\" ./cmd/loadgen"
go build -o "$LOADGEN" ./cmd/loadgen

free_port() {
  echo "+ \"$LOADGEN\" --free-port" >&2
  "$LOADGEN" --free-port
}

start_backend() {
  local port="$1"
  local name="$2"
  local log="$TMP/${name}.log"
  echo "+ \"$FAKEBACKEND\" --port \"$port\" --name \"$name\""
  "$FAKEBACKEND" --port "$port" --name "$name" >"$log" 2>&1 &
  PIDS+=("$!")
}

wait_http() {
  local url="$1"
  local attempts=0
  while [ "$attempts" -lt 60 ]; do
    if curl --silent --show-error --fail "$url" >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 0.2
  done
  echo "Timed out waiting for $url" >&2
  return 1
}

run_loadgen() {
  local url="$1"
  local run="$2"
  echo "Run $run: rate_limit.enabled=false for proxy scenarios"
  echo "+ \"$LOADGEN\" --url \"$url\" --duration \"$DURATION\" --warmup \"$WARMUP\" --concurrency \"$CONCURRENCY\" --method GET --format json"
  "$LOADGEN" --url "$url" --duration "$DURATION" --warmup "$WARMUP" --concurrency "$CONCURRENCY" --method GET --format json
}

run_scenario() {
  local label="$1"
  local backend_count="$2"
  local strategy="$3"
  local direct="$4"
  local ports=()
  local index
  local target
  local proxy_port
  local admin_port
  local config_file

  echo
  echo "=== Scenario: $label ==="
  for ((index = 1; index <= backend_count; index++)); do
    ports+=("$(free_port)")
  done
  for ((index = 0; index < backend_count; index++)); do
    start_backend "${ports[$index]}" "backend-$((index + 1))"
    wait_http "http://127.0.0.1:${ports[$index]}/health"
  done

  if [ "$direct" = "true" ]; then
    target="http://127.0.0.1:${ports[0]}/"
  else
    proxy_port="$(free_port)"
    admin_port="$(free_port)"
    config_file="$TMP/${label//[^a-zA-Z0-9]/_}.yaml"
    {
      printf 'listen: "127.0.0.1:%s"\n' "$proxy_port"
      printf 'admin_listen: "127.0.0.1:%s"\n' "$admin_port"
      printf 'strategy: "%s"\n' "$strategy"
      printf 'log_level: "error"\n'
      printf 'backends:\n'
      for ((index = 0; index < backend_count; index++)); do
        printf '  - url: "http://127.0.0.1:%s"\n    weight: 1\n' "${ports[$index]}"
      done
      printf 'rate_limit:\n  enabled: false\n'
      printf 'health:\n  interval: "1h"\n'
    } >"$config_file"
    echo "Generated config ($config_file):"
    cat "$config_file"
    echo "Proxy config uses rate_limit.enabled=false and log_level=error."
    echo "+ \"$LOADBEAR\" --config \"$config_file\""
    "$LOADBEAR" --config "$config_file" >"$TMP/loadbear-${label//[^a-zA-Z0-9]/_}.log" 2>&1 &
    PIDS+=("$!")
    for ((index = 0; index < backend_count; index++)); do
      wait_http "http://127.0.0.1:${ports[$index]}/health"
    done
    wait_http "http://127.0.0.1:${admin_port}/status"
    local status healthy_count
    status="$(curl --silent --show-error --fail "http://127.0.0.1:${admin_port}/status")"
    healthy_count="$(printf '%s' "$status" | awk -F '"healthy":true' '{print NF-1}')"
    if [ "$healthy_count" -ne "$backend_count" ]; then
      echo "Expected $backend_count healthy backends in /status, got $healthy_count: $status" >&2
      return 1
    fi
    echo "Admin /status reports $healthy_count healthy backends."
    target="http://127.0.0.1:${proxy_port}/"
  fi

  for ((index = 1; index <= RUNS; index++)); do
    run_loadgen "$target" "$index"
  done
  stop_processes
}

run_scenario "direct-one-backend" 1 "round_robin" "true"
run_scenario "proxy-one-backend" 1 "round_robin" "false"
run_scenario "proxy-three-round-robin" 3 "round_robin" "false"
run_scenario "proxy-three-smooth-weighted-round-robin" 3 "smooth_weighted_round_robin" "false"
run_scenario "proxy-three-least-connections" 3 "least_connections" "false"
run_scenario "proxy-three-ip-hash" 3 "ip_hash" "false"
