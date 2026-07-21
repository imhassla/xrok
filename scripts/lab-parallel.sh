#!/usr/bin/env bash
# Run independent local labs concurrently with isolated Compose projects.
set -euo pipefail
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
artifact_dir="${LAB_PARALLEL_ARTIFACT_DIR:-$root_dir/.artifacts/lab-parallel-$(date +%s)-$$}"
mkdir -p "$artifact_dir"
run_id="${LAB_PARALLEL_RUN_ID:-xrok-lab-$(date +%s)-$$}"
pids=(); names=(ha nat reference)

cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  # Each child owns its Compose project and performs normal cleanup via traps.
}
trap cleanup EXIT INT TERM

run_one() {
  local name="$1" script="$2" project="$run_id-$name" out="$artifact_dir/$name.log"
  local rc=0
  (
    export COMPOSE_PROJECT_NAME="$project"
    case "$name" in
      ha)
        # Keep generated certificates and Compose state isolated per parallel run.
        export XROK_CERT_DIR="$artifact_dir/$name-certs"
        HA_INTEGRATION_ARTIFACT="$artifact_dir/$name.json" "$script" ;;
      nat) NAT_MATRIX_ARTIFACT="$artifact_dir/$name.json" "$script" ;;
      reference)
        XROK_ARTIFACT_DIR="$artifact_dir/reference" \
          REFERENCE_LAB_ARTIFACT="$artifact_dir/$name.json" "$script" up-smoke ;;
    esac
  ) >"$out" 2>&1 || rc=$?
  if [[ ! -f "$artifact_dir/$name.json" ]]; then
    NAME="$name" RC="$rc" LOG="$out" OUT="$artifact_dir/$name.json" python3 - <<'PY'
import json, os, time
json.dump({
    "schema_version": "xrok.lab-result/v1",
    "lab": os.environ["NAME"],
    "status": "failed",
    "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    "reason": "runner exited without an artifact",
    "exit_code": int(os.environ["RC"]),
    "log": os.environ["LOG"],
}, open(os.environ["OUT"], "w"), indent=2)
PY
    rc=1
  fi
  return "$rc"
}

for i in "${!names[@]}"; do
  name="${names[$i]}"
  case "$name" in
    ha) script="${LAB_PARALLEL_HA_SCRIPT:-$root_dir/scripts/ha-docker-integration.sh}";;
    nat) script="${LAB_PARALLEL_NAT_SCRIPT:-$root_dir/scripts/nat-matrix.sh}";;
    reference) script="${LAB_PARALLEL_REFERENCE_SCRIPT:-$root_dir/scripts/reference-lab.sh}";;
  esac
  run_one "$name" "$script" & pids+=("$!")
done

process_status=passed
for pid in "${pids[@]}"; do wait "$pid" || process_status=failed; done

status=$(python3 - "$artifact_dir" "$process_status" "$run_id" <<'PY'
import json, pathlib, sys, time
d, status, run_id = sys.argv[1:]
labs = {}
for name in ('ha', 'nat', 'reference'):
    p = pathlib.Path(d, name + '.json')
    try:
        value = json.loads(p.read_text())
        if not isinstance(value, dict) or value.get('status') not in ('passed', 'failed', 'skipped'):
            raise ValueError('invalid lab status')
        labs[name] = value
    except Exception as exc:
        labs[name] = {'schema_version':'xrok.lab-result/v1', 'lab':name,
                      'status':'failed', 'reason':f'invalid artifact: {exc}', 'artifact':str(p)}
states = [v['status'] for v in labs.values()]
if status != 'passed' or 'failed' in states:
    status = 'failed'
elif all(s == 'skipped' for s in states):
    status = 'skipped'
else:
    status = 'passed'
out = {'schema_version':'xrok.lab-summary/v1', 'status': status, 'run_id': run_id,
       'generated_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), 'labs': labs}
pathlib.Path(d, 'summary.json').write_text(json.dumps(out, indent=2) + '\n')
print(status)
PY
)
echo "parallel lab: $status (summary: $artifact_dir/summary.json)"
[[ "$status" == passed || "$status" == skipped ]] || exit 1
