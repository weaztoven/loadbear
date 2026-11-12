#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
if command -v hey >/dev/null 2>&1; then
  echo "Running hey benchmark"
  go build -o ./bin/loadbear ./cmd/loadbear
  go build -o ./bin/fakebackend ./cmd/fakebackend
  ./bin/fakebackend --port 8081 --name backend-1 >/tmp/loadbear-backend-1.log 2>&1 &
  ./bin/fakebackend --port 8082 --name backend-2 >/tmp/loadbear-backend-2.log 2>&1 &
  ./bin/fakebackend --port 8083 --name backend-3 >/tmp/loadbear-backend-3.log 2>&1 &
  ./bin/loadbear --config ./examples/config.example.yaml >/tmp/loadbear.log 2>&1 &
  LB_PID=$!
  trap 'kill $LB_PID >/dev/null 2>&1 || true; pkill -f fakebackend >/dev/null 2>&1 || true' EXIT
  sleep 2
  hey -n 2000 -c 20 http://127.0.0.1:8080/
  exit 0
fi
if command -v wrk >/dev/null 2>&1; then
  echo "Running wrk benchmark"
  wrk -t4 -c50 -d5s http://127.0.0.1:8080/
  exit 0
fi

go test -run '^$' -bench . -benchmem ./...
