#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DURATION=${E2E_SOAK_DURATION:-60s}; TIMEOUT=${E2E_SOAK_TIMEOUT:-120s}
OUT=${E2E_SOAK_OUT:-"$ROOT_DIR/bench/e2e-soak-summary.json"}; mkdir -p "$(dirname "$OUT")"; cd "$ROOT_DIR"
duration_s=$(python3 - "$DURATION" <<'PY'
import sys
s=sys.argv[1].lower(); u={'s':1,'m':60,'h':3600}
v=float(s[:-1])*u[s[-1]] if s[-1] in u else float(s)
print(max(0,int(v)))
PY
)
start=$(date +%s); count=0; status=pass; log=$(mktemp); trap 'rm -f "$log"' EXIT INT TERM
while (( $(date +%s) - start < duration_s )); do
 count=$((count+1)); : >"$log"
 if ! timeout "$TIMEOUT" go test -tags=e2e ./e2e -count=1 >>"$log" 2>&1; then cat "$log"; status=fail; break; fi
done
python3 - "$OUT" "$DURATION" "$count" "$status" "$start" <<'PY'
import json,sys,time
o,d,c,s,t=sys.argv[1:]; json.dump({'schema_version':'xrok.e2e-soak/v1','duration':d,'iterations':int(c),'elapsed_seconds':max(0,time.time()-int(t)),'status':s},open(o,'w'),indent=2)
PY
[[ "$status" == pass ]] || exit 1
echo "e2e soak passed: $OUT (iterations=$count)"
