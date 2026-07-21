#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
snapshot_dir="$root/docs/api"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for pkg in cmd cmd/config; do
  (cd "$root" && go doc "./$pkg" 2>/dev/null) > "$tmp/${pkg#*/}.txt"
done

if [[ "${1:-}" == "--update" ]]; then
  mkdir -p "$snapshot_dir"
  cp "$tmp/cmd.txt" "$snapshot_dir/cmd.txt"
  cp "$tmp/config.txt" "$snapshot_dir/config.txt"
  echo "API snapshots updated"
  exit 0
fi

diff -u "$snapshot_dir/cmd.txt" "$tmp/cmd.txt"
diff -u "$snapshot_dir/config.txt" "$tmp/config.txt"
echo "public API snapshot: PASS"
