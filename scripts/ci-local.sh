#!/usr/bin/env bash
set -euo pipefail

# Reproducible, network-free validation gate for the local reference labs.
# Set CI_ARTIFACT_DIR to retain logs outside the repository.
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ARTIFACT_DIR=${CI_ARTIFACT_DIR:-"$ROOT_DIR/.artifacts/local-ci"}
mkdir -p "$ARTIFACT_DIR"
REPORT="$ARTIFACT_DIR/report.txt"
: >"$REPORT"
run() {
  local name=$1; shift
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
run shell-syntax bash -c 'for f in scripts/*.sh; do bash -n "$f"; done'
run go-test go test ./...
run go-vet go vet ./...
run go-race go test -race ./cmd/...
# Short fuzz smoke verifies targets compile and execute deterministically.
run fuzz-smoke bash -c 'go test ./cmd/config -run "^$" -fuzz=FuzzLoadServerConfigBytes -fuzztime=100ms && go test ./cmd/config -run "^$" -fuzz=FuzzLoadClientConfigBytes -fuzztime=100ms && go test ./cmd/p2p -run "^$" -fuzz=FuzzDecodeSignal -fuzztime=100ms'
{
  echo "status=PASS"
  echo "artifact_dir=$ARTIFACT_DIR"
  echo "commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
} | tee -a "$REPORT"
echo "local CI passed; report: $REPORT"
