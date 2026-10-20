# Pin Glide Compose deployment

## Runtime and images

Production uses two private images from the same release commit and one derived
database image:

| Service | Image | Lifecycle |
| --- | --- | --- |
| `api` | `ghcr.io/buddylabsco/pin-glide-api:${RELEASE}` | One release tag per release |
| `db-tools` | `ghcr.io/buddylabsco/pin-glide-db-tools:${RELEASE}` | One release tag per release |
| `postgres` | `ghcr.io/buddylabsco/pin-glide-postgres:18-1.6.8` | Independent infrastructure tag |

Git tag / GitHub Release `v0.1.0` maps to literal `RELEASE=0.1.0` in `.env` and
to image tag `:0.1.0`, not `:v0.1.0`. Both release images must be published
before that release is ready; redeploying a version reuses its immutable
artifacts. The `publish-release` release workflow publishes both release images
from the merged release commit, the PostgreSQL/pg_cron image under its own tag,
and both validated contract descriptions. Hosts pull packaged artifacts and
never build the service.

`pin-glide-postgres` is built from `postgres.Dockerfile`: the official
PostgreSQL 18 Debian (trixie) distribution, pg_cron 1.6.8 from the PGDG apt
repository, and the preload/database configuration pg_cron requires
(`shared_preload_libraries = 'pg_cron'`, `cron.database_name = 'pin_glide'`).
The base image cannot load the extension at all, so Compose, the disposable
lint/codegen loader and the integration harness all use this one image. Local
development uses the separate `scripts/dev/postgres.Dockerfile` because it also
needs Anonymizer. Its tag is independent of `RELEASE`: ordinary releases reuse
the same running PostgreSQL container and do not recreate, upgrade or delete
its volume.
PostgreSQL stores data in the project-scoped `pin-glide-postgres-data` volume.
A PostgreSQL or pg_cron upgrade is a separate operation, not part of the
db-tools CLI.

## Prepare the host and files manually

Prerequisites: Docker Engine, Docker Compose v2 or later with `up --wait` and
`config --format json`, Bash, `jq`, `flock`, and common Linux tools such as
`awk`, `sed`, `grep`, `mktemp`, `date`, `ln`, `mv`, and `rm`. The account
running the installer needs Docker access and read access to all three private
GHCR packages:

```sh
docker login ghcr.io -u YOUR_GITHUB_USER
```

Supply a personal access token (classic) with at least `read:packages` at the
password prompt; GHCR package access does not accept fine-grained tokens. Never
put a token in the script, Compose file, or `.env`.

Place `docker-compose.yml`, `.env`, and a copy of the repository-root
`install.sh` in the same directory on the server:

```sh
mkdir -p /srv/pin-glide-dev
cp deploy/compose/docker-compose.yml /srv/pin-glide-dev/docker-compose.yml
cp deploy/compose/.env.example /srv/pin-glide-dev/.env
cp install.sh /srv/pin-glide-dev/install.sh
chmod 700 /srv/pin-glide-dev/install.sh
chmod 600 /srv/pin-glide-dev/.env
cd /srv/pin-glide-dev
```

Edit `.env` before running the installer:

- `COMPOSE_PROJECT_NAME`: a stable, unique name for this installation. Do not
  change it during a release; it selects the existing network and data volume.
- `RELEASE`: exactly one literal `X.Y.Z` assignment, without `v` or variable
  expansion. This selects both release images. Equal-version prep is allowed,
  and the example version in `.env.example` is not a claim of availability.
- `POSTGRES_USER`: the PostgreSQL login, initially `pin_glide`; use a URL-safe
  username. On a fresh volume it is created as a superuser and owns the
  `pin_glide` database, which is what lets it install and control pg_cron.
- `POSTGRES_PASSWORD`: replace the example with a long random URL-safe
  alphanumeric password before first deployment. It is embedded in the internal
  database URL, so keep it free of `@`, `:`, `/`, `#` and quotes. Both database
  credentials are required; there is no fallback. Changing them does not change
  users or passwords in an existing volume; rotate the database credentials
  separately.
