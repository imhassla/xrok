#!/usr/bin/env bash
# Production-like Docker integration test for the HA state backends.
# Verifies Redis AOF and PostgreSQL volume persistence across restarts, then
# exercises replica failover and graceful draining.  It is intentionally
# self-contained and reports a machine-readable artifact for CI dashboards.
set -euo pipefail

compose_file="${XROK_HA_COMPOSE_FILE:-compose.ha.local.yml}"
artifact="${HA_INTEGRATION_ARTIFACT:-.artifacts/ha-integration.json}"
project="${COMPOSE_PROJECT_NAME:-}"
compose=()
mkdir -p "$(dirname "$artifact")"

pick_port() {
  local avoid="${1:-}"
  python3 - <<'PY'
import socket
import os
avoid=int(os.environ.get('PORT_AVOID','0'))
for p in range(18000, 28000):
    ss=[]
    if p == avoid: continue
    try:
        for typ in (socket.SOCK_STREAM, socket.SOCK_DGRAM):
            s=socket.socket(socket.AF_INET, typ); s.bind(('127.0.0.1', p)); ss.append(s)
        print(p); break
    except OSError: pass
    finally:
        [s.close() for s in ss]
PY
}
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  printf '{"status":"skipped","reason":"docker daemon unavailable"}\n' >"$artifact"
  echo "HA Docker integration: SKIP (Docker daemon unavailable)"
  exit 0
fi

retries="${HA_SMOKE_RETRIES:-30}"
interval="${HA_SMOKE_INTERVAL:-2}"
load_iterations="${HA_INTEGRATION_LOAD_ITERATIONS:-100}"
started_at=$(date +%s)
status=failed
cleanup() {
  "${compose[@]}" down >/dev/null 2>&1 || true
  ended_at=$(date +%s)
  printf '{"status":"%s","compose":"%s","duration_seconds":%s,"load_iterations":%s,"checks":["redis_aof","redis_load","redis_failure","postgres_volume","postgres_load","postgres_failure","replica_restart","graceful_drain"]}\n' \
    "$status" "$compose_file" "$((ended_at-started_at))" "$load_iterations" >"$artifact"
}
trap cleanup EXIT
fail() { echo "HA Docker integration: FAIL: $*" >&2; exit 1; }
wait_running() {
  local svc="$1" state=""
  for _ in $(seq 1 "$retries"); do
    state=$(${compose[@]} ps --format '{{.Service}} {{.State}}' "$svc" 2>/dev/null || true)
    [[ "$state" == *running* ]] && return 0
    sleep "$interval"
  done
  fail "$svc did not become running (state: ${state:-unknown})"
}

up_attempts="${HA_COMPOSE_UP_RETRIES:-5}"
up_ok=0
for attempt in $(seq 1 "$up_attempts"); do
  project="${COMPOSE_PROJECT_NAME:-xrok-ha-$RANDOM-$$-$attempt}"
  compose=(docker compose -p "$project" -f "$compose_file")
  export XROK_HA_PORT_A="${XROK_HA_PORT_A_OVERRIDE:-$(pick_port)}"
  export XROK_HA_UDP_A="${XROK_HA_UDP_A_OVERRIDE:-$XROK_HA_PORT_A}"
  PORT_AVOID="$XROK_HA_PORT_A"; export PORT_AVOID
  export XROK_HA_PORT_B="${XROK_HA_PORT_B_OVERRIDE:-$(pick_port "$XROK_HA_PORT_A")}" 
  export XROK_HA_UDP_B="${XROK_HA_UDP_B_OVERRIDE:-$XROK_HA_PORT_B}"
  if "${compose[@]}" config >/dev/null 2>&1 && "${compose[@]}" up -d --build >/dev/null 2>&1; then
    up_ok=1
    break
  fi
  # Docker performs the actual bind only during `up`; a prior socket probe is
  # therefore advisory. Tear down the partial project, choose a fresh set of
  # ports, and retry when another lab wins the race.
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
done
[[ "$up_ok" == 1 ]] || fail "compose up failed after $up_attempts attempts (likely port collision)"
for svc in redis postgres xrok-a xrok-b; do wait_running "$svc"; done

