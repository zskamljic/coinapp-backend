#!/bin/sh
# Builds the single static binary.
#
# CGO_ENABLED=0 so the result has no libc dependency and runs on any Linux VPS.
# -s -w drops the symbol table and DWARF debug info.
set -eu

cd "$(dirname "$0")/.."

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o coinapp-backend ./cmd/coinapp-backend

echo "built ./coinapp-backend"
ls -lh coinapp-backend
file coinapp-backend | cut -c1-120

echo
echo "Run the test suite with:  go test ./..."
echo "Preview a sync safely:   ./coinapp-backend --dry-run --once"
