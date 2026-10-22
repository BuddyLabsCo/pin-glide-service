# Pin Glide Service

Go service for Sender authentication, Pairing, model credentials, and encrypted Delivery over HTTP and WSS.

## Prerequisites

- Go from `go.mod`
- Node.js 24 and `npm ci`
- Docker with Compose v2
- `uvx` for SQL checks

## Checks

```sh
make build           # compile every Go package
make test            # node helper tests, then go test ./...
make generate        # Jet types and both published descriptions
make check-contracts # pinned protocol validators, then the contract tests
make format-check    # gofmt check over handwritten Go sources
make lint            # pinned golangci-lint, then the source-size check
make lint-sql        # disposable PostgreSQL load, catalog, Schemalint, SQLFluff
make format-sql      # SQLFluff fixes for the desired schemas
```

PostgreSQL integration tests use a disposable local database:

```sh
go test -tags=integration ./test/integration/...
```

## Local development

```sh
export PIN_GLIDE_SNAPSHOT_PATH=/path/to/pin-glide-snapshots
make dev            # rebuild local data from the newest snapshot, mask, then run the api
```

`make dev` starts the API on `127.0.0.1:8080`.

```sh
make dev-code-only  # rebuild/restart only the api, keeping local data
make dev-stop       # stop and remove this checkout's local container and volume
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
