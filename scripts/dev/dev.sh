#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

# Development refuses production configuration and uses its own Compose project,
# volume, loopback port, origin and Database Release, so it cannot touch a
# deployment installation. Inherited deployment variables are dropped rather
# than trusted: only the values below reach the local processes.
case "${PIN_GLIDE_ENV:-}" in
  ''|development) ;;
  *) echo 'development only: unset PIN_GLIDE_ENV or set it to development' >&2; exit 1 ;;
esac
unset PIN_GLIDE_DATABASE_URL PIN_GLIDE_API_ADDRESS COMPOSE_PROJECT_NAME RELEASE

build_dir="${TMPDIR:-/tmp}/pin-glide-dev"
mkdir -p "$build_dir"
# Build (not "go run": Go 1.27 strips "go run" executables, so delve cannot attach).
gcflags=(-gcflags='all=-N -l')
dlv_bin="${DLV_BIN:-$(command -v dlv || echo "$HOME/go/bin/dlv")}"
[[ -x "$dlv_bin" ]] || dlv_bin=""

# Production keeps PostgreSQL private; the local release CLI and api need a
# loopback port. Only PostgreSQL runs here, and it is the locally built derived
# image: development never needs a published artifact.
image="$(bash "$root/scripts/db/postgres-image.sh")"
compose() {
  RELEASE=0.0.0 POSTGRES_USER=pin_glide POSTGRES_PASSWORD=pin_glide docker compose \
    --project-name pin-glide-dev --env-file /dev/null \
    -f "$root/deploy/compose/docker-compose.yml" -f - "$@" <<YAML
services:
  postgres:
    image: $image
    ports:
      - "127.0.0.1:5432:5432"
YAML
}

api_pid=
cleanup() {
  if [[ -n "$api_pid" ]]; then kill "$api_pid" 2>/dev/null || true; fi
  compose stop postgres >/dev/null 2>&1 || true
}
trap cleanup EXIT

compose up --detach --force-recreate --wait postgres

export PIN_GLIDE_ENV=development
export PIN_GLIDE_DATABASE_URL=postgres://pin_glide:pin_glide@127.0.0.1:5432/pin_glide?sslmode=disable
export PIN_GLIDE_API_ADDRESS=127.0.0.1:8080
# Local development records its own Database Release; no published version is
# implied, and the disposable volume can be deleted at any time.
export PIN_GLIDE_RELEASE_VERSION=0.0.0
export PIN_GLIDE_RELEASE_INITIAL=false PIN_GLIDE_RELEASE_INITIAL_RETRY=false
# Local development explicitly bootstraps an empty deployment history. Query
# failures are fatal, never an implicit first installation against an unknown DB.
history_table="$(compose exec -T postgres psql -U pin_glide -d pin_glide -At -v ON_ERROR_STOP=1 \
  -c "SELECT to_regclass('release.release_deploy_history') IS NOT NULL")"
case "$history_table" in
  f) export PIN_GLIDE_RELEASE_INITIAL=true ;;
  t)
    history_rows="$(compose exec -T postgres psql -U pin_glide -d pin_glide -At -v ON_ERROR_STOP=1 \
      -c "SELECT EXISTS (SELECT 1 FROM release.release_deploy_history)")"
    case "$history_rows" in
      f) export PIN_GLIDE_RELEASE_INITIAL=true PIN_GLIDE_RELEASE_INITIAL_RETRY=true ;;
      t) ;;
      *) echo 'Invalid deployment history query result' >&2; exit 1 ;;
    esac
    ;;
  *) echo 'Invalid deployment history table query result' >&2; exit 1 ;;
esac

go build "${gcflags[@]}" -o "$build_dir/pin-glide-release" ./cmd/pin-glide-release
"$build_dir/pin-glide-release" migrate

go build "${gcflags[@]}" -o "$build_dir/pin-glide-api" ./cmd/pin-glide-api
if [[ -n "$dlv_bin" ]]; then
  "$dlv_bin" exec "$build_dir/pin-glide-api" \
    --headless --continue --accept-multiclient \
    --listen="127.0.0.1:2345" --api-version=2 &
else
  "$build_dir/pin-glide-api" &
fi
api_pid=$!

wait -n
