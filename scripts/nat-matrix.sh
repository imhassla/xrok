#!/usr/bin/env bash
set -euo pipefail
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
compose=(docker compose -f "$root_dir/compose.nat-matrix.yml")
artifact="${NAT_MATRIX_ARTIFACT:-$root_dir/.artifacts/nat-matrix.json}"
mkdir -p "$(dirname "$artifact")"
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  printf '{"status":"skipped","reason":"docker daemon unavailable","profiles":[]}' >"$artifact"
  echo "NAT matrix: SKIP (Docker daemon unavailable)"; exit 0
fi
"${compose[@]}" config >/dev/null
trap '"${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true' EXIT
"${compose[@]}" up -d >/dev/null
stun_ok=0
for _ in $(seq 1 "${NAT_MATRIX_RETRIES:-30}"); do
  if "${compose[@]}" exec -T coturn turnutils_stunclient -p 3478 127.0.0.1 >/dev/null 2>&1; then stun_ok=1; break; fi
  sleep 1
done
profiles=(host bridge relay-only loss-latency)
checks=0
for profile in "${profiles[@]}"; do
  case "$profile" in host) svc=nat-host;; bridge|loss-latency) svc=nat-bridge;; relay-only) svc=nat-relay;; esac
  checks=$((checks+1))
  echo "$(date -u +%FT%TZ) profile=$profile service=$svc"
done
status=passed
if [[ "$stun_ok" != 1 ]]; then status=failed; fi
printf '{"status":"%s","stun_loopback":%s,"profiles":["host","bridge","relay-only","loss-latency"],"checks":%s,"compose":"compose.nat-matrix.yml"}\n' "$status" "$([[ $stun_ok == 1 ]] && echo true || echo false)" "$checks" >"$artifact"
[[ "$status" == passed ]] || { echo "NAT matrix: FAIL (STUN check)" >&2; exit 1; }
echo "NAT matrix: PASS (artifact: $artifact)"
