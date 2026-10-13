#!/usr/bin/env bash
set -euo pipefail

# The single pin for SQLFluff, so `make lint-sql` and `make format-sql` cannot
# drift onto different versions.
exec uvx --from sqlfluff==4.3.0 sqlfluff "$@"
