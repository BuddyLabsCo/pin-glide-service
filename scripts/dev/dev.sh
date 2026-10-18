#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd -P)"
cd "$root"
mode="${1:-}"
case "$mode" in
  '')
    if [[ -z "${PIN_GLIDE_SNAPSHOT_PATH:-}" || ! -d "$PIN_GLIDE_SNAPSHOT_PATH" ||
      ! -r "$PIN_GLIDE_SNAPSHOT_PATH" || ! -x "$PIN_GLIDE_SNAPSHOT_PATH" ]]; then
      echo 'PIN_GLIDE_SNAPSHOT_PATH must name a readable existing directory' >&2
      exit 1
    fi
    ;;
  --code-only|--stop) ;;
  *) echo 'Usage: dev.sh [--code-only|--stop]' >&2; exit 1 ;;
esac

# Never inherit the deployment project, Compose files, credentials or DB URL.
project="pin-glide-dev-$(printf '%s' "$root" | sha256sum | cut -c 1-16)"
state_dir="${TMPDIR:-/tmp}/$project"
mkdir -p "$state_dir"
exec 9>"$state_dir/lifecycle.lock"
flock 9
compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u PIN_GLIDE_SNAPSHOT_PATH -u PIN_GLIDE_SNAPSHOT_TIME -u PIN_GLIDE_SNAPSHOT_KEEP \
    docker compose --env-file /dev/null -p "$project" \
    -f "$root/scripts/dev/docker-compose.yml" "$@"
}

# PID plus kernel start time prevents a stale file from killing a reused PID.
if [[ -f "$state_dir/apps.pid" ]]; then
  read -r old_pid old_start < "$state_dir/apps.pid" || true
  if [[ "${old_pid:-}" =~ ^[0-9]+$ && -n "${old_start:-}" && -r "/proc/$old_pid/stat" ]] &&
    [[ "$(awk '{print $22}' "/proc/$old_pid/stat")" == "${old_start:-}" ]]; then
    kill -TERM "$old_pid" 2>/dev/null || true
    for ((attempt = 0; attempt < 100; attempt++)); do
      [[ ! -e "$state_dir/apps.pid" ]] && break
      sleep 0.1
    done
    if [[ -e "$state_dir/apps.pid" ]]; then
      echo 'Previous development processes did not stop' >&2
      exit 1
    fi
  fi
  rm -f "$state_dir/apps.pid"
fi

if [[ "$mode" == --stop ]]; then
  compose down --volumes --remove-orphans
  rm -f "$state_dir/ready"
  exit
fi
if [[ "$mode" == --code-only && ! -f "$state_dir/ready" ]]; then
  echo 'Run make dev successfully before make dev-code-only' >&2
  exit 1
fi

export PIN_GLIDE_ENV=development
export PIN_GLIDE_DATABASE_URL='postgres://pin_glide:pin_glide@127.0.0.1:55432/pin_glide?sslmode=disable'
export PIN_GLIDE_API_ADDRESS=127.0.0.1:8080
unset PIN_GLIDE_WECHAT_APP_ID PIN_GLIDE_WECHAT_APP_SECRET PIN_GLIDE_BAILIAN_API_KEY

if [[ "$mode" != --code-only ]]; then
  rm -f "$state_dir/ready"
  # Select by mtime, with a lexical tie-breaker; filenames may contain spaces.
  snapshot=''
  shopt -s nullglob
  for candidate in "$PIN_GLIDE_SNAPSHOT_PATH"/*.dump; do
    [[ -f "$candidate" && ! -L "$candidate" ]] || continue
    if [[ -z "$snapshot" || "$candidate" -nt "$snapshot" ||
      ( ! "$snapshot" -nt "$candidate" && "$candidate" > "$snapshot" ) ]]; then
      snapshot="$candidate"
    fi
  done
  if [[ -n "$snapshot" && ! -r "$snapshot" ]]; then
    echo "Snapshot file is not readable: $snapshot" >&2
    exit 1
  fi
  compose down --volumes --remove-orphans
  compose up --detach --wait --build postgres
  if [[ -n "$snapshot" ]]; then
    echo "Restoring snapshot: $snapshot"
    compose exec -T postgres pg_restore --exit-on-error --no-owner --no-privileges \
      -U pin_glide -d pin_glide < "$snapshot"
  else
    echo 'No *.dump snapshot found; starting with an empty database'
  fi

  export PIN_GLIDE_RELEASE_VERSION="$(node -p "require('./version.json').version")"
  export PIN_GLIDE_RELEASE_INITIAL=false PIN_GLIDE_RELEASE_INITIAL_RETRY=false
  history_table="$(compose exec -T postgres psql -X -U pin_glide -d pin_glide -At -v ON_ERROR_STOP=1 \
    -c "SELECT to_regclass('release.release_deploy_history') IS NOT NULL")"
  case "$history_table" in
    f) export PIN_GLIDE_RELEASE_INITIAL=true ;;
    t)
      history_rows="$(compose exec -T postgres psql -X -U pin_glide -d pin_glide -At -v ON_ERROR_STOP=1 \
        -c 'SELECT EXISTS (SELECT 1 FROM release.release_deploy_history)')"
      case "$history_rows" in
        f) export PIN_GLIDE_RELEASE_INITIAL=true PIN_GLIDE_RELEASE_INITIAL_RETRY=true ;;
        t) ;;
        *) echo 'Invalid deployment history query result' >&2; exit 1 ;;
      esac
      ;;
    *) echo 'Invalid deployment history table query result' >&2; exit 1 ;;
  esac
  go run ./cmd/pin-glide-db-tools migrate
  "$root/scripts/dev/anonymize.sh" "$project"
  : > "$state_dir/ready"
fi

exec "$root/scripts/dev/apps.sh" "$state_dir"
