#!/usr/bin/env bash
# Fast compatibility entry point for the authoritative HA integration runner.
# Keeping one implementation prevents the smoke and integration paths from
# drifting on restart semantics, port retries, cleanup, and artifact schema.
set -euo pipefail

root_dir="$(cd "$(dirname "$0")/.." && pwd)"
export HA_INTEGRATION_LOAD_ITERATIONS="${HA_INTEGRATION_LOAD_ITERATIONS:-1}"
export HA_INTEGRATION_ARTIFACT="${HA_INTEGRATION_ARTIFACT:-${HA_FAILOVER_ARTIFACT:-$root_dir/.artifacts/ha-failover-smoke.json}}"
exec "$root_dir/scripts/ha-docker-integration.sh"
