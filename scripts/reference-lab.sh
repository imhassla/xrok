#!/usr/bin/env bash
set -euo pipefail
c=(docker compose -f compose.reference.yml)
case "${1:-config}" in
 config) "${c[@]}" config >/dev/null; echo "reference compose: valid";;
 up|down|ps|logs) "${c[@]}" "$@";;
 smoke) curl -fsS http://127.0.0.1:9090/-/ready >/dev/null; curl -fsS http://127.0.0.1:3000/api/health >/dev/null; curl -fsS http://127.0.0.1:13133/ >/dev/null; echo "reference lab: healthy";;
 nat) echo "profiles: host, degraded (toxiproxy), relay-only (coturn)";;
 *) echo "usage: $0 {config|up|down|ps|logs|smoke|nat}"; exit 2;;
esac
