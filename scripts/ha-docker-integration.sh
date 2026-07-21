#!/usr/bin/env bash
# Production-like Docker integration test for the HA state backends.
# Verifies Redis AOF and PostgreSQL volume persistence across restarts, then
# exercises replica failover and graceful draining.  It is intentionally
# self-contained and reports a machine-readable artifact for CI dashboards.
set -euo pipefail

root_dir="$(cd "$(dirname "$0")/.." && pwd)"
compose_file="${XROK_HA_COMPOSE_FILE:-$root_dir/compose.ha.local.yml}"
if [[ "$compose_file" != /* ]]; then compose_file="$root_dir/$compose_file"; fi
artifact="${HA_INTEGRATION_ARTIFACT:-$root_dir/.artifacts/ha-integration.json}"
cert_dir="${XROK_CERT_DIR:-$root_dir/local-certs}"
if [[ ! -s "$cert_dir/server.pem" || ! -s "$cert_dir/server-key.pem" ]]; then
  if command -v openssl >/dev/null 2>&1; then
    mkdir -p "$cert_dir"
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=localhost' \
      -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
      -keyout "$cert_dir/server-key.pem" -out "$cert_dir/server.pem" >/dev/null 2>&1
  fi
fi
log_dir="${HA_INTEGRATION_LOG_DIR:-$(dirname "$artifact")/ha-integration-attempts}"
project="${COMPOSE_PROJECT_NAME:-}"
compose=()
mkdir -p "$(dirname "$artifact")"
mkdir -p "$log_dir"

pick_port() {
  local avoid="${1:-}"
  python3 - <<'PY'
import socket
import os
avoid=int(os.environ.get('PORT_AVOID','0'))
for p in range(18000, 28000):
    ss=[]
    if p == avoid: continue
    try:
        for typ in (socket.SOCK_STREAM, socket.SOCK_DGRAM):
            s=socket.socket(socket.AF_INET, typ); s.bind(('127.0.0.1', p)); ss.append(s)
        print(p); break
    except OSError: pass
    finally:
        [s.close() for s in ss]
PY
}
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  OUT="$artifact" python3 - <<'PY'
import json, os, time
json.dump({'schema_version':'xrok.lab-result/v1','lab':'ha','status':'skipped',
 'created_at':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),
 'reason':'docker daemon unavailable','attempts':0,'collision_count':0,
 'selected_ports':{'tcp_a':'','udp_a':'','tcp_b':'','udp_b':''},
 'compose_up_duration_ms':0,'service_health':{},'checks':[]},
 open(os.environ['OUT'],'w'), indent=2)
PY
  echo "HA Docker integration: SKIP (Docker daemon unavailable)"
  exit 0
fi

retries="${HA_SMOKE_RETRIES:-30}"
interval="${HA_SMOKE_INTERVAL:-2}"
load_iterations="${HA_INTEGRATION_LOAD_ITERATIONS:-100}"
started_at=$(date +%s)
now_ms() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import time; print(int(time.time()*1000))'
  else
    echo "$(date +%s)000"
  fi
}
started_epoch_ms=$(date +%s%3N 2>/dev/null || date +%s000)
status=failed
attempts=0
collision_count=0
selected_tcp_a=""
selected_udp_a=""
selected_tcp_b=""
selected_udp_b=""
up_duration_ms=0
health_timings='{}'
diagnostics_file="${HA_INTEGRATION_DIAGNOSTICS:-${artifact%.json}.diagnostics.log}"
mkdir -p "$(dirname "$diagnostics_file")"
failure_class=""
last_error_log=""
current_attempt_dir=""
cleanup() {
  # Keep the artifact valid JSON even when a failure occurs before Compose is
  # initialized. Health details are captured from the final project state.
  health="{}"
  if ((${#compose[@]})); then
    health=$("${compose[@]}" ps --format '{{.Service}}={{.State}}' 2>/dev/null | python3 -c 'import json,sys; print(json.dumps(dict(x.split("=",1) for x in sys.stdin.read().splitlines() if "=" in x)))' 2>/dev/null || echo '{}')
  fi
  "${compose[@]}" down >/dev/null 2>&1 || true
  ended_at=$(date +%s)
  ARTIFACT="$artifact" STATUS="$status" COMPOSE_FILE="$compose_file" DURATION="$((ended_at-started_at))" LOAD="$load_iterations" ATTEMPTS="$attempts" COLLISIONS="$collision_count" TCP_A="$selected_tcp_a" UDP_A="$selected_udp_a" TCP_B="$selected_tcp_b" UDP_B="$selected_udp_b" UP_MS="$up_duration_ms" HEALTH="$health" HEALTH_TIMINGS="$health_timings" FAILURE_CLASS="$failure_class" LAST_ERROR_LOG="$last_error_log" LOG_DIR="$log_dir" DIAGNOSTICS="$diagnostics_file" ASSERT_HTTP="${HA_ASSERT_HTTP:-0}" ASSERT_METRICS="${HA_ASSERT_METRICS:-0}" python3 - <<'PY'
import json, os, time
out = {
  "status": os.environ["STATUS"], "compose": os.environ["COMPOSE_FILE"],
  "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
  "duration_seconds": int(os.environ["DURATION"]), "load_iterations": int(os.environ["LOAD"]),
  "attempts": int(os.environ["ATTEMPTS"]), "collision_count": int(os.environ["COLLISIONS"]),
  "selected_ports": {"tcp_a": os.environ["TCP_A"], "udp_a": os.environ["UDP_A"], "tcp_b": os.environ["TCP_B"], "udp_b": os.environ["UDP_B"]},
  "compose_up_duration_ms": int(os.environ["UP_MS"]),
  "health_check_timings_ms": json.loads(os.environ.get("HEALTH_TIMINGS", "{}")),
  "failure_class": os.environ.get("FAILURE_CLASS", ""),
  "last_error_log": os.environ.get("LAST_ERROR_LOG", ""),
  "attempt_log_dir": os.environ.get("LOG_DIR", ""),
  "diagnostics": os.environ.get("DIAGNOSTICS", ""),
  "service_health": json.loads(os.environ.get("HEALTH", "{}")),
  "schema_version": "xrok.lab-result/v1", "lab": "ha",
}
checks = ["redis_aof","redis_load","redis_failure","postgres_volume",
          "postgres_sql_load","postgres_failure","replica_restart","graceful_drain"]
if os.environ["STATUS"] == "passed" and os.environ.get("ASSERT_HTTP") == "1":
  checks += ["xrok_health","xrok_ready"]
if os.environ["STATUS"] == "passed" and os.environ.get("ASSERT_METRICS") == "1":
  checks += ["xrok_metrics"]
out["checks"] = checks if os.environ["STATUS"] == "passed" else []
with open(os.environ["ARTIFACT"], "w") as f: json.dump(out, f, sort_keys=True); f.write("\n")
PY
}
trap cleanup EXIT
fail() { echo "HA Docker integration: FAIL: $*" >&2; exit 1; }
wait_running() {
  local svc="$1" state=""
  local check_started check_finished
  check_started=$(now_ms)
  for _ in $(seq 1 "$retries"); do
    state=$(${compose[@]} ps --format '{{.Service}} {{.State}}' "$svc" 2>/dev/null || true)
    if [[ "$state" == *running* || "$state" == *Up* ]]; then
      check_finished=$(now_ms)
      health_timings=$(SVC="$svc" ELAPSED="$((check_finished-check_started))" PREV="$health_timings" python3 -c 'import json,os; d=json.loads(os.environ["PREV"]); d[os.environ["SVC"]]=int(os.environ["ELAPSED"]); print(json.dumps(d,separators=(",",":")))')
      return 0
    fi
    sleep "$interval"
  done
  failure_class="health"
  if [[ -n "$current_attempt_dir" ]]; then
    printf 'service=%s state=%s\n' "$svc" "${state:-unknown}" >"$current_attempt_dir/health.err"
    "${compose[@]}" ps >"$current_attempt_dir/health.ps" 2>&1 || true
    "${compose[@]}" logs --no-color "$svc" >"$current_attempt_dir/health.log" 2>&1 || true
    last_error_log="$current_attempt_dir/health.err"
  fi
  fail "$svc did not become running (state: ${state:-unknown})"
}

check_http_endpoint() {
  local name="$1" port="$2" path="$3" expected="$4"
  local url="https://127.0.0.1:${port}${path}" body="" code=""
  local started finished elapsed
  started=$(now_ms)
  if command -v curl >/dev/null 2>&1; then
    body=$(curl -ksS --max-time "${HA_HTTP_TIMEOUT:-5}" -H "Authorization: Bearer ${XROK_API_TOKEN:-local-ha-token}" -w '\n%{http_code}' "$url" 2>/dev/null || true)
    code="${body##*$'\n'}"; body="${body%$'\n'*}"
  fi
  finished=$(now_ms); elapsed=$((finished-started))
  health_timings=$(NAME="$name" ELAPSED="$elapsed" CODE="$code" PREV="$health_timings" python3 -c 'import json,os; d=json.loads(os.environ["PREV"]); d[os.environ["NAME"]]={"latency_ms":int(os.environ["ELAPSED"]),"status_code":os.environ["CODE"]}; print(json.dumps(d,separators=(",",":")))')
  [[ "$code" == "$expected" ]] || fail "$name endpoint unhealthy (code=${code:-unavailable})"
}

up_attempts="${HA_COMPOSE_UP_RETRIES:-5}"
up_ok=0
validate_ports() {
  local p
  for p in "$XROK_HA_PORT_A" "$XROK_HA_UDP_A" "$XROK_HA_PORT_B" "$XROK_HA_UDP_B"; do
    [[ "$p" =~ ^[0-9]+$ ]] && ((p > 0 && p < 65536)) || return 1
  done
  [[ "$XROK_HA_PORT_A" != "$XROK_HA_PORT_B" ]] || return 1
  [[ "$XROK_HA_UDP_A" != "$XROK_HA_UDP_B" ]] || return 1
}
for attempt in $(seq 1 "$up_attempts"); do
  attempts=$attempt
  project="${COMPOSE_PROJECT_NAME:-xrok-ha-$RANDOM-$$-$attempt}"
  compose=(docker compose -p "$project" -f "$compose_file")
  export XROK_HA_PORT_A="${XROK_HA_PORT_A_OVERRIDE:-$(pick_port)}"
  export XROK_HA_UDP_A="${XROK_HA_UDP_A_OVERRIDE:-$XROK_HA_PORT_A}"
  PORT_AVOID="$XROK_HA_PORT_A"; export PORT_AVOID
  export XROK_HA_PORT_B="${XROK_HA_PORT_B_OVERRIDE:-$(pick_port "$XROK_HA_PORT_A")}" 
  export XROK_HA_UDP_B="${XROK_HA_UDP_B_OVERRIDE:-$XROK_HA_PORT_B}"
  if ! validate_ports; then
    collision_count=$((collision_count+1)); echo "invalid/conflicting port mapping (attempt $attempt)" >>"$diagnostics_file"; continue
  fi
  selected_tcp_a="$XROK_HA_PORT_A"; selected_udp_a="$XROK_HA_UDP_A"; selected_tcp_b="$XROK_HA_PORT_B"; selected_udp_b="$XROK_HA_UDP_B"
  up_started=$(now_ms)
  attempt_dir="$log_dir/attempt-$attempt"
  current_attempt_dir="$attempt_dir"
  mkdir -p "$attempt_dir"
  {
    echo "=== attempt $attempt project=$project ==="
    echo "ports: tcp_a=$XROK_HA_PORT_A udp_a=$XROK_HA_UDP_A tcp_b=$XROK_HA_PORT_B udp_b=$XROK_HA_UDP_B"
    "${compose[@]}" config --environment 2>&1 || true
    "${compose[@]}" config 2>&1
  } >>"$diagnostics_file"
  if "${compose[@]}" config >"$attempt_dir/config.out" 2>"$attempt_dir/config.err" && "${compose[@]}" up -d --build >"$attempt_dir/up.out" 2>"$attempt_dir/up.err"; then
    up_finished=$(now_ms); up_duration_ms=$((up_finished-up_started))
    up_ok=1
    break
  fi
  up_finished=$(now_ms); up_duration_ms=$((up_finished-up_started))
  last_error_log="$attempt_dir/up.err"
  if grep -Eqi 'operation not permitted|permission denied|cannot connect to the docker daemon|is the docker daemon running' "$attempt_dir/up.err"; then
    failure_class="environment"
  elif grep -Eqi 'address already in use|bind.*failed|port is already allocated' "$attempt_dir/up.err"; then
    failure_class="bind"
    collision_count=$((collision_count+1))
  elif grep -Eqi 'build|failed to solve|dockerfile|image' "$attempt_dir/up.err"; then
    failure_class="build"
  elif [[ -s "$attempt_dir/config.err" ]]; then
    failure_class="config"
    last_error_log="$attempt_dir/config.err"
  else
    failure_class="up"
  fi
  "${compose[@]}" ps >"$attempt_dir/ps.txt" 2>&1 || true
  "${compose[@]}" logs --no-color >"$attempt_dir/logs.txt" 2>&1 || true
  # Docker performs the actual bind only during `up`; a prior socket probe is
  # therefore advisory. Tear down the partial project, choose a fresh set of
  # ports, and retry when another lab wins the race.
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
done
[[ "$up_ok" == 1 ]] || fail "compose up failed after $up_attempts attempts (class=${failure_class:-unknown}; see ${last_error_log:-$log_dir})"
for svc in redis postgres xrok-a xrok-b; do wait_running "$svc"; done
if [[ "${HA_ASSERT_HTTP:-0}" == "1" ]]; then
  check_http_endpoint "xrok_a_health" "$XROK_HA_PORT_A" "/health" 200
  check_http_endpoint "xrok_a_ready" "$XROK_HA_PORT_A" "/ready" 200
  check_http_endpoint "xrok_b_health" "$XROK_HA_PORT_B" "/health" 200
  check_http_endpoint "xrok_b_ready" "$XROK_HA_PORT_B" "/ready" 200
fi
if [[ "${HA_ASSERT_METRICS:-0}" == "1" ]]; then
  check_http_endpoint "xrok_a_metrics" "$XROK_HA_PORT_A" "/metrics" 200
  check_http_endpoint "xrok_b_metrics" "$XROK_HA_PORT_B" "/metrics" 200
fi

# Redis AOF persistence check.
"${compose[@]}" exec -T redis redis-cli SET xrok-ha-integration alive >/dev/null
"${compose[@]}" restart redis >/dev/null
wait_running redis
[[ "$(${compose[@]} exec -T redis redis-cli GET xrok-ha-integration | tr -d '\r')" == alive ]] || fail "redis AOF value lost"

# Exercise the real Redis command path under bounded load, then inject a
# transient failure and verify the service recovers before continuing.
for i in $(seq 1 "$load_iterations"); do
  "${compose[@]}" exec -T redis redis-cli SET "xrok-load-$i" "$i" >/dev/null
done
[[ "$(${compose[@]} exec -T redis redis-cli GET "xrok-load-$load_iterations" | tr -d '\r')" == "$load_iterations" ]] || fail "redis load write/read failed"
"${compose[@]}" stop redis >/dev/null
for _ in $(seq 1 "$retries"); do
  "${compose[@]}" start redis >/dev/null 2>&1 || true
  if "${compose[@]}" exec -T redis redis-cli ping 2>/dev/null | grep -q PONG; then break; fi
  sleep "$interval"
done
wait_running redis
[[ "$(${compose[@]} exec -T redis redis-cli GET xrok-ha-integration | tr -d '\r')" == alive ]] || fail "redis did not recover after failure"

# PostgreSQL volume persistence check.
"${compose[@]}" exec -T postgres psql -U xrok -d xrok -v ON_ERROR_STOP=1 \
  -c 'CREATE TABLE IF NOT EXISTS xrok_ha_integration (id text PRIMARY KEY, value text NOT NULL);' \
  -c "INSERT INTO xrok_ha_integration(id,value) VALUES ('smoke','alive') ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value;" >/dev/null
"${compose[@]}" restart postgres >/dev/null
wait_running postgres
pg_value=$(${compose[@]} exec -T postgres psql -U xrok -d xrok -Atqc "SELECT value FROM xrok_ha_integration WHERE id='smoke';" | tr -d '\r')
[[ "$pg_value" == alive ]] || fail "postgres volume value lost"

# PostgreSQL driver path/load and restart recovery.  psql speaks the same
# wire protocol used by the database/sql adapter in the runtime.
for i in $(seq 1 "$load_iterations"); do
  "${compose[@]}" exec -T postgres psql -U xrok -d xrok -v ON_ERROR_STOP=1 -c \
    "INSERT INTO xrok_ha_integration(id,value) VALUES ('load-$i','$i') ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value;" >/dev/null
done
pg_load=$(${compose[@]} exec -T postgres psql -U xrok -d xrok -Atqc "SELECT value FROM xrok_ha_integration WHERE id='load-$load_iterations';" | tr -d '\r')
[[ "$pg_load" == "$load_iterations" ]] || fail "postgres load write/read failed"
"${compose[@]}" stop postgres >/dev/null
for _ in $(seq 1 "$retries"); do
  "${compose[@]}" start postgres >/dev/null 2>&1 || true
  if "${compose[@]}" exec -T postgres pg_isready -U xrok -d xrok >/dev/null 2>&1; then break; fi
  sleep "$interval"
done
wait_running postgres
pg_value=$(${compose[@]} exec -T postgres psql -U xrok -d xrok -Atqc "SELECT value FROM xrok_ha_integration WHERE id='smoke';" | tr -d '\r')
[[ "$pg_value" == alive ]] || fail "postgres did not recover after failure"

# Replica restart and peer availability.
"${compose[@]}" restart xrok-a >/dev/null
after=$(${compose[@]} ps -q xrok-a)
[[ -n "$after" ]] || fail "xrok-a did not restart"
wait_running xrok-a
"${compose[@]}" stop -t "${HA_DRAIN_TIMEOUT:-10}" xrok-a >/dev/null
wait_running xrok-b

status=passed
echo "HA Docker integration: PASS (artifact: $artifact)"
