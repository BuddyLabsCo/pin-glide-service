#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
export ROOT="$root"

"$root/scripts/db/psqldef.sh" bash -c '
  set -euo pipefail
  {
    printf "%s\n" \
      "CREATE TEMP TABLE IF NOT EXISTS lint_ignore_object (identifier text PRIMARY KEY);"
    cat "$ROOT/scripts/quality/sql-lint-ignore.sql" "$ROOT/scripts/quality/sql-lint.sql"
  } | docker exec -i "$POSTGRES_CONTAINER" psql -U pin_glide -d pin_glide -v ON_ERROR_STOP=1
  npx --no-install schemalint
  uvx --from sqlfluff==4.3.0 sqlfluff lint db/schema/release db/schema/pin_glide db/migrations db/scripts
'