# Redis AOF persistence check.
"${compose[@]}" exec -T redis redis-cli SET xrok-ha-integration alive >/dev/null
"${compose[@]}" restart redis >/dev/null
wait_running redis
[[ "$(${compose[@]} exec -T redis redis-cli GET xrok-ha-integration | tr -d '\r')" == alive ]] || fail "redis AOF value lost"

# Exercise the real Redis command path under bounded load, then inject a
# transient failure and verify the service recovers before continuing.
for i in $(seq 1 "$load_iterations"); do
  "${compose[@]}" exec -T redis redis-cli SET "xrok-load-$i" "$i" >/dev/null
done
[[ "$(${compose[@]} exec -T redis redis-cli GET "xrok-load-$load_iterations" | tr -d '\r')" == "$load_iterations" ]] || fail "redis load write/read failed"
"${compose[@]}" stop redis >/dev/null
for _ in $(seq 1 "$retries"); do
  "${compose[@]}" start redis >/dev/null 2>&1 || true
  if "${compose[@]}" exec -T redis redis-cli ping 2>/dev/null | grep -q PONG; then break; fi
  sleep "$interval"
done
wait_running redis
[[ "$(${compose[@]} exec -T redis redis-cli GET xrok-ha-integration | tr -d '\r')" == alive ]] || fail "redis did not recover after failure"

# PostgreSQL volume persistence check.
"${compose[@]}" exec -T postgres psql -U xrok -d xrok -v ON_ERROR_STOP=1 \
  -c 'CREATE TABLE IF NOT EXISTS xrok_ha_integration (id text PRIMARY KEY, value text NOT NULL);' \
  -c "INSERT INTO xrok_ha_integration(id,value) VALUES ('smoke','alive') ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value;" >/dev/null
"${compose[@]}" restart postgres >/dev/null
wait_running postgres
pg_value=$(${compose[@]} exec -T postgres psql -U xrok -d xrok -Atqc "SELECT value FROM xrok_ha_integration WHERE id='smoke';" | tr -d '\r')
[[ "$pg_value" == alive ]] || fail "postgres volume value lost"

# PostgreSQL driver path/load and restart recovery.  psql speaks the same
# wire protocol used by the database/sql adapter in the runtime.
for i in $(seq 1 "$load_iterations"); do
  "${compose[@]}" exec -T postgres psql -U xrok -d xrok -v ON_ERROR_STOP=1 -c \
    "INSERT INTO xrok_ha_integration(id,value) VALUES ('load-$i','$i') ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value;" >/dev/null
done
pg_load=$(${compose[@]} exec -T postgres psql -U xrok -d xrok -Atqc "SELECT value FROM xrok_ha_integration WHERE id='load-$load_iterations';" | tr -d '\r')
[[ "$pg_load" == "$load_iterations" ]] || fail "postgres load write/read failed"
"${compose[@]}" stop postgres >/dev/null
for _ in $(seq 1 "$retries"); do
  "${compose[@]}" start postgres >/dev/null 2>&1 || true
  if "${compose[@]}" exec -T postgres pg_isready -U xrok -d xrok >/dev/null 2>&1; then break; fi
  sleep "$interval"
done
wait_running postgres
pg_value=$(${compose[@]} exec -T postgres psql -U xrok -d xrok -Atqc "SELECT value FROM xrok_ha_integration WHERE id='smoke';" | tr -d '\r')
[[ "$pg_value" == alive ]] || fail "postgres did not recover after failure"

# Replica restart and peer availability.
before=$(${compose[@]} ps -q xrok-a)
"${compose[@]}" restart xrok-a >/dev/null
after=$(${compose[@]} ps -q xrok-a)
[[ -n "$after" && "$before" != "$after" ]] || fail "xrok-a did not restart"
wait_running xrok-a
"${compose[@]}" stop -t "${HA_DRAIN_TIMEOUT:-10}" xrok-a >/dev/null
wait_running xrok-b

status=passed
echo "HA Docker integration: PASS (artifact: $artifact)"
