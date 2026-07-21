#!/usr/bin/env bash
# Network-free contract tests for lab aggregation and benchmark fail-closed behavior.
set -euo pipefail

root_dir="$(cd "$(dirname "$0")/../.." && pwd)"
tmp_dir=$(mktemp -d)
cleanup() { rm -rf "$tmp_dir"; }
trap cleanup EXIT INT TERM

make_runner() {
  local path="$1" status="$2" exit_code="$3" mode="${4:-json}"
  printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'out=${HA_INTEGRATION_ARTIFACT:-${NAT_MATRIX_ARTIFACT:-${REFERENCE_LAB_ARTIFACT:?artifact variable missing}}}' \
    "if [[ '$mode' == malformed ]]; then printf 'not-json\\n' >\"\$out\"; else printf '{\"schema_version\":\"xrok.lab-result/v1\",\"lab\":\"fake\",\"status\":\"$status\"}\\n' >\"\$out\"; fi" \
    "exit $exit_code" >"$path"
  chmod +x "$path"
}

make_runner "$tmp_dir/pass" passed 0
make_runner "$tmp_dir/skip" skipped 0
make_runner "$tmp_dir/fail" failed 1
make_runner "$tmp_dir/malformed" passed 0 malformed

run_parallel() {
  local out="$1" ha="$2" nat="$3" reference="$4"
  LAB_PARALLEL_ARTIFACT_DIR="$out" \
  LAB_PARALLEL_HA_SCRIPT="$ha" LAB_PARALLEL_NAT_SCRIPT="$nat" \
  LAB_PARALLEL_REFERENCE_SCRIPT="$reference" "$root_dir/scripts/lab-parallel.sh"
}

run_parallel "$tmp_dir/all-pass" "$tmp_dir/pass" "$tmp_dir/pass" "$tmp_dir/pass"
python3 - "$tmp_dir/all-pass/summary.json" <<'PY'
import json, sys
d=json.load(open(sys.argv[1])); assert d['status']=='passed'; assert set(d['labs'])=={'ha','nat','reference'}
PY

run_parallel "$tmp_dir/all-skip" "$tmp_dir/skip" "$tmp_dir/skip" "$tmp_dir/skip"
python3 - "$tmp_dir/all-skip/summary.json" <<'PY'
import json, sys
assert json.load(open(sys.argv[1]))['status']=='skipped'
PY

if run_parallel "$tmp_dir/one-fail" "$tmp_dir/fail" "$tmp_dir/pass" "$tmp_dir/pass"; then
  echo "orchestrator accepted a failed child" >&2; exit 1
fi
if run_parallel "$tmp_dir/malformed-result" "$tmp_dir/malformed" "$tmp_dir/pass" "$tmp_dir/pass"; then
  echo "orchestrator accepted a malformed artifact" >&2; exit 1
fi

# A regression gate must fail when required evidence is absent and persist a
# machine-readable failed result instead of only printing a warning.
if BASELINE="$root_dir/bench/baseline.json" SCALE="$tmp_dir/missing-scale.json" \
  APPLES="$tmp_dir/missing-apples.csv" ANALYTICS_OUT="$tmp_dir/regression.json" \
  "$root_dir/scripts/benchmark-regression.sh" >/dev/null 2>&1; then
  echo "benchmark gate accepted missing evidence" >&2; exit 1
fi
python3 - "$tmp_dir/regression.json" <<'PY'
import json, sys
assert json.load(open(sys.argv[1]))['status']=='failed'
PY

printf '%s\n' '[{"tunnels":1000,"put_ms":1,"get_ms":1},{"tunnels":10000,"put_ms":2,"get_ms":2}]' >"$tmp_dir/scale.json"
printf '%s\n' \
  'implementation,iteration,latency_ms,http_code,bytes' \
  'xrok,1,1.0,200,100' 'xrok,2,1.1,200,100' 'xrok,3,1.2,200,100' \
  'frp,1,1.0,200,100' 'frp,2,1.1,200,100' 'frp,3,1.2,200,100' >"$tmp_dir/apples.csv"
BASELINE="$root_dir/bench/baseline.json" SCALE="$tmp_dir/scale.json" \
  APPLES="$tmp_dir/apples.csv" ANALYTICS_OUT="$tmp_dir/regression-pass.json" \
  "$root_dir/scripts/benchmark-regression.sh" >/dev/null
python3 - "$tmp_dir/regression-pass.json" <<'PY'
import json, sys
assert json.load(open(sys.argv[1]))['status']=='passed'
PY

echo "lab orchestration smoke: PASS"