- `WECHAT_APP_ID` and `WECHAT_APP_SECRET`: an approved WeChat Open Platform
  mobile application. Set both to enable WeChat login.
- `QQ_APP_ID` and `QQ_APP_KEY`: an approved QQ mobile application. Set both to
  enable QQ login through the server-side authorization-code flow.
- Neither provider pair is required for the `api` service to start. A missing
  or partial pair fails only when that provider is used.
- `BAILIAN_API_KEY`: the parent API key of the approved Bailian Beijing
  Workspace, which only authorizes temporary Sender keys for the enabled model
  row below. It is required for the `api` service to start in production and
  stays outside images, source files and logs.
- `PIN_GLIDE_SNAPSHOT_PATH`: an existing host directory writable by the cron
  user's UID/GID. It is used only when automatic deployment snapshots are
  enabled.
- `PIN_GLIDE_SNAPSHOT_TIME`: daily snapshot time in host-local `HH:MM` form.
- `PIN_GLIDE_SNAPSHOT_KEEP`: positive number of completed tool-owned snapshots
  to retain.

Temporary model credentials additionally need the enabled, non-secret model configuration row in PostgreSQL. The release Data Script `issue_33_1_model_configuration.sql` installs the approved Bailian Beijing Workspace and `qwen3.7-flash` model. The service reports `503 model_configuration_unavailable` when that row is missing or disabled. The stored ID is one lowercase Workspace label, never the endpoint host or a URL, so the parent key can only reach that Workspace. Verify the deployed row without printing the parent key:

```sh
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT workspace_id, model, enabled
   FROM pin_glide.model_configurations WHERE id = 'default'"
```

To change this configuration in a later release, add a new immutable Data Script. Do not edit a processed script. Setting `enabled = false` stops new issuance while the row stays for the record. The service cannot revoke keys the provider already issued.

The script must be run **from this prepared directory**. It does not download
itself, install dependencies, generate templates, or upgrade Compose files, and
it never executes `.env` with `source` or `eval`. Every Docker/Compose call from
the installer unsets inherited `RELEASE`, so the version in local `.env` wins
even if the invoking shell exports a different value.

The prepared development-acceptance Compose file publishes host port `33489`
and keeps the API listening on container port `8080`. The adopted LAN origins
are `http://192.168.10.17:33489` and `ws://192.168.10.17:33489`. Put your own
HTTPS/WSS reverse proxy in front of a production installation; the service
terminates TLS nowhere and exposes no other port. Multiple installations need
distinct project names and host ports.

## Smoke-test the development installation

`test/smoke/runner.mjs` checks the pinned release, immutable image IDs and digests, Compose project, database release, schema/data ledgers, packaged SQL order, model configuration, cleanup schedule, LAN port and health. `test/smoke/release-support.json` is the authority for upgrade origins. The runner rejects a target release without a declaration and any history transition the declaration does not allow. It refuses any target other than `local-server`, `/srv/pin-glide-dev`, Compose project `pin-glide-dev`, database `pin_glide`, and the adopted origins.

Run the read-only checks after a published release:

```sh
PIN_GLIDE_SMOKE_RELEASE=0.1.6 node test/smoke/runner.mjs --check
```

Run the guarded SQL bootstrap and cleanup only when you authorize writes to the development database:

```sh
PIN_GLIDE_SMOKE_RELEASE=0.1.6 \
PIN_GLIDE_SMOKE_WRITE_AUTHORIZATION=pin-glide-dev \
  node test/smoke/runner.mjs --integration-ready
```

