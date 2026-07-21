#!/usr/bin/env bash
set -euo pipefail
COMPOSE_FILE=${COMPOSE_FILE:-compose.benchmark.yml}
case "${1:-up}" in
  config) docker compose -f "$COMPOSE_FILE" config >/dev/null; echo "benchmark compose is valid" ;;
  up) docker compose -f "$COMPOSE_FILE" up -d --build ;;
  down) docker compose -f "$COMPOSE_FILE" down ;;
  logs) docker compose -f "$COMPOSE_FILE" logs --tail="${2:-100}" ;;
  ps) docker compose -f "$COMPOSE_FILE" ps ;;
  *) echo "usage: $0 {config|up|down|logs [n]|ps}" >&2; exit 2 ;;
esac
