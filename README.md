# Pin Glide Service

Go service for Sender authentication, Pairing, model credentials, and encrypted Delivery over HTTP and WSS.

## Prerequisites

- Go from `go.mod`
- Node.js 24 and `npm ci`
- Docker with Compose v2
- `uvx` for SQL checks

## Checks

```sh
make build
make test
make format-check
make lint
make lint-sql
make check-contracts
make generate
```

PostgreSQL integration tests use a disposable local database:

```sh
go test -tags=integration ./test/integration/...
```

## Local development

```sh
export PIN_GLIDE_SNAPSHOT_PATH=/path/to/pin-glide-snapshots
make dev
```

`make dev` rebuilds this checkout's local database, restores the newest snapshot, migrates, masks data, and starts the API on `127.0.0.1:8080`.

```sh
make dev-code-only  # restart the API, keep local data
make dev-stop       # remove this checkout's containers and volume
```

## Smoke test

The smoke runner creates or updates `/srv/pin-glide-smoke`, patches its `.env`, installs the requested release, and runs the full local acceptance journey.

```sh
PIN_GLIDE_SMOKE_RELEASE=X.Y.Z \
  node test/smoke/runner.mjs --integration-ready
```

Requirements: Docker, Go, GHCR access to the released images, a Bailian API key, and passwordless `sudo`.

Additional checks:

```sh
node test/smoke/installation/release-install.mjs --integration-ready
node test/smoke/installation/published-install.mjs --integration-ready --from X.Y.Z --to X.Y.Z
node test/smoke/local-lifecycle/dev-snapshot.mjs --docker
```

See `test/smoke/README.md` for coverage and evidence.

## Deployment

Deployment uses the packaged images and `install.sh`. See `deploy/compose/README.md` for host preparation, initial deployment, upgrades, and recovery.

## Release publication

Merge the `auto-release` pull request with squash or rebase. The `publish-release` workflow re-runs CI, publishes both release images and the PostgreSQL image, then creates the GitHub Release and contract assets.
