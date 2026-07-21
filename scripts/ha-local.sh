#!/usr/bin/env bash
set -euo pipefail

compose_file="${XROK_HA_COMPOSE_FILE:-compose.ha.local.yml}"
action="${1:-config}"

case "$action" in
  config) docker compose -f "$compose_file" config >/dev/null; echo "HA compose config: OK" ;;
  up) docker compose -f "$compose_file" up -d --build ;;
  down) docker compose -f "$compose_file" down ;;
  ps|logs) docker compose -f "$compose_file" "$action" "${@:2}" ;;
  smoke)
    docker compose -f "$compose_file" up -d --build
    trap 'docker compose -f "$compose_file" down' EXIT
    docker compose -f "$compose_file" ps --status running | grep -q redis
    docker compose -f "$compose_file" ps --status running | grep -q postgres
    docker compose -f "$compose_file" ps --status running | grep -q xrok-a
    docker compose -f "$compose_file" ps --status running | grep -q xrok-b
    echo "HA local smoke: PASS"
    ;;
  *) echo "usage: $0 {config|up|down|ps|logs|smoke}" >&2; exit 2 ;;
esac
