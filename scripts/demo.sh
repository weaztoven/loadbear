#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

go build -o ./bin/loadbear ./cmd/loadbear

go build -o ./bin/fakebackend ./cmd/fakebackend

PORTS=(8081 8082 8083)
for i in "${!PORTS[@]}"; do
  PORT="${PORTS[$i]}"
  ./bin/fakebackend --port "$PORT" --name "backend-$((i + 1))" > /tmp/loadbear-backend-$PORT.log 2>&1 &
  echo "started backend-$((i+1)) on $PORT"
done

./bin/loadbear --config ./examples/config.example.yaml &
LB_PID=$!
trap 'kill $LB_PID >/dev/null 2>&1 || true; pkill -f fakebackend >/dev/null 2>&1 || true' EXIT
sleep 2
curl -sSf http://127.0.0.1:8080/ >/dev/null
curl -sSf http://127.0.0.1:9090/status >/dev/null
printf '\nDemo traffic sent successfully.\n'
wait "$LB_PID"
