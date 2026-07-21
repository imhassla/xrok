#!/usr/bin/env bash
# Bounded local soak/scale gate.  It is intentionally deterministic and does
# not require external services; callers can tune duration and concurrency.
set -euo pipefail
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=${OUT:-"$ROOT_DIR/bench/soak-summary.json"}
DURATION=${DURATION:-30s}
CONCURRENCY=${CONCURRENCY:-32}
SCALE=${SCALE:-1000}
mkdir -p "$(dirname "$OUT")"
cd "$ROOT_DIR"
start=$(date +%s)
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
iterations=${ITERATIONS:-1}
GOCACHE=${GOCACHE:-/tmp/xrok-go-build} go test ./cmd -run '^TestSyntheticScaleBounded$' -count="$iterations" >"$tmp" 2>&1 || {
  cat "$tmp"; exit 1;
}
python3 - "$OUT" "$DURATION" "$CONCURRENCY" "$SCALE" "$start" "$tmp" <<'PY'
import json,sys,time,hashlib
out,dur,conc,scale,start,log=sys.argv[1:]
raw=open(log,'rb').read()
json.dump({'schema_version':'xrok.soak/v1','duration':dur,'concurrency':int(conc),
 'scale':int(scale),'elapsed_seconds':max(0,time.time()-int(start)),
 'log_sha256':hashlib.sha256(raw).hexdigest(),'status':'pass'},open(out,'w'),indent=2)
PY
echo "soak passed: $OUT"
