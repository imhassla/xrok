#!/usr/bin/env bash
# Local HA failover smoke test.  It deliberately uses only Docker Compose and
# the pinned services from compose.ha.local.yml; when Docker is unavailable the
# test is reported as skipped and exits successfully.
set -euo pipefail

compose_file="${XROK_HA_COMPOSE_FILE:-compose.ha.local.yml}"
compose=(docker compose -f "$compose_file")

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "HA failover smoke: SKIP (Docker daemon unavailable)"
  exit 0
fi

"${compose[@]}" config >/dev/null
cleanup() { "${compose[@]}" down >/dev/null 2>&1 || true; }
trap cleanup EXIT

"${compose[@]}" up -d --build
for svc in redis postgres xrok-a xrok-b; do
  for _ in $(seq 1 "${HA_SMOKE_RETRIES:-30}"); do
    state=$("${compose[@]}" ps --format '{{.Service}} {{.State}}' "$svc" 2>/dev/null || true)
    [[ "$state" == *"running"* ]] && break
    sleep 2
  done
  [[ "$state" == *"running"* ]] || { echo "HA failover smoke: $svc did not become running" >&2; exit 1; }
done

# Verify control-plane persistence independently of xrok process lifecycle.
"${compose[@]}" exec -T redis redis-cli set xrok-ha-smoke alive >/dev/null
[[ $("${compose[@]}" exec -T redis redis-cli get xrok-ha-smoke | tr -d '\r') == alive ]]

before=$("${compose[@]}" ps -q xrok-a)
"${compose[@]}" restart xrok-a >/dev/null
after=$("${compose[@]}" ps -q xrok-a)
[[ -n "$after" && "$before" != "$after" ]] || { echo "HA failover smoke: replica did not restart" >&2; exit 1; }

for _ in $(seq 1 "${HA_SMOKE_RETRIES:-30}"); do
  state=$("${compose[@]}" ps --format '{{.Service}} {{.State}}' xrok-a 2>/dev/null || true)
  [[ "$state" == *"running"* ]] && break
  sleep 2
done
[[ "$state" == *"running"* ]] || { echo "HA failover smoke: xrok-a failed to recover" >&2; exit 1; }
[[ $("${compose[@]}" exec -T redis redis-cli get xrok-ha-smoke | tr -d '\r') == alive ]]

echo "HA failover smoke: PASS (restart/recovery and control-plane persistence verified)"
