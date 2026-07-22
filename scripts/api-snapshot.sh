#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
snapshot_dir="$root/docs/api"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

packages=(cmd cmd/config cmd/telemetry cmd/p2p)
for pkg in "${packages[@]}"; do
  name="${pkg#cmd/}"
  # -all records exported fields, methods and signatures; the package summary
  # alone collapses structs to `{ ... }` and misses breaking API changes.
  (cd "$root" && go doc -all "./$pkg" 2>/dev/null) | sed '${/^$/d;}' > "$tmp/$name.txt"
done

if [[ "${1:-}" == "--update" ]]; then
  mkdir -p "$snapshot_dir"
  cp "$tmp/cmd.txt" "$snapshot_dir/cmd.txt"
  cp "$tmp/config.txt" "$snapshot_dir/config.txt"
  cp "$tmp/telemetry.txt" "$snapshot_dir/telemetry.txt"
  cp "$tmp/p2p.txt" "$snapshot_dir/p2p.txt"
  echo "API snapshots updated"
  exit 0
fi

diff -u "$snapshot_dir/cmd.txt" "$tmp/cmd.txt"
diff -u "$snapshot_dir/config.txt" "$tmp/config.txt"
diff -u "$snapshot_dir/telemetry.txt" "$tmp/telemetry.txt"
diff -u "$snapshot_dir/p2p.txt" "$tmp/p2p.txt"
echo "public API snapshot: PASS"
