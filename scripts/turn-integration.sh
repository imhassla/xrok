#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "$0")/.." && pwd)"
compose_file="${XROK_REFERENCE_COMPOSE_FILE:-$root_dir/compose.reference.yml}"
base_project="${COMPOSE_PROJECT_NAME:-xrok-turn-integration-$$}"
cert_dir="$(mktemp -d)"
project=""
c=()

cleanup() {
  if [[ ${#c[@]} -gt 0 ]]; then
    "${c[@]}" down --remove-orphans >/dev/null 2>&1 || true
  fi
  if [[ "$cert_dir" == /tmp/* || "$cert_dir" == /private/tmp/* || "$cert_dir" == /var/folders/* ]]; then
    rm -rf "$cert_dir"
  fi
}
trap cleanup EXIT INT TERM

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "TURN integration: SKIP (Docker daemon unavailable)"
  exit 0
fi
if ! command -v openssl >/dev/null 2>&1; then
  echo "TURN integration: FAIL (openssl unavailable)" >&2
  exit 1
fi

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj /CN=localhost -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
  -keyout "$cert_dir/server-key.pem" -out "$cert_dir/server.pem" >/dev/null 2>&1
chmod 0644 "$cert_dir/server-key.pem" "$cert_dir/server.pem"
export COTURN_CERT_DIR="$cert_dir"

pick_ports() {
  python3 - <<'PY'
import random

# Docker is the allocator of record. Avoid a racy bind-then-release probe:
# choose bounded candidates and let compose publish them atomically. The outer
# loop regenerates the complete environment after a collision.
ports = random.sample(range(20000, 29999), 3)
relay = random.randint(30000, 57959)
print(ports[0], ports[1], ports[2], relay)
PY
}

started=0
for attempt in {1..5}; do
  if [[ ${#c[@]} -gt 0 ]]; then
    "${c[@]}" down --remove-orphans >/dev/null 2>&1 || true
  fi
  read -r udp_port tcp_port tls_port relay_min < <(pick_ports)
  export COTURN_UDP_PORT="${COTURN_UDP_PORT_OVERRIDE:-$udp_port}"
  export COTURN_TCP_PORT="${COTURN_TCP_PORT_OVERRIDE:-$tcp_port}"
  export COTURN_TLS_PORT="${COTURN_TLS_PORT_OVERRIDE:-$tls_port}"
  export COTURN_RELAY_MIN="${COTURN_RELAY_MIN_OVERRIDE:-$relay_min}"
  export COTURN_RELAY_MAX="${COTURN_RELAY_MAX_OVERRIDE:-$((COTURN_RELAY_MIN + 40))}"
  project="${base_project}-${attempt}"
  c=(docker compose -p "$project" -f "$compose_file")
  up_log="$(mktemp)"
  if "${c[@]}" up -d coturn >"$up_log" 2>&1; then
    for _ in {1..10}; do
      if "${c[@]}" ps --status running --services | grep -qx coturn; then
        started=1
        break
      fi
      sleep 0.2
    done
    if [[ "$started" == 1 ]]; then
      rm -f "$up_log"
      break
    fi
    echo "TURN integration: coturn exited during startup" >&2
    cat "$up_log" >&2
    "${c[@]}" ps -a >&2 || true
    "${c[@]}" logs --no-color coturn >&2 || true
    rm -f "$up_log"
    exit 1
  fi
  if ! grep -Eqi 'port is already allocated|address already in use|bind:' "$up_log"; then
    cat "$up_log" >&2
    rm -f "$up_log"
    exit 1
  fi
  rm -f "$up_log"
done
if [[ "$started" != 1 ]]; then
  echo "TURN integration: FAIL (published port collisions after retries)" >&2
  exit 1
fi

"${c[@]}" ps --status running --services | grep -qx coturn

run_turn_test() {
  local transport="$1" port="$2"
  local -a env_args=(
    "XROK_COTURN_ADDR=127.0.0.1:$port"
    "XROK_COTURN_USERNAME=matrix"
    "XROK_COTURN_PASSWORD=matrix-pass"
    "XROK_COTURN_TRANSPORT=$transport"
    "GOCACHE=${GOCACHE:-/tmp/xrok-go-build}"
    "GOMODCACHE=${GOMODCACHE:-/tmp/xrok-go-mod}"
  )
  if [[ "$transport" == tls ]]; then
    env_args+=("XROK_COTURN_TLS_CA=$cert_dir/server.pem" "XROK_COTURN_TLS_SERVER_NAME=localhost")
  fi
  # Functional failures are never retried: a one-in-five success is a flaky
  # protocol failure, not a passing lab. Only Compose port publication retries.
  (cd "$root_dir" && env "${env_args[@]}" go test ./cmd/p2p -run '^TestTURNRelayProviderCoturnBilateralDataPathIntegration$' -count=1)
}

run_turn_test udp "$COTURN_UDP_PORT"
run_turn_test tcp "$COTURN_TCP_PORT"
run_turn_test tls "$COTURN_TLS_PORT"
echo "TURN integration: PASS (UDP, TCP, TLS, bilateral ChannelData, deallocation)"
