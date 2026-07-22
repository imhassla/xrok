#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "$0")/.." && pwd)"
compose_file="${XROK_REFERENCE_COMPOSE_FILE:-$root_dir/compose.reference.yml}"
base_project="${COMPOSE_PROJECT_NAME:-xrok-otel-integration-$$}"

pick_ports() {
  python3 - <<'PY'
import random
# Docker is the allocator of record; compose-up collision retry below is the
# only authority. Avoid bind-then-release port probes.
print(*random.sample(range(20000, 50000), 3))
PY
}

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  echo "OTLP collector integration: SKIP (Docker daemon unavailable)"
  exit 0
fi

project=""
c=()
cleanup() {
  if [[ ${#c[@]} -gt 0 ]]; then "${c[@]}" down --remove-orphans >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

started=0
for attempt in {1..5}; do
  cleanup
  project="${base_project}-${attempt}"
  read -r candidate_grpc candidate_http candidate_health < <(pick_ports)
  export OTEL_GRPC_PORT="${OTEL_GRPC_PORT_OVERRIDE:-$candidate_grpc}"
  export OTEL_HTTP_PORT="${OTEL_HTTP_PORT_OVERRIDE:-$candidate_http}"
  export OTEL_HEALTH_PORT="${OTEL_HEALTH_PORT_OVERRIDE:-$candidate_health}"
  c=(docker compose -p "$project" -f "$compose_file")
  up_log="$(mktemp)"
  if "${c[@]}" up -d otel-collector >"$up_log" 2>&1; then
    started=1
    rm -f "$up_log"
    break
  fi
  if ! grep -Eqi 'port is already allocated|address already in use|bind:' "$up_log"; then
    cat "$up_log" >&2
    rm -f "$up_log"
    exit 1
  fi
  rm -f "$up_log"
done
if [[ "$started" != 1 ]]; then
  echo "OTLP collector integration: FAIL (published port collisions after retries)" >&2
  exit 1
fi

custom_endpoint="${XROK_OTLP_INTEGRATION_ENDPOINT:-}"
endpoint="${custom_endpoint:-http://127.0.0.1:$OTEL_HTTP_PORT}"
health_url="http://127.0.0.1:$OTEL_HEALTH_PORT/"

for _ in {1..30}; do
  if curl -fsS --max-time 2 "$health_url" >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS --max-time 2 "$health_url" >/dev/null

(cd "$root_dir" && GOCACHE="${GOCACHE:-/tmp/xrok-go-build}" GOMODCACHE="${GOMODCACHE:-/tmp/xrok-go-mod}" XROK_OTLP_INTEGRATION_ENDPOINT="$endpoint" go test ./cmd -run '^TestServerRuntimeOTLPCollectorIntegration$' -count=1)
if [[ -n "$custom_endpoint" ]]; then
  echo "OTLP collector integration: PASS (custom endpoint accepted and runtime drained)"
  exit 0
fi
for _ in {1..20}; do
  if "${c[@]}" logs otel-collector 2>&1 | grep -q '/collector-integration'; then
    echo "OTLP collector integration: PASS"
    exit 0
  fi
  sleep 1
done
echo "OTLP collector integration: FAIL (collector did not report integration span)" >&2
exit 1
