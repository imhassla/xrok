#!/usr/bin/env bash
set -euo pipefail
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
c=(docker compose -f "$root_dir/compose.reference.yml")
report_dir="${XROK_ARTIFACT_DIR:-$root_dir/.artifacts/reference-lab}"
artifact="${REFERENCE_LAB_ARTIFACT:-$report_dir/result.json}"
mkdir -p "$report_dir"
mkdir -p "$(dirname "$artifact")"

write_artifact() {
  local status="$1" reason="${2:-}" report="${3:-}"
  STATUS="$status" REASON="$reason" REPORT="$report" OUT="$artifact" python3 - <<'PY'
import json, os, time
json.dump({
  'schema_version':'xrok.lab-result/v1', 'lab':'reference',
  'status':os.environ['STATUS'],
  'created_at':time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
  'reason':os.environ.get('REASON',''), 'report':os.environ.get('REPORT',''),
  'checks':['prometheus_ready','grafana_health','otel_health','toxiproxy_api']
           if os.environ['STATUS'] == 'passed' else [],
}, open(os.environ['OUT'],'w'), indent=2)
PY
}

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
  write_artifact passed "" "$out"
  echo "reference lab: healthy (report: $out)"
}
action="${1:-config}"
if [[ "$action" == "up-smoke" ]] && { ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; }; then
  write_artifact skipped "docker daemon unavailable"
  echo "reference lab: SKIP (Docker daemon unavailable)"
  exit 0
fi
case "$action" in
 config) "${c[@]}" config >/dev/null; echo "reference compose: valid";;
 up|down|ps|logs) "${c[@]}" "$@";;
 smoke) smoke;;
 up-smoke)
   cleanup() { "${c[@]}" down --remove-orphans >/dev/null 2>&1 || true; }
   trap cleanup EXIT INT TERM
   if ! "${c[@]}" up -d; then
     write_artifact failed "compose up failed"
     exit 1
   fi
   if ! smoke; then
     write_artifact failed "one or more health checks failed" "$report_dir/smoke.txt"
     exit 1
   fi
   ;;
 nat) echo "profiles: host, degraded (toxiproxy), relay-only (coturn)";;
 *) echo "usage: $0 {config|up|up-smoke|down|ps|logs|smoke|nat}"; exit 2;;
esac