The integration run first uses the installer to stop the API, hold and drain pg_cron, create a root-authorized stopped snapshot, restart the API, and restore the snapshot into a disposable database through the PostgreSQL container. It verifies the release ledgers, exact model/retention/cron configuration, pg_cron extension and quiescence, then drops the disposable database. It then creates two synthetic QQ-namespace Accounts and isolated active Sender credential families, generates fresh M/U/F credentials, stores only lowercase SHA-256 digests with normal three-hour or seven-day lifetimes, and exercises scope and expiry failures, ordered refresh, lost-response recovery, reuse isolation, ordinary logout, Account-switch cleanup and unrelated access through the deployed HTTP and WebSocket surface. It also requests a temporary Bailian key with a normal U credential, runs strict text-only OTP and no-OTP inference against the configured workspace, and checks authority, configuration, invalid-parent-key and connectivity failures. It verifies the runner's latest operation and rotated bundle, waits for the provider-reported temporary-key expiry, checks diagnostic redaction, restores every controlled fault, then removes the run-owned rows. Raw credentials, SMS text and OTP values stay in runner memory; evidence contains only redacted outcomes and safe request IDs.

The invalid-parent-key fault briefly recreates the API container from a temporary environment file, then restores the original `.env`. The connectivity fault adds and removes a scoped `/etc/hosts` entry inside the current API container, then waits long enough for the service to discard an idle provider connection. Both faults run under the environment lock and restore in `finally` blocks.

Both modes require `/mnt/share/workspace/snapshots/pin-glide` on the server and development workstation. `--check` requires both sides to expose the same completed snapshot, records the restore as blocked and exits `2`; it never reports acceptance. `--integration-ready` can start from an empty directory because it creates the stopped-writer backup used for recovery verification. The runner serializes environment-wide work with `/tmp/pin-glide-dev-smoke.lock` on `local-server`. Evidence defaults to the ignored `test/smoke/evidence/` directory with mode `0600`. Use `PIN_GLIDE_SMOKE_EVIDENCE` to select another path.

The explicit integration run covers successful fresh install, same-version retry, failed migration, retained-container reconciliation, cancel/restore and scheduler fault injection on a disposable real PostgreSQL/Compose stack:

```sh
node test/smoke/release-install.mjs --integration-ready
```

The `--check` mode only verifies prerequisites; it does not run those scenarios. The fake `63.x` images exercise installer state transitions and are not evidence for a supported historical release. The deployed runner validates upgrade origins against GitHub releases.

After both releases are published, exercise a fresh installation of the target and the declared upgrade on disposable local Compose projects with the real release images:

```sh
node test/smoke/published-install.mjs --integration-ready --from 0.1.5 --to 0.1.6
```

This command pulls through the existing workstation Docker credential context and removes both project volumes. Its `--check` mode validates the releases and support declaration without starting containers.

Real QQ/WeChat authorization, fresh-owner Sender security revocation and fresh-owner `recover_receiver` remain outside unattended smoke coverage. Provider integration tests cover those paths without labeling fixtures as real-provider login.

## Menu and CLI

```sh
./install.sh --help
./install.sh --version 1.0.1   # Numbered single-choice menu; no implicit prep.
```

| Menu | Operation | CLI action |
| --- | --- | --- |
| 1 | Prepare release | `--prep` |
| 2 | Back up the complete Pin Glide database | `--backup` |
| 3 | Pull both release images | `--pull` |
| 4 | Stop the api and hold scheduled work | `--stop-api` |
| 5 | Run migration | `--migrate` |
| 6 | Start the api | `--start-api` |
| 7 | Full release | `--full-release` |
| 8 | Cancel release | `--cancel` |
| 9 | Initial deployment | `--initial-deploy` |
| 10 | Enable automatic deployment snapshots | `--enable-auto-snapshot` |
| 11 | Disable automatic deployment snapshots | `--disable-auto-snapshot` |

Choose exactly one action per invocation. Release actions other than prep require
the matching active `attempt-X.Y.Z.json`; snapshot actions need no active
attempt and do not read or mutate release state. No action prepares an attempt
implicitly.
Calls in the same installation directory are mutually exclusive through
`flock` on the directory inode.

