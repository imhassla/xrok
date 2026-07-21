#!/usr/bin/env bash
set -euo pipefail
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
project="${COMPOSE_PROJECT_NAME:-xrok-nat-$(date +%s)-$$}"
compose=(docker compose -p "$project" -f "$root_dir/compose.nat-matrix.yml")
artifact="${NAT_MATRIX_ARTIFACT:-$root_dir/.artifacts/nat-matrix.json}"
mkdir -p "$(dirname "$artifact")"
write_artifact() {
  local status="$1" reason="$2" loopback="$3" relay="$4" isolated="$5"
  STATUS="$status" REASON="$reason" LOOPBACK="$loopback" RELAY="$relay" ISOLATED="$isolated" OUT="$artifact" python3 - <<'PY'
import json, os, time
checks = {
  'coturn_loopback_stun': os.environ['LOOPBACK'] == '1',
  'relay_network_stun': os.environ['RELAY'] == '1',
  'private_network_isolation': os.environ['ISOLATED'] == '1',
}
json.dump({
  'schema_version':'xrok.lab-result/v1', 'lab':'nat',
  'status':os.environ['STATUS'],
  'created_at':time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
  'reason':os.environ['REASON'], 'compose':'compose.nat-matrix.yml',
  'coverage':'topology-and-stun-smoke', 'checks':checks,
  'validated_profiles':[name for name, ok in checks.items() if ok],
  'not_exercised':['host-namespace-stun','packet-loss-latency','symmetric-nat'],
}, open(os.environ['OUT'],'w'), indent=2)
PY
}
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  write_artifact skipped "docker daemon unavailable" 0 0 0
  echo "NAT matrix: SKIP (Docker daemon unavailable)"; exit 0
fi
"${compose[@]}" config >/dev/null
cleanup() { "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
if ! "${compose[@]}" up -d >/dev/null; then
  write_artifact failed "compose up failed" 0 0 0
  exit 1
fi
stun_ok=0
relay_ok=0
isolation_ok=0
for _ in $(seq 1 "${NAT_MATRIX_RETRIES:-30}"); do
  if "${compose[@]}" exec -T coturn turnutils_stunclient -p 3478 127.0.0.1 >/dev/null 2>&1; then stun_ok=1; break; fi
  sleep 1
done
if [[ "$stun_ok" == 1 ]] && "${compose[@]}" exec -T nat-relay \
    turnutils_stunclient -p 3478 coturn >/dev/null 2>&1; then
  relay_ok=1
fi
# The private-only client must not be able to resolve a service attached only
# to the public network. This is an assertion, not merely a declared profile.
if ! "${compose[@]}" exec -T nat-bridge getent hosts coturn >/dev/null 2>&1; then
  isolation_ok=1
fi
status=passed
reason=""
if [[ "$stun_ok" != 1 || "$relay_ok" != 1 || "$isolation_ok" != 1 ]]; then
  status=failed
  reason="required STUN/topology assertion failed"
fi
write_artifact "$status" "$reason" "$stun_ok" "$relay_ok" "$isolation_ok"
[[ "$status" == passed ]] || { echo "NAT matrix: FAIL ($reason)" >&2; exit 1; }
echo "NAT matrix: PASS (artifact: $artifact)"
