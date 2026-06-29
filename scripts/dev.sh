#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKEND_ADDR="${SLACK_AGENT_ADDR:-127.0.0.1:8787}"
FRONTEND_URL="${SLACK_AGENT_FRONTEND_URL:-http://127.0.0.1:5173}"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "[dev] missing required command: $1" >&2
    exit 1
  fi
}

require_command go
require_command npm

PIDS=()

cleanup() {
  trap - INT TERM EXIT
  if [ "${#PIDS[@]}" -gt 0 ]; then
    kill "${PIDS[@]}" >/dev/null 2>&1 || true
    wait "${PIDS[@]}" >/dev/null 2>&1 || true
  fi
}

trap cleanup INT TERM EXIT

if [ ! -d "$ROOT/frontend/node_modules" ]; then
  echo "[dev] installing frontend dependencies"
  (cd "$ROOT/frontend" && npm install)
fi

echo "[dev] backend:  http://$BACKEND_ADDR"
(cd "$ROOT/backend" && go run ./cmd/slack-agent) &
PIDS+=("$!")

echo "[dev] frontend: $FRONTEND_URL"
(cd "$ROOT/frontend" && npm run dev) &
PIDS+=("$!")

echo "[dev] press Ctrl-C to stop both services"

STATUS=0
while true; do
  for PID in "${PIDS[@]}"; do
    if ! kill -0 "$PID" >/dev/null 2>&1; then
      wait "$PID" || STATUS=$?
      exit "$STATUS"
    fi
  done
  sleep 1
done
