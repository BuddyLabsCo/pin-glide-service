#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd -P)"
# The caller supplies only its dedicated local project, never deployment config.
project="${1:?Development Compose project required}"
[[ "$project" == pin-glide-dev-* ]] || { echo 'Expected a development project' >&2; exit 1; }

{
  printf '%s\n' 'BEGIN;' 'CREATE EXTENSION IF NOT EXISTS anon;' 'SELECT anon.init();'
  shopt -s nullglob
  for labels in "$root"/dev/anon/*.sql; do
    cat "$labels"
    printf '\n'
  done
  printf '%s\n' 'SET LOCAL anon.nosuperuser = false;' 'SELECT anon.anonymize_database();' 'COMMIT;'
} | env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES docker compose \
  --env-file /dev/null -p "$project" -f "$root/scripts/dev/docker-compose.yml" \
  exec -T postgres psql -X -U pin_glide -d pin_glide -v ON_ERROR_STOP=1
