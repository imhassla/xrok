#!/usr/bin/env bash
# Run a repeatable xrok/frp benchmark workload.  The workload command must
# exercise an application endpoint exposed through each tunnel; this keeps the
# comparison apples-to-apples while allowing users to choose HTTP or raw TCP.
set -euo pipefail
COMPOSE_FILE=${COMPOSE_FILE:-compose.benchmark.yml}
OUT=${OUT:-bench/results.csv}
ITERATIONS=${ITERATIONS:-5}
XROK_URL=${XROK_URL:-http://127.0.0.1:${XROK_APP_PORT:-18080}}
FRP_URL=${FRP_URL:-http://127.0.0.1:${FRP_APP_PORT:-18081}}
mkdir -p "$(dirname "$OUT")"
printf 'implementation,iteration,latency_ms,http_code,bytes\n' >"$OUT"
measure() {
  local name=$1 url=$2 i start end elapsed body code bytes
  for i in $(seq 1 "$ITERATIONS"); do
    body=$(mktemp); start=$(date +%s%N)
    code=$(curl --silent --show-error --max-time "${CURL_TIMEOUT:-15}" -o "$body" -w '%{http_code}' "$url" || echo 000)
    end=$(date +%s%N); elapsed=$(( (end-start)/1000000 )); bytes=$(wc -c <"$body" | tr -d ' '); rm -f "$body"
    printf '%s,%s,%s,%s,%s\n' "$name" "$i" "$elapsed" "$code" "$bytes" | tee -a "$OUT"
  done
}
docker compose -f "$COMPOSE_FILE" ps >/dev/null
measure xrok "$XROK_URL"
measure frp "$FRP_URL"
echo "results written to $OUT"
echo "For CPU/RSS snapshots: docker stats --no-stream xrok-benchmark-xrok-1 xrok-benchmark-frps-1"
