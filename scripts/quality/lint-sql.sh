#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
export ROOT="$root"
export SQLFLUFF="$root/scripts/quality/sqlfluff.sh"

# scripts/db/psqldef.sh owns the disposable database and runs the checks below
# against it, so nothing here can reach a shared database.
"$root/scripts/db/psqldef.sh" bash -s <<'CHECKS'
set -euo pipefail

violations="$ROOT/test/fixtures/sql/desired_schema_violations.sql"

catalog_check() {
  cat "$ROOT/scripts/quality/sql-lint-ignore.sql" "$ROOT/scripts/quality/sql-lint.sql" |
    docker exec -i "$POSTGRES_CONTAINER" psql -U pin_glide -d pin_glide -v ON_ERROR_STOP=1
}

schemalint_check() {
  npx --no-install schemalint
}

# `npx --no-install` runs whatever node_modules holds, so the installed
# Schemalint is checked against the version package.json pins.
check_schemalint_version() {
  local pinned installed
  pinned="$(node -p "require('$ROOT/package.json').devDependencies.schemalint")"
  installed="$(npx --no-install schemalint --version 2>/dev/null || true)"
  if [[ "$installed" != "$pinned" ]]; then
    printf 'schemalint %s is required; found %s. Run npm ci.\n' "$pinned" "${installed:-nothing}" >&2
    exit 1
  fi
}

# A clean run cannot show that these checks are able to fail, so the violating
# fixture must be rejected by every rule it trips before this command may pass.
expect_failure() {
  local description="$1" reports="$2"
  shift 2
  local missing=()
  local output report
  if output="$("$@" 2>&1)"; then
    printf '%s\n%s accepted the violating fixture.\n' "$output" "$description" >&2
    exit 1
  fi
  local IFS='|'
  for report in $reports; do
    grep -qF -- "$report" <<<"$output" || missing+=("$report")
  done
  if ((${#missing[@]})); then
    printf '%s\n%s failed without reporting %s.\n' "$output" "$description" "${missing[*]}" >&2
    exit 1
  fi
}

check_schemalint_version
catalog_check
schemalint_check
"$SQLFLUFF" lint db/schema/release db/schema/pin_glide db/migrations db/scripts

docker exec -i "$POSTGRES_CONTAINER" psql -U pin_glide -d pin_glide -v ON_ERROR_STOP=1 < "$violations"
expect_failure 'the catalog naming check' \
  'SQL lint found|primary key must be named|foreign key must be named|unique constraints are not allowed|index must be named' \
  catalog_check
expect_failure 'Schemalint' 'pin-glide-object-naming|pin-glide-comments' schemalint_check
expect_failure 'SQLFluff' 'CP01|LT02' "$SQLFLUFF" lint "$violations"
CHECKS
