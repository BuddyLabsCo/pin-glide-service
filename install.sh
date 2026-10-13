#!/usr/bin/env bash
# Copy this file beside an operator-prepared Compose file and .env.
set -euo pipefail
umask 077
export LC_ALL=C

fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'HELP'
Usage: ./install.sh --version X.Y.Z [ACTION]
Without ACTION, show the numbered menu (never prepares automatically).
  --prep             Query Database Release, back up .env, prepare an upgrade
  --initial-install  With --prep only: explicitly prepare a first installation
  --backup           Back up the complete Pin Glide database
  --pull             Pull both release images
  --stop-api         Stop the api service and hold its scheduled database work
  --migrate          Run the fixed-name migration container
  --start-api        Start the api service; only an active attempt is required
  --full-release     Pull, stop, back up, migrate, start
  --cancel           Restore the old environment and, if needed, database
  --initial-deploy   Pull, start PostgreSQL, migrate, start
  --help             Show this help
HELP
}
valid_version() { [[ "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }
not_older() {
  local -a target previous
  local i
  IFS=. read -r -a target <<< "$1"; IFS=. read -r -a previous <<< "$2"
  for i in 0 1 2; do
    ((${#target[i]} > ${#previous[i]})) && return 0
    ((${#target[i]} < ${#previous[i]})) && return 1
    [[ ${target[i]} > ${previous[i]} ]] && return 0
    [[ ${target[i]} < ${previous[i]} ]] && return 1
  done
  return 0
}
compose() { env -u RELEASE docker compose --env-file .env -f "$compose_file" "$@"; }
docker_cmd() { env -u RELEASE docker "$@"; }
temps=()
temporary() { temp=$(mktemp ./.pin-glide-tmp.XXXXXXXX); temps+=("$temp"); }
trap 'rm -f -- "${temps[@]}"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
update() {
  temporary
  jq "$@" "$attempt" > "$temp"
  mv -fT -- "$temp" "$attempt"
}
environment_version() {
  [[ $(grep -Ec '^[[:space:]]*(export[[:space:]]+)?RELEASE[[:space:]]*=' .env) == 1 ]] || fail '.env must contain exactly one RELEASE assignment'
  previous=$(sed -nE "s/^[[:space:]]*(export[[:space:]]+)?RELEASE[[:space:]]*=[[:space:]]*['\"]?([0-9]+\.[0-9]+\.[0-9]+)['\"]?[[:space:]]*(#.*)?$/\2/p" .env)
  valid_version "$previous" || fail '.env RELEASE must be a literal X.Y.Z version'
}
container_exists() {
  local names
  names=$(docker_cmd container ls --all --filter "name=^/${container}$" --format '{{.Names}}') || fail 'cannot inspect migration containers'
  [[ -n "$names" ]]
}
query() {
  compose exec -T postgres sh -ec 'exec psql -X -v ON_ERROR_STOP=1 --username="$POSTGRES_USER" --dbname="$POSTGRES_DB" -Atc "$1"' pin-glide-install "$1"
}
# pg_cron executes scheduled SQL inside PostgreSQL, so a stopped api container is
# not yet a stopped-writer baseline. Installation records the intended job state,
# holds every job inactive, and proves running job SQL has exited.
cron_extension() {
  query "SELECT count(*) FROM pg_extension WHERE extname = 'pg_cron'" || fail 'cannot inspect pg_cron'
}
cron_drained() {
  local poll
  # shortcut: a 60-second drain window, widen it when a scheduled job legitimately runs longer.
  for poll in {1..60}; do
    if [[ "$(query "SELECT (SELECT count(*) FROM cron.job_run_details WHERE status = 'running') + (SELECT count(*) FROM pg_stat_activity WHERE application_name = 'pg_cron')")" == 0 ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}
hold_schedule() {
  local jobs
  if [[ "$(cron_extension)" == 0 ]]; then
    update '.cron = {status:"absent"}'
    return
  fi
  # Record the intended flags once; a repeated hold must not persist them paused.
  if ! jq -e '.cron.jobs' "$attempt" >/dev/null; then
    jobs=$(query "SELECT coalesce(json_object_agg(jobid, active), '{}'::json) FROM cron.job") || fail 'cannot read scheduled jobs'
    update --argjson jobs "$jobs" '.cron.jobs = $jobs'
  fi
  query 'UPDATE cron.job SET active = false' >/dev/null || fail 'cannot deactivate scheduled jobs'
  update '.cron.status = "paused"'
  cron_drained || fail 'running scheduled SQL has not exited'
}
schedule_held() {
  if [[ "$(cron_extension)" == 0 ]]; then
    jq -e '.cron.status == "absent"' "$attempt" >/dev/null || fail 'pg_cron state is unknown: scheduled writers were never verified'
    return 0
  fi
  jq -e '.cron.status == "paused"' "$attempt" >/dev/null || fail 'pg_cron state is unknown: scheduled writers were never held'
  [[ "$(query "SELECT count(*) FROM cron.job WHERE active")" == 0 ]] || fail 'scheduled jobs are still active'
  cron_drained || fail 'running scheduled SQL has not exited'
}
release_schedule() {
  local jobs active inactive
  jq -e '.cron.status == "paused"' "$attempt" >/dev/null || return 0
  [[ "$(cron_extension)" == 1 ]] || fail 'pg_cron is missing; scheduled jobs stay inactive'
  # Restore only the flags this attempt held; jobs the release added keep the
  # state their own Data Script declared.
  jobs=$(jq -c '.cron.jobs // {}' "$attempt")
  active=$(jq -r '[to_entries[] | select(.value) | .key] | join(",")' <<< "$jobs")
  inactive=$(jq -r '[to_entries[] | select(.value | not) | .key] | join(",")' <<< "$jobs")
  [[ -z "$active" ]] || query "UPDATE cron.job SET active = true WHERE jobid IN ($active)" >/dev/null || fail 'cannot resume scheduled jobs'
  [[ -z "$inactive" ]] || query "UPDATE cron.job SET active = false WHERE jobid IN ($inactive)" >/dev/null || fail 'cannot restore the scheduled-job state'
  update '.cron.status = "restored"'
}
prep() {
  local file database_version=
  for file in attempt-*.json; do
    [[ "$file" =~ ^attempt-[0-9]+\.[0-9]+\.[0-9]+\.json$ ]] && fail "active attempt exists: $file"
  done
  container_exists && fail "migration container $container exists; inspect and remove it manually"
  environment_version
  if [[ "$initial_install" != true ]]; then
    database_version=$(query 'SELECT release_version FROM release.release_deploy_history ORDER BY id DESC LIMIT 1') || fail 'cannot query Database Release; upgrade preparation stopped'
    valid_version "$database_version" || fail 'database must have a valid recorded Database Release'
    not_older "$version" "$database_version" || fail 'ordinary downgrades below Database Release are not allowed'
  fi
  temporary
  jq -n --arg target "$version" --arg previous "$previous" --arg database_version "$database_version" --argjson initial "$initial_install" '{target:$target, previous:$previous, database_version:(if $initial then null else $database_version end), initial:$initial, phase:"preparing", environment_backup:{path:".env.bak",valid:false}, migration:{status:"not_started"}}' > "$temp"
  mv -T -- "$temp" "$attempt"
  temporary; cat .env > "$temp"; mv -fT -- "$temp" .env.bak
  update '.environment_backup.valid = true'
  temporary
  awk -v v="$version" '/^[ \t]*(export[ \t]+)?RELEASE[ \t]*=/ { print "RELEASE=" v; next } { print }' .env > "$temp"
  mv -fT -- "$temp" .env
  update '.phase = "prepared"'
  printf 'Prepared %s\n' "$attempt"
}
require_prepared() {
  jq -e --arg v "$version" '.phase == "prepared" and .target == $v' "$attempt" >/dev/null || fail 'attempt is not prepared'
  environment_version
  [[ "$previous" == "$version" ]] || fail '.env RELEASE does not match the active attempt'
}
applications_stopped() {
  local running
  running=$(compose ps --status running --services api) || fail 'cannot check application state'
  [[ -z "$running" ]]
}
# True only when the api and every scheduled writer are verified stopped. An
# unknown scheduler state fails instead of being read as either answer.
writers_stopped() {
  applications_stopped || return 1
  jq -e '.operations.stopped == true' "$attempt" >/dev/null || return 1
  schedule_held
}
pull_images() {
  compose pull api migrate
  update '.operations.pulled = true'
}
stop_api() {
  compose stop api
  applications_stopped || fail 'the api is still running'
  hold_schedule
  update '.operations.stopped = true'
}
backup_database() {
  local baseline=false
  [[ $(jq -r '.migration.status // "not_started"' "$attempt") == not_started ]] || fail 'backup is frozen after migration starts'
  if jq -e '.operations.stopped == true' "$attempt" >/dev/null && writers_stopped; then baseline=true; fi
  update '.backup.valid = false'
  temporary
  compose exec -T postgres sh -ec 'exec pg_dump -Fc --username="$POSTGRES_USER" --dbname="$POSTGRES_DB"' > "$temp"
  [[ -s "$temp" ]] || fail 'database dump is empty'
  compose exec -T postgres sh -ec 'exec pg_restore --list' < "$temp" >/dev/null
  if [[ "$baseline" == true ]] && ! writers_stopped; then fail 'writers restarted during the backup'; fi
  mv -fT -- "$temp" "backup-${version}.dump"
  update --arg path "backup-${version}.dump" --argjson stopped "$baseline" '.backup = {path:$path,valid:true,stopped:$stopped}'
}
reconcile_migration() {
  local status details exit_code logs
  status=$(jq -r '.migration.status // "not_started"' "$attempt")
  if ! container_exists; then
    [[ "$status" != running ]] || fail 'migration outcome is unknown: recorded as running but its container is missing'
    return
  fi
  details=$(docker_cmd container inspect "$container")
  jq -e --arg v "$version" '.[0].Config.Labels["pin-glide.release"] == $v' <<< "$details" >/dev/null || fail 'migration container does not belong to this attempt version'
  [[ $(jq -r '.[0].State.Status' <<< "$details") == exited ]] || fail 'migration is running or its outcome is unknown; do not migrate or restore concurrently'
  [[ "$status" != not_started ]] || fail 'unexpected migration container; outcome is not recorded by this attempt'
  exit_code=$(jq -er '.[0].State.ExitCode | numbers' <<< "$details")
  if jq -e '.initial == true' "$attempt" >/dev/null; then
    logs=$(docker_cmd logs "$container" 2>&1) || fail 'cannot read migration logs to verify initial installation'
    if grep -Fxq 'pin-glide-release migrate: initial database accepted' <<< "$logs"; then
      update '.migration.initial_verified = true'
    fi
  fi
  if [[ "$exit_code" == 0 ]]; then
    update '.migration += {status:"succeeded",exit_code:0}'
    docker_cmd container rm "$container"
  else
    [[ "$status" != succeeded ]] || fail 'migration JSON and container results disagree'
    update --argjson code "$exit_code" '.migration += {status:"failed",exit_code:$code}'
  fi
}
migrate() {
  local initial retry
  require_prepared
  reconcile_migration
  [[ $(jq -r '.migration.status' "$attempt") != succeeded ]] || return 0
  container_exists && fail "inspect and manually remove $container before retrying the same attempt"
  if jq -e '.initial == true' "$attempt" >/dev/null; then
    hold_schedule
  else
    writers_stopped || fail 'stop the api and its scheduled writers before migration'
    jq -e '.backup.valid == true and .backup.stopped == true' "$attempt" >/dev/null || fail 'a successful post-stop recovery backup is required'
    [[ -s $(jq -r '.backup.path' "$attempt") ]] || fail 'recovery backup is missing'
  fi
  # A retry changes the database again, invalidating any earlier partial-cancel restore.
  initial=$(jq -r '.initial == true' "$attempt")
  retry=$(jq -r '.initial == true and .migration.initial_verified == true' "$attempt")
  update '.migration.status = "running" | del(.migration.exit_code, .restore)'
  # Docker retains both successful and failed containers until JSON is durable.
  compose run --name "$container" --label "pin-glide.release=$version" --no-deps \
    -e "PIN_GLIDE_RELEASE_VERSION=$version" -e "PIN_GLIDE_RELEASE_INITIAL=$initial" \
    -e "PIN_GLIDE_RELEASE_INITIAL_RETRY=$retry" migrate || true
  reconcile_migration
  [[ $(jq -r '.migration.status' "$attempt") == succeeded ]] || fail "migration failed; retained $container and its logs"
}
full_release() {
  require_prepared
  reconcile_migration
  pull_images
  stop_api
  if [[ $(jq -r '.migration.status' "$attempt") == not_started ]]; then backup_database; fi
  migrate
  start_api
}
initial_deploy() {
  require_prepared
  jq -e '.initial == true' "$attempt" >/dev/null || fail 'initial deployment requires --prep --initial-install'
  reconcile_migration
  pull_images
  compose up -d --no-deps --wait postgres
  migrate
  start_api
}
restore_database() {
  local backup
  jq -e '.backup.valid == true and .backup.stopped == true' "$attempt" >/dev/null || fail 'no valid post-stop recovery baseline; the database will not be deleted'
  backup=$(jq -r '.backup.path' "$attempt")
  [[ -s "$backup" ]] || fail 'recovery backup is missing; the database will not be deleted'
  if ! jq -e '.restore.database == true' "$attempt" >/dev/null; then
    compose exec -T postgres sh -ec 'exec pg_restore --list' < "$backup" >/dev/null
    update '.restore.database = false'
    # Maintenance connection rebuilds ONLY the Pin Glide database. PostgreSQL stays up.
    compose exec -T postgres sh -ec '
      case "$POSTGRES_DB" in ""|postgres|template0|template1) echo "unsafe target database" >&2; exit 1;; esac
      dropdb --username="$POSTGRES_USER" --maintenance-db=postgres --force --if-exists "$POSTGRES_DB"
      createdb --username="$POSTGRES_USER" --maintenance-db=postgres --owner="$POSTGRES_USER" "$POSTGRES_DB"
      exec pg_restore --exit-on-error --username="$POSTGRES_USER" --dbname="$POSTGRES_DB"
    ' < "$backup"
    update '.restore.database = true'
  fi
}
cancel_release() {
  local status
  reconcile_migration
  status=$(jq -r '.migration.status // "not_started"' "$attempt")
  [[ "$status" != succeeded ]] || fail 'a successful migration cannot be cancelled, even if application startup failed'
  jq -e '.environment_backup.valid == true and .environment_backup.path == ".env.bak"' "$attempt" >/dev/null || fail 'no valid environment backup'
  [[ -f .env.bak ]] || fail '.env.bak is missing'
  temporary; cat .env.bak > "$temp"; mv -fT -- "$temp" .env
  update '.restore.environment = true'
  pull_images
  stop_api
  if [[ "$status" == failed ]]; then restore_database; fi
  start_api
}
archive() {
  local stamp destination suffix=0
  stamp=$(date -u +%Y%m%dT%H%M%S)
  destination="attempt-${version}-${stamp}.json"
  while [[ -e "$destination" ]]; do
    suffix=$((suffix + 1)); destination="attempt-${version}-${stamp}-${suffix}.json"
  done
  # A hard-link rename never overwrites an archive, even on timestamp collisions.
  ln -T -- "$attempt" "$destination"
  rm -- "$attempt"
  printf 'Archived %s\n' "$destination"
}
start_api() {
  compose up -d --no-deps --wait api
  release_schedule
  update '.operations.started = true'
  archive
}
menu() {
  printf '%s\n' '1. Prepare release' '2. Back up database' '3. Pull images' '4. Stop the api' '5. Migrate' '6. Start the api' '7. Full release' '8. Cancel release' '9. Initial deployment'
  local choice
  read -r -p 'Choose one action (1-9): ' choice || fail 'an explicit menu choice is required'
  [[ "$choice" =~ ^[1-9]$ ]] || fail 'choose a number from 1 through 9'
  local actions=(prep backup pull stop-api migrate start-api full-release cancel initial-deploy)
  action=${actions[choice-1]}
  if [[ "$action" == prep && "$initial_install" != true ]]; then
    read -r -p 'First installation (no Database Release)? [y/N]: ' choice || choice=
    case "$choice" in
      y|Y) initial_install=true ;;
      n|N|'') ;;
      *) fail 'choose y for initial installation or n for upgrade' ;;
    esac
  fi
}

version= action= initial_install=false
while (($#)); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --initial-install) initial_install=true ;;
    --version) (($# >= 2)) || fail '--version needs X.Y.Z'; version=$2; shift ;;
    --prep|--backup|--pull|--stop-api|--migrate|--start-api|--full-release|--cancel|--initial-deploy)
      [[ -z "$action" ]] || fail 'choose exactly one action'; action=${1#--} ;;
    *) fail "unknown argument: $1" ;;
  esac
  shift
done
[[ -n "$version" ]] || { read -r -p 'Target version (X.Y.Z): ' version || fail 'target version required'; }
valid_version "$version" || fail 'target version must be X.Y.Z (without v)'
for command in docker jq flock mktemp; do command -v "$command" >/dev/null || fail "required command is missing: $command"; done
[[ -f .env ]] || fail 'prepare .env in the current directory first'
compose_file=
for file in compose.yaml compose.yml docker-compose.yaml docker-compose.yml; do
  if [[ -f "$file" ]]; then compose_file=$file; break; fi
done
[[ -n "$compose_file" ]] || fail 'prepare a Compose file in the current directory first'
# Lock the directory inode; child Docker clients retain the lock until they finish.
exec 9< .
flock -n 9 || fail 'another action is running in this directory'
[[ -n "$action" ]] || menu
[[ "$initial_install" != true || "$action" == prep ]] || fail '--initial-install is only valid with --prep'
attempt="attempt-${version}.json"
[[ "$action" == prep || -f "$attempt" ]] || fail "missing $attempt; explicitly prepare first"
project=$(compose config --format json | jq -er '.name')
[[ "$project" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || fail 'invalid Compose project name'
container="${project}-migrate"
case "$action" in
  prep) prep ;;
  backup) backup_database ;;
  pull) pull_images ;;
  stop-api) stop_api ;;
  migrate) migrate ;;
  start-api) start_api ;;
  full-release) full_release ;;
  cancel) cancel_release ;;
  initial-deploy) initial_deploy ;;
esac
