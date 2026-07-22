#!/usr/bin/env bash
set -euo pipefail
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
compose_file="${XROK_REFERENCE_COMPOSE_FILE:-$root_dir/compose.reference.yml}"
c=(docker compose -f "$compose_file")
report_dir="${XROK_ARTIFACT_DIR:-$root_dir/.artifacts/reference-lab}"
artifact="${REFERENCE_LAB_ARTIFACT:-$report_dir/result.json}"
base_project="${COMPOSE_PROJECT_NAME:-xrok-reference-$(date +%s)-$$}"
mkdir -p "$report_dir"
mkdir -p "$(dirname "$artifact")"

# Capture caller choices before retry attempts overwrite the exported Compose
# variables. The *_OVERRIDE form matches the other local lab runners, while
# retaining the original variable keeps existing invocations compatible.
coturn_udp_override="${COTURN_UDP_PORT_OVERRIDE:-${COTURN_UDP_PORT:-}}"
coturn_tcp_override="${COTURN_TCP_PORT_OVERRIDE:-${COTURN_TCP_PORT:-}}"
coturn_tls_override="${COTURN_TLS_PORT_OVERRIDE:-${COTURN_TLS_PORT:-}}"
coturn_relay_min_override="${COTURN_RELAY_MIN_OVERRIDE:-${COTURN_RELAY_MIN:-}}"
coturn_relay_max_override="${COTURN_RELAY_MAX_OVERRIDE:-${COTURN_RELAY_MAX:-}}"
otel_grpc_override="${OTEL_GRPC_PORT_OVERRIDE:-${OTEL_GRPC_PORT:-}}"
otel_http_override="${OTEL_HTTP_PORT_OVERRIDE:-${OTEL_HTTP_PORT:-}}"
otel_health_override="${OTEL_HEALTH_PORT_OVERRIDE:-${OTEL_HEALTH_PORT:-}}"
prometheus_override="${PROMETHEUS_PORT_OVERRIDE:-${PROMETHEUS_PORT:-}}"
grafana_override="${GRAFANA_PORT_OVERRIDE:-${GRAFANA_PORT:-}}"
toxiproxy_api_override="${TOXIPROXY_API_PORT_OVERRIDE:-${TOXIPROXY_API_PORT:-}}"
toxiproxy_http_override="${TOXIPROXY_HTTP_PORT_OVERRIDE:-${TOXIPROXY_HTTP_PORT:-}}"
toxiproxy_tls_override="${TOXIPROXY_TLS_PORT_OVERRIDE:-${TOXIPROXY_TLS_PORT:-}}"

pick_candidate_ports() {
  python3 - <<'PY'
import random

# Docker is the allocator of record. Candidates are deliberately not probed
# with bind/release, eliminating the false reservation window. A failed Docker
# publish causes the whole Compose project to be recreated with fresh values.
ports = random.sample(range(20000, 44999), 11)
relay_min = random.randint(46000, 57959)
print(*ports, relay_min)
PY
}

configure_attempt() {
  local attempt="$1"
  local udp tcp tls grpc http health prometheus grafana tox_api tox_http tox_tls relay_min
  read -r udp tcp tls grpc http health prometheus grafana tox_api tox_http tox_tls relay_min < <(pick_candidate_ports)
  export COTURN_UDP_PORT="${coturn_udp_override:-$udp}"
  export COTURN_TCP_PORT="${coturn_tcp_override:-$tcp}"
  export COTURN_TLS_PORT="${coturn_tls_override:-$tls}"
  export COTURN_RELAY_MIN="${coturn_relay_min_override:-$relay_min}"
  export COTURN_RELAY_MAX="${coturn_relay_max_override:-$((COTURN_RELAY_MIN + 40))}"
  export OTEL_GRPC_PORT="${otel_grpc_override:-$grpc}"
  export OTEL_HTTP_PORT="${otel_http_override:-$http}"
  export OTEL_HEALTH_PORT="${otel_health_override:-$health}"
  export PROMETHEUS_PORT="${prometheus_override:-$prometheus}"
  export GRAFANA_PORT="${grafana_override:-$grafana}"
  export TOXIPROXY_API_PORT="${toxiproxy_api_override:-$tox_api}"
  export TOXIPROXY_HTTP_PORT="${toxiproxy_http_override:-$tox_http}"
  export TOXIPROXY_TLS_PORT="${toxiproxy_tls_override:-$tox_tls}"
  c=(docker compose -p "${base_project}-${attempt}" -f "$compose_file")
}

is_port_collision() {
  grep -Eqi 'port is already allocated|address already in use|bind:|failed to bind' "$1"
}

ensure_coturn_certs() {
  local cert_dir="${COTURN_CERT_DIR:-$report_dir/coturn-certs}"
  if ! command -v openssl >/dev/null 2>&1; then
    echo "reference lab: openssl is required for coturn TLS" >&2
    return 1
  fi
  mkdir -p "$cert_dir"
  if [[ ! -s "$cert_dir/server.pem" || ! -s "$cert_dir/server-key.pem" ]]; then
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
      -subj /CN=localhost -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
      -keyout "$cert_dir/server-key.pem" -out "$cert_dir/server.pem" >/dev/null 2>&1
    chmod 0600 "$cert_dir/server-key.pem"
    chmod 0644 "$cert_dir/server.pem"
  fi
  export COTURN_CERT_DIR="$cert_dir"
}

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
    health prometheus "http://127.0.0.1:${PROMETHEUS_PORT:-9090}/-/ready"
    health grafana "http://127.0.0.1:${GRAFANA_PORT:-3000}/api/health"
    health otel "http://127.0.0.1:${OTEL_HEALTH_PORT:-13133}/"
    health toxiproxy "http://127.0.0.1:${TOXIPROXY_API_PORT:-8474}/version"
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
 up) ensure_coturn_certs; "${c[@]}" "$@";;
 down|ps|logs) "${c[@]}" "$@";;
 smoke) smoke;;
 up-smoke)
   if ! ensure_coturn_certs; then
     write_artifact failed "coturn certificate provisioning failed"
     exit 1
   fi
   c=()
   cleanup() {
     # up-smoke projects are unique and disposable; remove their named volumes
     # as well so retries and repeated local runs do not leak Docker resources.
     if [[ ${#c[@]} -gt 0 ]]; then "${c[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true; fi
   }
   trap cleanup EXIT INT TERM
   started=0
   max_attempts="${REFERENCE_START_ATTEMPTS:-5}"
   if [[ ! "$max_attempts" =~ ^[1-9][0-9]*$ ]]; then
     write_artifact failed "REFERENCE_START_ATTEMPTS must be a positive integer"
     exit 2
   fi
   for ((attempt=1; attempt<=max_attempts; attempt++)); do
     cleanup
     configure_attempt "$attempt"
     up_log="$report_dir/compose-up-attempt-${attempt}.log"
     if "${c[@]}" up -d --wait --wait-timeout "${REFERENCE_STARTUP_TIMEOUT:-90}" >"$up_log" 2>&1; then
       started=1
       break
     fi
     if ! is_port_collision "$up_log"; then
       cat "$up_log" >&2
       "${c[@]}" ps -a >&2 || true
       "${c[@]}" logs --no-color >&2 || true
       write_artifact failed "compose services failed to become running/healthy" "$up_log"
       exit 1
     fi
   done
   if [[ "$started" != 1 ]]; then
     write_artifact failed "published port collisions after ${max_attempts} attempts" "$up_log"
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
