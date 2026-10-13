#!/usr/bin/env bash

# Sets postgres and pg_port for an isolated database. Callers own its cleanup and
# must install their EXIT trap before calling, including startup failure paths.
start_disposable_postgres() {
  local root image
  root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  image="$(bash "$root/scripts/db/postgres-image.sh")"
  postgres="$(docker run --rm -d -p 127.0.0.1::5432 -e POSTGRES_USER=pin_glide -e POSTGRES_PASSWORD=pin_glide -e POSTGRES_DB=pin_glide "$image")"
  for attempt in {1..60}; do
    if docker exec "$postgres" pg_isready -h 127.0.0.1 -U pin_glide -d pin_glide >/dev/null 2>&1; then break; fi
    if (( attempt == 60 )); then echo 'PostgreSQL did not become ready' >&2; exit 1; fi
    sleep 1
  done
  pg_port="$(docker port "$postgres" 5432/tcp | head -n 1 | cut -d: -f2)"
}
