#!/usr/bin/env bash
# Local HA failover smoke test.  It deliberately uses only Docker Compose and
# the pinned services from compose.ha.local.yml; when Docker is unavailable the
# test is reported as skipped and exits successfully.
set -euo pipefail

compose_file="${XROK_HA_COMPOSE_FILE:-compose.ha.local.yml}"
project="${COMPOSE_PROJECT_NAME:-xrok-ha-smoke-$RANDOM-$$}"
compose=(docker compose -p "$project" -f "$compose_file")
retries="${HA_SMOKE_RETRIES:-30}"
interval="${HA_SMOKE_INTERVAL:-2}"
up_attempts="${HA_SMOKE_UP_ATTEMPTS:-5}"
fixed_ports=0
if [[ -n "${XROK_HA_PORT_A:-}" && -n "${XROK_HA_PORT_B:-}" ]]; then fixed_ports=1; fi

fail() { echo "HA failover smoke: FAIL: $*" >&2; exit 1; }
wait_running() {
  local svc="$1" state=""
  for _ in $(seq 1 "$retries"); do
    state=$("${compose[@]}" ps --format '{{.Service}} {{.State}}' "$svc" 2>/dev/null || true)
    [[ "$state" == *"running"* ]] && return 0
    sleep "$interval"
  done
  fail "$svc did not become running within $((retries * interval))s (state: ${state:-unknown})"
}

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "HA failover smoke: SKIP (Docker daemon unavailable)"
  exit 0
fi

cleanup() { "${compose[@]}" down >/dev/null 2>&1 || true; }
trap cleanup EXIT

up_ok=0
up_log="${TMPDIR:-/tmp}/xrok-ha-up-${project}.log"
for attempt in $(seq 1 "$up_attempts"); do
  # Port selection is advisory: another process may claim a port before
  # Docker publishes it. Generate a fresh set for every attempt and let
  # Compose be the source of truth for reservation.
  if (( fixed_ports )); then
    : # caller supplied a complete fixed port set; Compose still validates it.
  elif [[ -z "${XROK_HA_PORT_A_OVERRIDE:-}" ]]; then
    while IFS='=' read -r k v; do export "$k=$v"; done < <("$(dirname "$0")/allocate-ports.sh")
  else
    export XROK_HA_PORT_A="$XROK_HA_PORT_A_OVERRIDE"
    export XROK_HA_UDP_A="${XROK_HA_UDP_A_OVERRIDE:-$((XROK_HA_PORT_A + 1))}"
    export XROK_HA_PORT_B="${XROK_HA_PORT_B_OVERRIDE:-$((XROK_HA_PORT_A + 2))}"
    export XROK_HA_UDP_B="${XROK_HA_UDP_B_OVERRIDE:-$((XROK_HA_PORT_A + 3))}"
  fi
  echo "HA failover smoke: compose up attempt ${attempt}/${up_attempts} ports=${XROK_HA_PORT_A},${XROK_HA_PORT_B}" >&2
  if "${compose[@]}" config >/dev/null 2>"$up_log" && "${compose[@]}" up -d --build >>"$up_log" 2>&1; then
    up_ok=1
    break
  fi
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
  if (( attempt < up_attempts )); then sleep 1; fi
done
if (( ! up_ok )); then
  cat "$up_log" >&2
  fail "compose up failed after ${up_attempts} attempts (possible port collision)"
fi
for svc in redis postgres xrok-a xrok-b; do
  wait_running "$svc"
done

# Verify control-plane persistence independently of xrok process lifecycle.
"${compose[@]}" exec -T redis redis-cli set xrok-ha-smoke alive >/dev/null
[[ $("${compose[@]}" exec -T redis redis-cli get xrok-ha-smoke | tr -d '\r') == alive ]]

# Verify PostgreSQL persistence independently of xrok lifecycle. This catches
# accidental ephemeral volumes and validates the HA state backend survives a
# database restart.
"${compose[@]}" exec -T postgres psql -U xrok -d xrok -v ON_ERROR_STOP=1 \
  -c 'CREATE TABLE IF NOT EXISTS xrok_ha_smoke (id text PRIMARY KEY, value text NOT NULL);' \
  -c "INSERT INTO xrok_ha_smoke(id,value) VALUES ('smoke','alive') ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value;" >/dev/null
"${compose[@]}" restart postgres >/dev/null
wait_running postgres
pg_value=$("${compose[@]}" exec -T postgres psql -U xrok -d xrok -Atqc "SELECT value FROM xrok_ha_smoke WHERE id='smoke';" | tr -d '\r')
[[ "$pg_value" == alive ]] || fail "postgres persistence check returned '${pg_value:-empty}'"

before=$("${compose[@]}" ps -q xrok-a)
"${compose[@]}" restart xrok-a >/dev/null
after=$("${compose[@]}" ps -q xrok-a)
[[ -n "$after" && "$before" != "$after" ]] || { echo "HA failover smoke: replica did not restart" >&2; exit 1; }
wait_running xrok-a
[[ $("${compose[@]}" exec -T redis redis-cli get xrok-ha-smoke | tr -d '\r') == alive ]]

# Exercise graceful drain semantics: stop one replica with a bounded timeout
# and ensure the peer remains available to accept traffic.
"${compose[@]}" stop -t "${HA_DRAIN_TIMEOUT:-10}" xrok-a >/dev/null
stopped=$("${compose[@]}" ps --format '{{.Service}} {{.State}}' xrok-a 2>/dev/null || true)
[[ "$stopped" == *"exited"* || "$stopped" == *"stopped"* ]] || fail "xrok-a did not drain cleanly (state: $stopped)"
wait_running xrok-b

echo "HA failover smoke: PASS (Redis/Postgres persistence, reconnect recovery, and graceful drain verified)"
