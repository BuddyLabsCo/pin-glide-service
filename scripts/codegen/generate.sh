#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

tmp="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

export ROOT="$root" TMP="$tmp"

# Jet generation runs against the disposable database from scripts/db/psqldef.sh
# so it can never read a development or production schema.
"$root/scripts/db/psqldef.sh" bash -c '
set -euo pipefail
cd "$ROOT"

# Jet writes <path>/<database>/<schema>, so the database name the disposable
# database uses is part of the temporary output path. A schema with no tables,
# views or enums makes Jet succeed without writing anything, so the target is
# removed first and replaced only when Jet actually produced it. A schema that
# became empty therefore loses its stale generated objects instead of keeping
# them.
mkdir -p internal/db/jetgen
for schema in release pin_glide; do
  rm -rf "internal/db/jetgen/$schema"
  go tool jet -dsn="$DATABASE_URL" -schema="$schema" -path="$TMP/jetgen"
  jet_output="$TMP/jetgen/$PGDATABASE/$schema"
  if [[ -d "$jet_output" ]]; then
    mv "$jet_output" "internal/db/jetgen/$schema"
  fi
done
'

# Both published descriptions come from the API entrypoint in its offline
# modes, which read no configuration and start no listener.
mkdir -p generated/protocol
go run ./cmd/pin-glide-api -openapi > "$tmp/openapi.yaml"
mv "$tmp/openapi.yaml" generated/protocol/openapi.yaml
go run ./cmd/pin-glide-api -asyncapi > "$tmp/asyncapi.yaml"
mv "$tmp/asyncapi.yaml" generated/protocol/asyncapi.yaml
