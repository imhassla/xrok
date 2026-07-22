#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "$0")/../.." && pwd)"
test_dir="$(mktemp -d)"
cleanup() { rm -rf "$test_dir"; }
trap cleanup EXIT INT TERM
mkdir -p "$test_dir/bin" "$test_dir/certs" "$test_dir/artifacts"
: >"$test_dir/certs/server.pem"
: >"$test_dir/certs/server-key.pem"
# ensure_coturn_certs requires non-empty files.
printf 'test-cert\n' >"$test_dir/certs/server.pem"
printf 'test-key\n' >"$test_dir/certs/server-key.pem"

cat >"$test_dir/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$REFERENCE_TEST_CALLS"
if [[ " $* " == *" info "* ]]; then exit 0; fi
if [[ " $* " == *" up -d --wait "* ]]; then
  count=0
  [[ ! -f "$REFERENCE_TEST_COUNT" ]] || count=$(cat "$REFERENCE_TEST_COUNT")
  count=$((count + 1)); printf '%s\n' "$count" >"$REFERENCE_TEST_COUNT"
  if [[ "$count" == 1 ]]; then
    echo 'Bind for 0.0.0.0 failed: port is already allocated' >&2
    exit 1
  fi
fi
exit 0
SH
cat >"$test_dir/bin/curl" <<'SH'
#!/usr/bin/env bash
exit 0
SH
chmod +x "$test_dir/bin/docker" "$test_dir/bin/curl"

export PATH="$test_dir/bin:$PATH"
export REFERENCE_TEST_CALLS="$test_dir/docker.calls"
export REFERENCE_TEST_COUNT="$test_dir/up.count"
export COTURN_CERT_DIR="$test_dir/certs"
export XROK_ARTIFACT_DIR="$test_dir/artifacts"
export REFERENCE_LAB_ARTIFACT="$test_dir/result.json"
export COMPOSE_PROJECT_NAME="reference-retry-test"

"$root_dir/scripts/reference-lab.sh" up-smoke >/dev/null
[[ "$(cat "$REFERENCE_TEST_COUNT")" == 2 ]]
grep -q -- '-p reference-retry-test-1' "$REFERENCE_TEST_CALLS"
grep -q -- '-p reference-retry-test-2' "$REFERENCE_TEST_CALLS"
grep -q -- 'up -d --wait --wait-timeout' "$REFERENCE_TEST_CALLS"
grep -q -- 'down --volumes --remove-orphans' "$REFERENCE_TEST_CALLS"
python3 - "$REFERENCE_LAB_ARTIFACT" <<'PY'
import json, sys
value = json.load(open(sys.argv[1]))
assert value['status'] == 'passed', value
PY
echo "reference lab retry smoke: PASS"