Upgrade prep (the default) queries PostgreSQL through `docker compose exec`
before writing any deployment files:

```sql
SELECT release_version FROM release.release_deploy_history ORDER BY id DESC LIMIT 1;
```

It compares the target numerically against this **Database Release**, allows
equal versions, and rejects downgrades. Database Release means Pin Glide Schema
Sync has completed apply and check; the history entry is appended before Data
Scripts, even if those scripts or later application startup fail. `.env` is not
a database-version fallback. A failed query, missing table, empty history, or
malformed version stops upgrade prep without changing `.env`, `.env.bak`, or
attempt files.

Before writing anything, prep also rejects any unarchived attempt, including one
for another version, or the fixed-name migration container, running or exited.
It backs up the **entire** `.env` to `.env.bak`, updates `RELEASE`, and records
progress in `attempt-X.Y.Z.json`. Preparing/prepared markers and atomic
temporary-file replacement protect interrupted writes. Do not overwrite the
original backup or remove an active attempt to bypass these checks; the attempt
JSON is the only workflow state.

## Initial deployment and upgrades

First deployment:

```sh
./install.sh --version 0.1.0 --prep --initial-install
./install.sh --version 0.1.0 --initial-deploy
```

The `--initial-install` modifier is valid only with prep. It records
`initial:true` in the very first attempt JSON write and skips the
database-version query without starting PostgreSQL. Menu item 1 asks the same
explicit first-install question. Initial deployment pulls both release images,
starts PostgreSQL and waits for readiness, runs migration, then starts the api
and waits for health. It does not reset existing databases or volumes.

Upgrade or equal-version redeployment:

```sh
./install.sh --version 1.0.1 --prep
./install.sh --version 1.0.1 --full-release
```

The full release order is fixed:

1. `docker compose pull api db-tools`.
2. `docker compose stop api`, then hold scheduled work and prove it is drained.
3. Create the stopped-writer recovery backup; abort on failure.
4. Run migration; abort on failure, without automatically starting the api.
5. `docker compose up -d --no-deps --wait api`, then restore the intended
   scheduled-job state.

For manual control, use the corresponding individual actions in that order.
Stopping the api stops admitting work and drains it within the five-second
bounded shutdown window; Compose allows ten seconds before SIGKILL.

## Scheduled database work and external writers

pg_cron runs scheduled SQL inside PostgreSQL, so stopping the api is not yet a
stopped-writer baseline. `--stop-api` (and the stop leg of `--full-release`)
does the following after the api is confirmed stopped:

1. Records the intended `cron.job` state of this installation in the attempt
   JSON (which jobs are active and which are deliberately inactive).
2. Sets every job inactive, so no new scheduled SQL starts.
3. Waits until `cron.job_run_details` has no `running` row and
   `pg_stat_activity` has no `pg_cron` backend, proving running scheduled SQL
   has exited. pg_cron cancels a job that is deactivated; the installer allows
   up to 60 seconds and then fails instead of continuing.

A failed query, an extension that appears after scheduling was controlled, an
active job, or SQL that never drains stops the release. That state is never
silently treated as either "running" or "stopped". `--backup` on a still-running
api is a valid online backup, but it is not a recovery baseline.

Every successful start restores the recorded flags, so the release preserves
the intended scheduling state through an upgrade, a same-version retry, and a
cancelled release, including a database restored from a frozen dump. Jobs
introduced by the release keep the state their own Data Script declared; the
installer only restores jobs it held.

Concrete roles, owners and intended state:

| Concern | Owner |
| --- | --- |
| pg_cron extension and `cron.job` control | `POSTGRES_USER` (`pin_glide`), superuser on a fresh volume |
| Scheduled job owner and database | `pin_glide` in database `pin_glide` (`cron.database_name`) |
| Job definitions and their intended active state | Data Scripts in the packaged `pin-glide-db-tools` image |
| Complete dump, database recreation and restore | `POSTGRES_USER` through the in-container `pg_dump`, `pg_restore`, `dropdb` and `createdb` clients |
| Maintenance connection for recreation | The `postgres` database inside the same container |

