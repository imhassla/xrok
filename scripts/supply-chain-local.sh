#!/usr/bin/env bash
# Offline-friendly supply-chain gate. Optional tools enrich the report.
set -euo pipefail
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
OUT=${OUT:-"$ROOT_DIR/bench/supply-chain.json"}
mkdir -p "$(dirname "$OUT")"; cd "$ROOT_DIR"
go mod verify
GOCACHE=${GOCACHE:-/tmp/xrok-go-build} go build -trimpath -buildvcs=false -o /tmp/xrok-supply-a .
GOCACHE=${GOCACHE:-/tmp/xrok-go-build} go build -trimpath -buildvcs=false -o /tmp/xrok-supply-b .
sha_a=$(shasum -a 256 /tmp/xrok-supply-a | awk '{print $1}')
sha_b=$(shasum -a 256 /tmp/xrok-supply-b | awk '{print $1}')
test "$sha_a" = "$sha_b"
sbom_status=skipped
if command -v syft >/dev/null 2>&1; then syft dir:. -o cyclonedx-json=/tmp/xrok-sbom.json >/dev/null; sbom_status=generated; fi
python3 - "$OUT" "$sha_a" "$sbom_status" <<'PY'
import json,sys,time
json.dump({'schema_version':'xrok.supply-chain/v1','created_at':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),'reproducible_sha256':sys.argv[2],'sbom':sys.argv[3],'module_verified':True},open(sys.argv[1],'w'),indent=2)
PY
echo "supply-chain gate passed: $OUT"
