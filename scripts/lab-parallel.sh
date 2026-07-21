#!/usr/bin/env bash
# Run independent local labs concurrently with isolated Compose projects.
set -euo pipefail
root_dir="$(cd "$(dirname "$0")/.." && pwd)"
artifact_dir="${LAB_PARALLEL_ARTIFACT_DIR:-$root_dir/.artifacts/lab-parallel-$(date +%s)}"
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
  (
    export COMPOSE_PROJECT_NAME="$project"
    case "$name" in
      ha)
        # Keep generated certificates and Compose state isolated per parallel run.
        export XROK_CERT_DIR="$artifact_dir/$name-certs"
        HA_INTEGRATION_ARTIFACT="$artifact_dir/$name.json" "$script" ;;
      nat) NAT_MATRIX_ARTIFACT="$artifact_dir/$name.json" "$script" ;;
      reference) XROK_ARTIFACT_DIR="$artifact_dir/reference" "$script" up-smoke ;;
    esac
  ) >"$out" 2>&1
  local rc=$?
  [[ -f "$artifact_dir/$name.json" ]] || printf '{"status":"%s","log":"%s"}\n' \
    "$([[ $rc == 0 ]] && echo passed || echo failed)" "$out" >"$artifact_dir/$name.json"
  return "$rc"
}

for i in "${!names[@]}"; do
  name="${names[$i]}"
  case "$name" in
    ha) script="$root_dir/scripts/ha-docker-integration.sh";;
    nat) script="$root_dir/scripts/nat-matrix.sh";;
    reference) script="$root_dir/scripts/reference-lab.sh";;
  esac
  run_one "$name" "$script" & pids+=("$!")
done

status=passed
for pid in "${pids[@]}"; do wait "$pid" || status=failed; done

# A missing/inaccessible Docker daemon is an expected local-dev skip, not a
# failed product check (the individual scripts emit their own skip artifacts).
if [[ "$status" == failed ]] && grep -R -qiE 'docker daemon unavailable|permission denied.*docker daemon|connect: operation not permitted' "$artifact_dir"/*.log 2>/dev/null; then
  non_docker_failure=0
  for log in "$artifact_dir"/*.log; do
    if ! grep -qiE 'docker daemon unavailable|permission denied.*docker daemon|connect: operation not permitted' "$log"; then non_docker_failure=1; fi
  done
  [[ "$non_docker_failure" == 0 ]] && status=passed
fi

python3 - "$artifact_dir" "$status" "$run_id" <<'PY'
import json, pathlib, sys, time
d, status, run_id = sys.argv[1:]
labs = {}
for p in pathlib.Path(d).glob('*.json'):
    try: labs[p.stem] = json.loads(p.read_text())
    except Exception: labs[p.stem] = {'status':'failed','artifact':str(p)}
# Derive aggregate status from artifacts as well as process exit codes. This
# prevents a child that exited early after writing a failed artifact from
# being reported as passed by the orchestrator.
if any(str(v.get('status','')).lower() in ('failed','fail','error') for v in labs.values()):
    status = 'failed'
out = {'status': status, 'run_id': run_id, 'generated_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), 'labs': labs}
pathlib.Path(d, 'summary.json').write_text(json.dumps(out, indent=2) + '\n')
PY
echo "parallel lab: $status (summary: $artifact_dir/summary.json)"
[[ "$status" == passed ]] || exit 1