The packaged desired schema declares `CREATE EXTENSION pg_cron`, so Schema Sync
installs the extension this image preloads and rejects a database where it
disappeared. psqldef compares extensions by name and never upgrades their
version, so upgrading pg_cron itself is a separate operation.

The installer cooperates with every writer it can see. Writers outside this
installation (for example a second application pointed at the same database, or
an operator psql session holding a write transaction) must be stopped by you
before `--backup`/`--migrate`. Check them before the baseline:

```sh
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT pid, usename, application_name, state, left(query, 60) FROM pg_stat_activity
    WHERE datname = current_database() AND pid <> pg_backend_pid()"
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT jobid, jobname, active, database, username FROM cron.job ORDER BY jobid"
```

## Retention and scheduled cleanup

Every table the service implements has exactly one whole-table policy. No table
mixes durable rows with expiring rows, and no configuration, authority or
ledger table is a cleanup target:

| Table | Policy | Clock |
| --- | --- | --- |
| `pin_glide.delivery_deliveries` | seven days | `created_at`: the admission time a duplicate joins until the row itself is deleted |
| `pin_glide.identity_credentials` | seven days | `expires_at`: a live credential, and so a sliding family, is never deleted |
| `pin_glide.identity_refresh_operations` | seven days | `expires_at` of the consumed token, so an ordering guard is retained while that token is valid and goes with its parent |
| `pin_glide.pairing_sessions` | seven days | `expires_at`: the absolute end of the session and its restricted credential |
| `pin_glide.service_diagnostics` | thirty days | `occurred_at` |
| `release.release_schema_audits` | thirty days | `executed_at` |
| `cron.job_run_details` | thirty days | `end_time`, NULL while a run is still going, so a running job is never deleted |
| everything else | no scheduled deletion | Durable Account/device identity, Receiver credentials, active Pairings, model configuration, this cleanup configuration, both script ledgers, Database Release history and `cron.job` job definitions |

The `pin-glide-retention-cleanup` job runs daily at 19:00 UTC, which is 03:00
Asia/Shanghai the next day, and calls
`pin_glide.pin_glide_fn_cleanup_expired_rows(now())`. That routine walks
`pin_glide.retention_policies` in name order, quotes every identifier
separately and binds each cutoff, so a configuration row cannot carry SQL.
An interrupted run deletes nothing partially: every run filters the same
windows again from its own time, so there is no checkpoint, no staged restore
hook and no application-side fallback. Deduplication ends with the Delivery row
that carries it, not with a window: while that row exists a duplicate still
joins it, and only its deletion admits the same Sender, Message and Receiver
again as new work.

The Data Scripts own the job, and the release records each of them in
`release.release_data_scripts` and applies it only once: this release declares
`pin-glide-retention-cleanup` from its own script, with a stable name and owner
through `cron.schedule`, explicit active state through `cron.alter_job`, and
`cron.unschedule` for every other `pin-glide-` job name it no longer declares.
Declaring it again adds a job row an earlier release left missing and rewrites
the schedule and command of the row this owner holds, and the explicit
activation rewrites that row's run state. A row another role owns is not
something a declaration repairs, so a later release that changes the scheduled
set ships its own script instead of editing a recorded one. The cleanup
configuration and the dynamic routine are revoked from `PUBLIC`, which
PostgreSQL grants `EXECUTE` on a new function by default, so no other role
writes targets or runs cleanup.
Schema Sync reconciles neither `cron.job` rows nor extension versions, so verify
the job and the extension separately:

```sh
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT jobname, schedule, command, database, username, active FROM cron.job ORDER BY jobname"
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT extname, extversion FROM pg_extension WHERE extname = 'pg_cron'"
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT schema_name, table_name, retention_days, date_column FROM pin_glide.retention_policies ORDER BY 1, 2"
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT status, start_time, end_time, return_message FROM cron.job_run_details ORDER BY runid DESC LIMIT 5"
```

