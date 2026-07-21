#!/usr/bin/env bash
set -euo pipefail

# Reproducible, network-free validation gate for the local reference labs.
# Set CI_ARTIFACT_DIR to retain logs outside the repository.
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ARTIFACT_DIR=${CI_ARTIFACT_DIR:-"$ROOT_DIR/.artifacts/local-ci"}
mkdir -p "$ARTIFACT_DIR"
RETENTION=${CI_ARTIFACT_RETENTION:-10}
REPORT="$ARTIFACT_DIR/report.txt"
: >"$REPORT"
started_at=$(date +%s)
current_step=initializing
status=failed
finalize() {
  local rc=$?
  local ended_at
  local final_status="$status"
  if (( rc != 0 )); then final_status=failed; fi
  ended_at=$(date +%s)
  STATUS="$final_status" RC="$rc" STEP="$current_step" DURATION="$((ended_at-started_at))" OUT="$ARTIFACT_DIR/summary.json" python3 - <<'PY'
import json, os, time
json.dump({'schema_version':'xrok.ci-summary/v1','status':os.environ['STATUS'],
 'exit_code':int(os.environ['RC']),'last_step':os.environ['STEP'],
 'duration_seconds':int(os.environ['DURATION']),
 'created_at':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime())},
 open(os.environ['OUT'],'w'),indent=2)
PY
}
trap finalize EXIT
python3 - "$ARTIFACT_DIR/metadata.json" "$RETENTION" <<'PY'
import json, platform, subprocess, sys, time
out, retention = sys.argv[1:]
def git(*args):
    try: return subprocess.check_output(["git", *args], text=True).strip()
    except Exception: return "unknown"
json.dump({"schema_version":"xrok.ci-artifact/v1", "kind":"local-ci", "created_at":time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "commit":git("rev-parse","HEAD"), "platform":{"system":platform.system(),"release":platform.release(),"machine":platform.machine()}, "go":subprocess.check_output(["go","version"], text=True).strip(), "retention":int(retention)}, open(out,"w"), indent=2)
PY
run() {
  local name=$1; shift
  current_step="$name"
  echo "[$name] $*" | tee -a "$REPORT"
  "$@" >>"$REPORT" 2>&1
}

cd "$ROOT_DIR"
export GOCACHE=${GOCACHE:-/tmp/xrok-go-build}
export GOMODCACHE=${GOMODCACHE:-/tmp/xrok-go-mod}
run compose-config bash -c '
  command -v docker >/dev/null || { echo "docker unavailable"; exit 0; }
  for f in compose*.yml; do docker compose -f "$f" config >/dev/null; done
'
run shell-syntax bash -c 'for f in scripts/*.sh scripts/tests/*.sh; do bash -n "$f"; done'
run lab-orchestration-smoke ./scripts/tests/lab-orchestration-smoke.sh
run go-test go test ./...
run go-vet go vet ./...
run api-compat ./scripts/api-snapshot.sh
run go-race go test -race ./cmd/...
# Short fuzz smoke verifies targets compile and execute deterministically.
run fuzz-smoke bash -c 'go test ./cmd/config -run "^$" -fuzz=FuzzLoadServerConfigBytes -fuzztime=100ms && go test ./cmd/config -run "^$" -fuzz=FuzzLoadClientConfigBytes -fuzztime=100ms && go test ./cmd/p2p -run "^$" -fuzz=FuzzDecodeSignal -fuzztime=100ms'
if [[ "${CI_RUN_E2E_SOAK:-0}" == 1 ]]; then
  run e2e-soak ./scripts/e2e-soak.sh
fi
# Keep only the newest bounded number of retained run-* artifact directories.
current_step=artifact-retention
python3 - "$ARTIFACT_DIR/.." "$RETENTION" <<'PY'
import pathlib, shutil, sys
root=pathlib.Path(sys.argv[1]); keep=int(sys.argv[2])
runs=sorted((p for p in root.glob('run-*') if p.is_dir()),
            key=lambda p:(p.stat().st_mtime_ns,p.name), reverse=True)
for path in runs[keep:]: shutil.rmtree(path)
PY
{
  echo "status=PASS"
  echo "artifact_dir=$ARTIFACT_DIR"
  echo "commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
} | tee -a "$REPORT"
status=passed
current_step=complete
echo "local CI passed; report: $REPORT"
