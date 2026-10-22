#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

node --test scripts/*/*.test.mjs 'test/smoke/**/*.test.mjs' test/smoke/local-lifecycle/dev-snapshot.mjs
go test ./...