Deleting a row is not immediate physical erasure. PostgreSQL keeps its own
storage and WAL lifecycle, and backups and Docker/host logs stay
operator-managed: this section defines what the service deletes by itself, not
an erasure guarantee, and it is the whole of the service's own log retention.

## Migration failures, backups, and cancellation

The db-tools image contains the release and pin_glide desired schemas and
Migration Scripts, plus Data Scripts. Release metadata is upgraded first; each
schema uses psqldef apply followed by check, with separate audits. Successful
Pin Glide Schema Sync appends Database Release before Data Scripts execute.
Migrations may commit partially: failure does not imply an unchanged database.
They must change only objects inside the Pin Glide database, not login roles,
passwords, tablespaces, other databases, server-global settings, or database
identity.

The installer passes `PIN_GLIDE_RELEASE_VERSION=<target>` and the prepared
`PIN_GLIDE_RELEASE_INITIAL=true|false` explicitly to
`compose run db-tools migrate`. For a verified initial attempt, partial Schema
Sync or Data Script failure can be retried without a new prep. The installer persists
`.migration.initial_verified` only after reading the exact
`pin-glide-db-tools migrate: initial database accepted` line from this attempt's
retained container logs, and passes `PIN_GLIDE_RELEASE_INITIAL_RETRY=true` only
from that saved marker.

Migration runs in the foreground with `docker compose run --no-deps`, a fixed
name `<compose-project>-db-tools`, and **no `--rm`**. On success the installer
confirms a zero exit status, writes success to the active JSON, and only then
removes the container. Failure, interruption, or a failed JSON update retains
the container, logs, and exit status.

```sh
docker logs <compose-project>-db-tools
docker inspect --format '{{json .State}}' <compose-project>-db-tools
jq . attempt-1.0.1.json
```

Never launch concurrent migration or recovery while the fixed container is
running. If it exited successfully but JSON was not updated, rerun `--migrate`
to reconcile that success before deleting it. An uncertain outcome blocks
automatic recovery. After confirming a **failed, exited** container, remove it
manually and retry the same attempt with its original backup:

```sh
docker rm <compose-project>-db-tools
./install.sh --version 1.0.1 --migrate
```

To cancel an attempt whose migration has not succeeded:

```sh
./install.sh --version 1.0.1 --cancel
```

Cancellation restores `.env.bak`, pulls the original images, stops and holds
the api and its scheduled work, restores the database from the frozen
`backup-X.Y.Z.dump` if the migration ran, then starts and waits for the original
api and restores the intended scheduled-job state. **Once this attempt's
migration succeeds, cancellation is forbidden**, even if application startup
subsequently fails.

`backup-X.Y.Z.dump` and `.env.bak` live on the host beside the Compose file. The
custom-format `pg_dump -Fc` backup covers the complete Pin Glide database,
including schemas, data, sequences, functions, release ledgers and pg_cron job
rows. Recovery keeps PostgreSQL running, uses a maintenance connection to
recreate only the target database, and imports the full backup. Recovery
failure leaves the api stopped and preserves backups and progress. No valid
backup means no automatic database restoration, database deletion, or volume
deletion. This is a single-database recovery mechanism, not instance backup or
PITR.

Backup retention is manual: the installer never deletes backups or `.env.bak`,
so keep them wherever your retention policy requires before a later prep reuses
the same file names.

## Archival and a later repair release

Every successful start archives the active JSON as
`attempt-X.Y.Z-<timestamp>.json`, without overwriting an existing archive. A
successful start archives even when migration was skipped or failed. After
archival the same version can be prepared again, provided no migration
container remains.

