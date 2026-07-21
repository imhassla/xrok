#!/usr/bin/env bash
set -euo pipefail

# Reproducible local microbenchmarks. Set BENCHTIME (for example 5s) to trade
# runtime for confidence. A frpc command can be supplied separately by the
# operator; this script never downloads or mutates third-party tooling.
ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
BENCHTIME=${BENCHTIME:-3s}
GOCACHE=${GOCACHE:-/tmp/xrok-go-build}
GOMODCACHE=${GOMODCACHE:-/tmp/xrok-go-mod}

cd "$ROOT_DIR"
GOCACHE="$GOCACHE" GOMODCACHE="$GOMODCACHE" \
  go test -run '^$' -bench=. -benchmem -benchtime="$BENCHTIME" ./...

cat <<'EOF'

For an apples-to-apples frp comparison, run the same benchmark workload with
frpc/frps pinned to a recorded version and store both outputs as artifacts.
The xrok suite intentionally does not install or fetch frp automatically.
EOF
