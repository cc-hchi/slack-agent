#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${SLACK_AGENT_ENV_FILE:-$ROOT/.env}"
FORCE="${SLACK_AGENT_RESTART_FORCE:-false}"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "[restart] missing required command: $1" >&2
    exit 1
  fi
}

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

dotenv_get() {
  local key="$1"
  local line name value

  [ -f "$ENV_FILE" ] || return 1

  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    line="$(trim "$line")"
    case "$line" in
      "" | \#*) continue ;;
    esac
    case "$line" in
      export[[:space:]]*) line="$(trim "${line#export}")" ;;
    esac
    case "$line" in
      *=*) ;;
      *) continue ;;
    esac

    name="$(trim "${line%%=*}")"
    [ "$name" = "$key" ] || continue

    value="$(trim "${line#*=}")"
    case "$value" in
      \"*\") value="${value#\"}"; value="${value%\"}" ;;
      \'*\') value="${value#\'}"; value="${value%\'}" ;;
    esac
    printf '%s' "$value"
    return 0
  done < "$ENV_FILE"

  return 1
}

export_from_dotenv_if_unset() {
  local key="$1"
  local value

  if [ -z "${!key+x}" ] && value="$(dotenv_get "$key")"; then
    export "$key=$value"
  fi
}

truthy() {
  case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
    1 | true | yes | y) return 0 ;;
    *) return 1 ;;
  esac
}

port_from_addr() {
  local addr="$1"
  printf '%s' "${addr##*:}"
}

port_from_url() {
  local url="$1"
  local host

  host="${url#*://}"
  host="${host%%/*}"
  host="${host##*@}"
  if [[ "$host" = *:* ]]; then
    printf '%s' "${host##*:}"
    return
  fi

  case "$url" in
    https://*) printf '443' ;;
    *) printf '80' ;;
  esac
}

validate_port() {
  local label="$1"
  local port="$2"

  if ! [[ "$port" =~ ^[0-9]+$ ]]; then
    echo "[restart] could not resolve $label port from configuration: $port" >&2
    exit 1
  fi
}

port_pids() {
  local port="$1"
  lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null | sort -u || true
}

pid_cwd() {
  local pid="$1"
  local output line

  output="$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null || true)"
  while IFS= read -r line; do
    case "$line" in
      n*) printf '%s' "${line#n}"; return 0 ;;
    esac
  done <<< "$output"
}

pid_is_in_project() {
  local pid="$1"
  local cwd

  cwd="$(pid_cwd "$pid")"
  case "$cwd" in
    "$ROOT" | "$ROOT"/*) return 0 ;;
    *) return 1 ;;
  esac
}

wait_for_port_clear() {
  local port="$1"
  local attempt=0

  while [ "$attempt" -lt 20 ]; do
    if [ -z "$(port_pids "$port")" ]; then
      return 0
    fi
    sleep 0.2
    attempt=$((attempt + 1))
  done

  return 1
}

stop_port() {
  local label="$1"
  local port="$2"
  local pids pid cwd targets skipped

  pids="$(port_pids "$port" | tr '\n' ' ')"
  if [ -z "${pids//[[:space:]]/}" ]; then
    echo "[restart] no $label process listening on port $port"
    return
  fi

  targets=""
  skipped=""
  for pid in $pids; do
    cwd="$(pid_cwd "$pid")"
    if truthy "$FORCE" || pid_is_in_project "$pid"; then
      targets="$targets $pid"
    else
      skipped="$skipped $pid:$cwd"
    fi
  done

  if [ -n "${skipped//[[:space:]]/}" ]; then
    echo "[restart] $label port $port is used outside this project:$skipped" >&2
    echo "[restart] set SLACK_AGENT_RESTART_FORCE=true to stop it anyway" >&2
    exit 1
  fi

  echo "[restart] stopping $label on port $port:$targets"
  kill $targets >/dev/null 2>&1 || true

  if ! wait_for_port_clear "$port"; then
    echo "[restart] force stopping $label on port $port:$targets"
    kill -9 $targets >/dev/null 2>&1 || true
    wait_for_port_clear "$port" || {
      echo "[restart] port $port is still in use" >&2
      exit 1
    }
  fi
}

require_command lsof

export_from_dotenv_if_unset SLACK_AGENT_ADDR
export_from_dotenv_if_unset SLACK_AGENT_FRONTEND_URL

BACKEND_ADDR="${SLACK_AGENT_ADDR:-127.0.0.1:8787}"
FRONTEND_URL="${SLACK_AGENT_FRONTEND_URL:-http://127.0.0.1:5173}"
BACKEND_PORT="${SLACK_AGENT_RESTART_BACKEND_PORT:-$(port_from_addr "$BACKEND_ADDR")}"
FRONTEND_PORT="${SLACK_AGENT_RESTART_FRONTEND_PORT:-$(port_from_url "$FRONTEND_URL")}"

validate_port backend "$BACKEND_PORT"
validate_port frontend "$FRONTEND_PORT"

stop_port backend "$BACKEND_PORT"
stop_port frontend "$FRONTEND_PORT"

echo "[restart] starting local dev services"
exec "$ROOT/scripts/dev.sh"