**Explicit startup exception:** `--start-api` (menu 6) requires only the
matching active attempt as its release-progress gate. It does not require a
backup, prepared status, or successful migration, so an operator may
deliberately start with migration skipped or after a failed migration. That
weaker guarantee does not bypass full-release failure handling and does not
remove an unfinished baseline: `.env.bak`, the attempt JSON and any recovery
backup stay in place.

If migration succeeded but application startup keeps failing, an operator may
deliberately end the attempt and prepare a **higher** repair version after
confirming the migration result is recorded and no migration is running. Keep
the upgraded schema in place; there is no automatic rollback.

## Development snapshots

Deployment snapshots are raw logical data for local development, not recovery
backups. Create an existing host directory, add the three snapshot variables to
`.env`, then enable the daily entry:

```sh
mkdir -p /var/lib/pin-glide/snapshots
./install.sh --enable-auto-snapshot
./install.sh --make-snapshot
```

Enable validates the directory through a non-creating bind, the daily time and
positive retention count before replacing the current user's crontab. Only the
marked entry for this Compose project is changed. Disable with
`./install.sh --disable-auto-snapshot`.

Each run writes a private temporary custom-format `pg_dump`, rejects empty or
invalid archives with `pg_restore --list`, and atomically publishes
`pin-glide-snapshot-<UTC timestamp>-<suffix>.dump` with the invoking UID/GID and
`0600` mode. The dump excludes the `anon` extension and security labels but
keeps business, release and pg_cron job data. Retention removes only older
completed files matching this naming format; unrelated files, directories,
symlinks and `backup-X.Y.Z.dump` are preserved. A failed export publishes
nothing and prunes nothing.

The cron wrapper uses its own snapshot lock and does not require interactive
input, a target version or release-attempt state. It also does not read or
modify `attempt-*.json`. The stopped-writer `backup-X.Y.Z.dump` used by release
cancellation remains a separate, manually retained recovery artifact.

## Local development

```sh
export PIN_GLIDE_SNAPSHOT_PATH=/var/lib/pin-glide/snapshots
make dev            # destructive rebuild, restore, migrate, mask, then api
make dev-code-only  # rebuild/restart only the api; keep local data
make dev-stop       # stop and remove this checkout's local project and volume
```

`make dev` uses a checkout-derived Compose project, its own volume,
loopback-only `127.0.0.1:55432` for PostgreSQL and `127.0.0.1:8080` for the api,
and its own Database Release. It validates `PIN_GLIDE_SNAPSHOT_PATH` before
stopping an application or deleting local data. An invalid value causes no
destructive operation.

The launcher ignores deployment Compose files, `.env`, project overrides and
database URLs. It resets only its own project, restores the newest regular
`*.dump` by modification time with a lexical tie-breaker, migrates, applies
`dev/anon` labels transactionally, and starts the api only after every step
succeeds. An empty snapshot directory initializes a fresh database. The local
PostgreSQL image pins PostgreSQL 18, Anonymizer 3.2.3 and pg_cron 1.6.8 while
disabling pg_cron execution, so restored jobs and Data Script activations cannot
run during local work.

Local migrations record the checkout's `version.json`, not a synthetic `0.0.0`
Database Release. A restored deployment snapshot already carries its published
release history, so the old synthetic target would be a downgrade.

Inherited `PIN_GLIDE_WECHAT_APP_ID`, `PIN_GLIDE_WECHAT_APP_SECRET`,
`PIN_GLIDE_QQ_APP_ID`, `PIN_GLIDE_QQ_APP_KEY` and `PIN_GLIDE_BAILIAN_API_KEY`
are removed before the api starts, including `make dev-code-only`.
Provider-bound operations therefore fail their existing credential checks
before network I/O; this workflow does not provide a real provider acceptance
environment.

Ctrl-C stops application descendants and keeps PostgreSQL and its data.
`make dev-code-only` requires a successful prior preparation, rebuilds only the
api and does not invoke Docker, a snapshot, restore, migration or masking.
`make dev-stop` stops the application supervisors and removes only this
checkout's development containers, network and volume.
