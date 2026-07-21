#!/usr/bin/env bash
set -euo pipefail
c=(docker compose -f compose.reference.yml)
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
report_dir="${XROK_ARTIFACT_DIR:-$root_dir/.artifacts/reference-lab}"
mkdir -p "$report_dir"

health() {
  local name=$1 url=$2
  local i
  for i in {1..30}; do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
      printf '%s ok %s\n' "$name" "$url"; return 0
    fi
    sleep 1
  done
  printf '%s failed %s\n' "$name" "$url" >&2
  return 1
}

smoke() {
  local out="$report_dir/smoke.txt"
  {
    echo "reference_lab=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    health prometheus http://127.0.0.1:9090/-/ready
    health grafana http://127.0.0.1:3000/api/health
    health otel http://127.0.0.1:13133/
    health toxiproxy http://127.0.0.1:8474/version
    "${c[@]}" ps
  } | tee "$out"
  echo "reference lab: healthy (report: $out)"
}
case "${1:-config}" in
 config) "${c[@]}" config >/dev/null; echo "reference compose: valid";;
 up|down|ps|logs) "${c[@]}" "$@";;
 smoke) smoke;;
 up-smoke)
   trap '"${c[@]}" down --remove-orphans >/dev/null 2>&1 || true' EXIT
   "${c[@]}" up -d
   smoke
   ;;
 nat) echo "profiles: host, degraded (toxiproxy), relay-only (coturn)";;
 *) echo "usage: $0 {config|up|up-smoke|down|ps|logs|smoke|nat}"; exit 2;;
esac
