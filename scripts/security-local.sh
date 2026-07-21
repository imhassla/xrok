#!/usr/bin/env bash
set -euo pipefail

echo "[security] go vet"
go vet ./...
if command -v govulncheck >/dev/null 2>&1; then
  echo "[security] govulncheck"
  govulncheck ./...
else
  echo "[security] govulncheck not installed; skipped (install golang.org/x/vuln/cmd/govulncheck)" >&2
fi
echo "[security] module graph"
go list -m all >/dev/null
echo "[security] source gate passed"
