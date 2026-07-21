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
failures=0
now_ns() { python3 -c 'import time; print(time.monotonic_ns())'; }
measure() {
  local name=$1 url=$2 i start end elapsed body code bytes
  for i in $(seq 1 "$ITERATIONS"); do
    body=$(mktemp); start=$(now_ns)
    code=$(curl --silent --show-error --max-time "${CURL_TIMEOUT:-15}" -o "$body" -w '%{http_code}' "$url" 2>/dev/null) || code=000
    end=$(now_ns)
    elapsed=$(python3 -c 'import sys; print(f"{(int(sys.argv[2])-int(sys.argv[1]))/1_000_000:.3f}")' "$start" "$end")
    bytes=$(wc -c <"$body" | tr -d ' '); rm -f "$body"
    [[ "$code" =~ ^[0-9]{3}$ ]] || code=000
    if [[ ! "$code" =~ ^[23][0-9][0-9]$ ]]; then failures=$((failures+1)); fi
    printf '%s,%s,%s,%s,%s\n' "$name" "$i" "$elapsed" "$code" "$bytes" | tee -a "$OUT"
  done
}
docker compose -f "$COMPOSE_FILE" ps >/dev/null
measure xrok "$XROK_URL"
measure frp "$FRP_URL"
echo "results written to $OUT"
echo "For CPU/RSS snapshots: docker stats --no-stream xrok-benchmark-xrok-1 xrok-benchmark-frps-1"
if (( failures > 0 )) && [[ "${BENCHMARK_ALLOW_ERRORS:-0}" != 1 ]]; then
  echo "benchmark workload failed: $failures non-success responses (set BENCHMARK_ALLOW_ERRORS=1 only when measuring failure behavior)" >&2
  exit 1
fi
