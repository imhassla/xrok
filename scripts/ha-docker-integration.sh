#!/usr/bin/env bash
# Production-like Docker integration test for the HA state backends.
# Verifies Redis AOF and PostgreSQL volume persistence across restarts, then
# exercises replica failover and graceful draining.  It is intentionally
# self-contained and reports a machine-readable artifact for CI dashboards.
set -euo pipefail

compose_file="${XROK_HA_COMPOSE_FILE:-compose.ha.local.yml}"
artifact="${HA_INTEGRATION_ARTIFACT:-.artifacts/ha-integration.json}"
compose=(docker compose -f "$compose_file")
mkdir -p "$(dirname "$artifact")"

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  printf '{"status":"skipped","reason":"docker daemon unavailable"}\n' >"$artifact"
  echo "HA Docker integration: SKIP (Docker daemon unavailable)"
  exit 0
fi

retries="${HA_SMOKE_RETRIES:-30}"
interval="${HA_SMOKE_INTERVAL:-2}"
started_at=$(date +%s)
status=failed
cleanup() {
  "${compose[@]}" down >/dev/null 2>&1 || true
  ended_at=$(date +%s)
  printf '{"status":"%s","compose":"%s","duration_seconds":%s,"checks":["redis_aof","postgres_volume","replica_restart","graceful_drain"]}\n' \
    "$status" "$compose_file" "$((ended_at-started_at))" >"$artifact"
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

"${compose[@]}" config >/dev/null
"${compose[@]}" up -d --build >/dev/null
for svc in redis postgres xrok-a xrok-b; do wait_running "$svc"; done

# Redis AOF persistence check.
"${compose[@]}" exec -T redis redis-cli SET xrok-ha-integration alive >/dev/null
"${compose[@]}" restart redis >/dev/null
wait_running redis
[[ "$(${compose[@]} exec -T redis redis-cli GET xrok-ha-integration | tr -d '\r')" == alive ]] || fail "redis AOF value lost"

# PostgreSQL volume persistence check.
"${compose[@]}" exec -T postgres psql -U xrok -d xrok -v ON_ERROR_STOP=1 \
  -c 'CREATE TABLE IF NOT EXISTS xrok_ha_integration (id text PRIMARY KEY, value text NOT NULL);' \
  -c "INSERT INTO xrok_ha_integration(id,value) VALUES ('smoke','alive') ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value;" >/dev/null
"${compose[@]}" restart postgres >/dev/null
wait_running postgres
pg_value=$(${compose[@]} exec -T postgres psql -U xrok -d xrok -Atqc "SELECT value FROM xrok_ha_integration WHERE id='smoke';" | tr -d '\r')
[[ "$pg_value" == alive ]] || fail "postgres volume value lost"

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
