#!/usr/bin/env bash
# Run bounded 1k/10k virtual-tunnel scale checks and capture JSON plus RSS/CPU.
set -euo pipefail
OUT=${OUT:-bench/results-scale.json}
mkdir -p "$(dirname "$OUT")"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
if command -v /usr/bin/time >/dev/null 2>&1; then
  /usr/bin/time -l env SCALE_OUTPUT="$tmp" go test ./cmd -run '^TestSyntheticScaleBounded$' -count=1 2>"${tmp}.time"
else
  env SCALE_OUTPUT="$tmp" go test ./cmd -run '^TestSyntheticScaleBounded$' -count=1
  : >"${tmp}.time"
fi
# Keep the test's deterministic measurements intact; resource figures are
# emitted separately because `time -l` format differs between Unix systems.
cp "$tmp" "$OUT"
awk '/maximum resident set size/{rss=$1} /percent of CPU/{cpu=$1} END {printf("{\"max_rss_bytes\":%s,\"cpu_percent\":%s}\n", rss+0, cpu+0)}' "${tmp}.time" >"${OUT%.json}.resources.json" 2>/dev/null || true
echo "scale results written to $OUT"
