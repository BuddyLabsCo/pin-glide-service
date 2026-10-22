# Pin Glide Compose deployment

This directory deploys one Pin Glide release from published images. The host runs Docker Compose and pulls all images from GHCR.

## Prerequisites

- Docker Engine and Compose v2 with `up --wait` and `config --format json`
- Bash, `jq`, `flock`, `awk`, `sed`, `grep`, `mktemp`, `date`, `ln`, `mv`, and `rm`
- A GitHub personal access token (classic) with `read:packages`

```sh
docker login ghcr.io -u your_github_user
```

## Prepare the host

```sh
mkdir -p /srv/pin-glide-service
cp deploy/compose/docker-compose.yml /srv/pin-glide-service/docker-compose.yml
cp deploy/compose/.env.example /srv/pin-glide-service/.env
cp install.sh /srv/pin-glide-service/install.sh
chmod 700 /srv/pin-glide-service/install.sh
chmod 600 /srv/pin-glide-service/.env
cd /srv/pin-glide-service
```

## Configure `.env`

- `COMPOSE_PROJECT_NAME`: stable installation name. Do not change it after the first deployment.
- `RELEASE`: target version as literal `X.Y.Z`, without `v`.
- `POSTGRES_USER`: PostgreSQL login. Keep it URL-safe.
- `POSTGRES_PASSWORD`: long random URL-safe password. Do not change it on an existing volume without rotating the database credentials separately.
- `WECHAT_APP_ID`, `WECHAT_APP_SECRET`: optional WeChat pair.
- `QQ_APP_ID`, `QQ_APP_KEY`: optional QQ pair.
- `BAILIAN_API_KEY`: required parent key for the approved Bailian Workspace.
- `PIN_GLIDE_SNAPSHOT_PATH`: existing host directory for automatic snapshots.
- `PIN_GLIDE_SNAPSHOT_TIME`: daily snapshot time in host-local `HH:MM`.
- `PIN_GLIDE_SNAPSHOT_KEEP`: number of completed snapshots to retain.

`RELEASE=0.1.0` maps to image tags `:0.1.0`. Both application images must exist before deployment.

## Initial deployment

```sh
mkdir -p /path/to/pin-glide-snapshots
./install.sh --version 0.1.0 --prep --initial-install
./install.sh --version 0.1.0 --initial-deploy
```

This pulls both release images, starts PostgreSQL, runs the migration, starts the API, and waits for health. It does not reset an existing database or volume.

## Upgrade or redeploy

Copy the new `docker-compose.yml` and `install.sh` into the deployment directory, keeping `.env`:

```sh
cp /path/to/new/docker-compose.yml ./docker-compose.yml
cp /path/to/new/install.sh ./install.sh
chmod 700 ./install.sh
```

Then run:

```sh
./install.sh --version 1.0.1 --prep
./install.sh --version 1.0.1 --full-release
```

`--prep` checks the recorded Database Release, rejects downgrades, backs up `.env`, and creates `attempt-1.0.1.json`.

`--full-release` runs in a fixed order:

1. Pull the API and database-tools images.
2. Stop the API, pause scheduled jobs, and wait for running jobs to finish.
3. Create a stopped-writer recovery backup.
4. Run the migration.
5. Start the API and restore the previous scheduled-job state.

Equal-version redeployment is allowed.

## Verify the deployment

```sh
docker compose exec -T api wget -q -O - http://127.0.0.1:8080/health
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT release_version FROM release.release_deploy_history ORDER BY id DESC LIMIT 1"
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d pin_glide -c \
  "SELECT jobname, schedule, active FROM cron.job ORDER BY jobname"
```

## Backups and cancellation

Create a manual backup:

```sh
./install.sh --version 1.0.1 --backup
```

Cancel an attempt before its migration succeeds:

```sh
./install.sh --version 1.0.1 --cancel
```

Cancellation restores `.env.bak`, pulls the previous images, restores `backup-1.0.1.dump` if migration started, and restarts the previous release. Cancellation is forbidden after migration succeeds.

`backup-X.Y.Z.dump` and `.env.bak` stay beside the Compose file. The installer never deletes them.

## Manual release actions

```sh
./install.sh --version 1.0.1 --pull
./install.sh --version 1.0.1 --stop-api
./install.sh --version 1.0.1 --migrate
./install.sh --version 1.0.1 --start-api
```

Use these in the same order as `--full-release`.

## Migration failure

The migration container is retained after failure. Inspect it before retrying:

```sh
docker logs pin-glide-service-db-tools
docker inspect --format '{{json .State}}' pin-glide-service-db-tools
jq . attempt-1.0.1.json
```

After confirming a failed, exited container, remove it and retry:

```sh
docker rm pin-glide-service-db-tools
./install.sh --version 1.0.1 --migrate
```

Migrations may commit partially. A failed migration does not mean the database is unchanged.

## Scheduled cleanup and snapshots

The API stop step holds all pg_cron jobs and waits up to 60 seconds for running SQL to finish. A job that does not stop blocks the release.

The packaged `pin-glide-retention-cleanup` job runs daily at 19:00 UTC. Retention covers Delivery records, credentials, refresh operations, Pairing Sessions, diagnostics, schema audits, and cron history. Configuration, authority, and release ledgers are not deleted by scheduled cleanup.

Automatic development snapshots:

```sh
./install.sh --enable-auto-snapshot
./install.sh --make-snapshot
./install.sh --disable-auto-snapshot
```

Snapshots are development data, not recovery backups. Recovery uses `backup-X.Y.Z.dump`.

## Installer reference

```sh
./install.sh --help
./install.sh --version 1.0.1
```

| Action | Operation |
| --- | --- |
| `--prep` | Prepare a release |
| `--backup` | Back up the database |
| `--pull` | Pull release images |
| `--stop-api` | Stop the API and hold scheduled work |
| `--migrate` | Run the migration |
| `--start-api` | Start the API |
| `--full-release` | Run the complete update |
| `--cancel` | Cancel before migration succeeds |
| `--initial-deploy` | Run the first deployment |
| `--enable-auto-snapshot` | Enable the daily snapshot |
| `--disable-auto-snapshot` | Disable the daily snapshot |
