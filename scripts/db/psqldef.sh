#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
if (($# == 0)); then
  echo "usage: scripts/db/psqldef.sh command [args...]" >&2
  exit 2
fi

tmp="$(mktemp -d)"
postgres=""
cleanup() {
  if [[ -n "$postgres" ]]; then docker rm -f "$postgres" >/dev/null 2>&1 || true; fi
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

source "$root/scripts/db/postgres.sh"
start_disposable_postgres
export POSTGRES_CONTAINER="$postgres"
export PGHOST=127.0.0.1 PGPORT="$pg_port" PGUSER=pin_glide PGPASSWORD=pin_glide PGDATABASE=pin_glide
export DATABASE_URL="postgres://pin_glide:pin_glide@127.0.0.1:${pg_port}/pin_glide?sslmode=disable"

for schema in release pin_glide; do
  find "$root/db/schema/$schema" -type f -name '*.sql' -print0 | LC_ALL=C sort -z | xargs -0 cat > "$tmp/schema.sql"
  printf 'target_schema: %s\n' "$schema" > "$tmp/config.yml"
  go tool psqldef -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" "$PGDATABASE" --config "$tmp/config.yml" --apply < "$tmp/schema.sql"
done

cd "$root"
"$@"
