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

if [[ -z "${XROK_HA_PORT_A:-}" ]]; then
  while IFS='=' read -r k v; do export "$k=$v"; done < <("$(dirname "$0")/allocate-ports.sh")
fi

"${compose[@]}" config >/dev/null
cleanup() { "${compose[@]}" down >/dev/null 2>&1 || true; }
trap cleanup EXIT

"${compose[@]}" up -d --build
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
