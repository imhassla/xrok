#!/usr/bin/env bash
set -euo pipefail
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
project="${COMPOSE_PROJECT_NAME:-xrok-nat-$(date +%s)-$$}"
compose=(docker compose -p "$project" -f "$root_dir/compose.nat-matrix.yml")
artifact="${NAT_MATRIX_ARTIFACT:-$root_dir/.artifacts/nat-matrix.json}"
mkdir -p "$(dirname "$artifact")"
write_artifact() {
  local status="$1" reason="$2" loopback="$3" relay="$4" isolated="$5" symmetric="$6" degraded="$7"
  STATUS="$status" REASON="$reason" LOOPBACK="$loopback" RELAY="$relay" ISOLATED="$isolated" SYMMETRIC="$symmetric" DEGRADED="$degraded" OUT="$artifact" python3 - <<'PY'
import json, os, time
def load(path):
  try:
    with open(path) as f: return json.load(f)
  except (OSError, ValueError): return {}
symmetric = load(os.environ['SYMMETRIC']) if os.environ['SYMMETRIC'] else {}
degraded = load(os.environ['DEGRADED']) if os.environ['DEGRADED'] else {}
checks = {
  'coturn_loopback_stun': os.environ['LOOPBACK'] == '1',
  'relay_network_stun': os.environ['RELAY'] == '1',
  'private_network_isolation': os.environ['ISOLATED'] == '1',
  'endpoint_dependent_mapping': symmetric.get('symmetric') is True,
  'packet_loss_observed': 5 <= degraded.get('loss_percent', -1) <= 80,
  'network_delay_observed': degraded.get('p50_ms', 0) >= 50,
}
json.dump({
  'schema_version':'xrok.lab-result/v1', 'lab':'nat',
  'status':os.environ['STATUS'],
  'created_at':time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
  'reason':os.environ['REASON'], 'compose':'compose.nat-matrix.yml',
  'coverage':'routed-namespaces-stun-and-netem', 'checks':checks,
  'validated_profiles':[name for name, ok in checks.items() if ok],
  'measurements': {'symmetric_nat': symmetric, 'degraded_network': degraded},
  'not_exercised':['cgnat','ipv6-nat64','real-isp-firewall'],
}, open(os.environ['OUT'],'w'), indent=2)
PY
}
symmetric_result="$(mktemp)"
degraded_result="$(mktemp)"
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  write_artifact skipped "docker daemon unavailable" 0 0 0 "" ""
  echo "NAT matrix: SKIP (Docker daemon unavailable)"; exit 0
fi
"${compose[@]}" config >/dev/null
cleanup() {
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
  rm -f "$symmetric_result" "$degraded_result"
}
trap cleanup EXIT INT TERM
if ! "${compose[@]}" up -d >/dev/null; then
  write_artifact failed "compose up failed" 0 0 0 "" ""
  exit 1
fi
stun_ok=0
relay_ok=0
isolation_ok=0
symmetric_ok=0
degraded_ok=0
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
for _ in $(seq 1 "${NAT_MATRIX_RETRIES:-30}"); do
  if "${compose[@]}" exec -T nat-symmetric natprobe symmetric 172.30.0.2:3478 172.30.0.2:3479 >"$symmetric_result" 2>/dev/null; then
    symmetric_ok=1
    break
  fi
  sleep 1
done
if "${compose[@]}" exec -T nat-degraded natprobe degraded 172.32.0.2:3478 "${NAT_MATRIX_DEGRADED_PROBES:-40}" >"$degraded_result" 2>/dev/null; then
  if RESULT="$degraded_result" python3 - <<'PY'
import json, os, sys
with open(os.environ['RESULT']) as f: r=json.load(f)
sys.exit(0 if 5 <= r.get('loss_percent', -1) <= 80 and r.get('p50_ms', 0) >= 50 else 1)
PY
  then degraded_ok=1; fi
fi
status=passed
reason=""
if [[ "$stun_ok" != 1 || "$relay_ok" != 1 || "$isolation_ok" != 1 || "$symmetric_ok" != 1 || "$degraded_ok" != 1 ]]; then
  status=failed
  reason="required STUN/NAT/netem assertion failed"
fi
write_artifact "$status" "$reason" "$stun_ok" "$relay_ok" "$isolation_ok" "$symmetric_result" "$degraded_result"
[[ "$status" == passed ]] || { echo "NAT matrix: FAIL ($reason)" >&2; exit 1; }
echo "NAT matrix: PASS (artifact: $artifact)"
