#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

node --test scripts/*/*.test.mjs test/smoke/dev-snapshot.mjs
go test ./...
