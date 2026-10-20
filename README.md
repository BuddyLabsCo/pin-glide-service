# Pin Glide Service

The service side of Pin Glide: one Go process that authenticates Android
Senders, authorizes Pairings, supplies model credentials, relays live ciphertext
to Windows Receivers and owns the shared HTTP/WSS contract.

## Local build and tests

Prerequisites: the Go toolchain from `go.mod`, Node.js 24 for the shell-helper
tests, Docker for the disposable database checks and the tagged PostgreSQL
integration tests, and the root npm dependencies (`npm ci`) with `uvx` for the
SQL checks.

```sh
make build          # compile every Go package
make test           # node helper tests, then go test ./...
make generate       # Jet types and both published descriptions
make check-contracts # pinned protocol validators, then the contract tests
make format-check   # gofmt check over handwritten Go sources
make lint           # pinned golangci-lint, then the source-size check
make lint-sql       # disposable PostgreSQL load, catalog, Schemalint, SQLFluff
make format-sql     # SQLFluff fixes for the desired schemas
make dev            # rebuild local data from the newest snapshot, mask, then run the api
make dev-code-only  # rebuild/restart only the api, keeping local data
make dev-stop       # stop and remove this checkout's local container and volume
```

The default tests need no credentials and no database. They start the composed
service on a real loopback listener and assert the sanitized `/health` contract,
routing fallbacks, the shared problem-details error model and bounded shutdown.

Disposable SQL and integration databases run the derived PostgreSQL 18 + pg_cron
image built from `deploy/compose/postgres.Dockerfile`. Local development uses
`scripts/dev/postgres.Dockerfile` instead: the same PostgreSQL and pg_cron
versions plus Anonymizer 3.2.3, with pg_cron's launcher disabled.

`make lint-sql` loads `db/schema/release` and `db/schema/pin_glide` into a
disposable loopback PostgreSQL container, runs the native catalog naming checks,
Schemalint with the repository rules and SQLFluff, then removes the container.
It never connects to a development or production database, and it finishes by
loading `test/fixtures/sql/desired_schema_violations.sql` and requiring the
native naming rules, both Schemalint rules and both SQLFluff rule families to
report violations in it, so a check that quietly stopped reporting fails the
command.

`make format-sql` formats only the desired schemas; migration and data scripts
are linted but never reformatted automatically.

`make generate` loads both desired schemas into the same disposable PostgreSQL
container `make lint-sql` uses, generates the Jet types into
`internal/db/jetgen/{pin_glide,release}` and writes the published HTTP and WSS
descriptions from the API entrypoint's offline modes into `generated/protocol`.
An empty schema is legitimate: pinned Jet succeeds without writing anything, so
generation installs nothing and removes the schema's stale output instead of
moving a directory that does not exist. Commit the regenerated artifacts; `git
diff` plus `./scripts/quality/check-generated.sh` fail on tracked drift and on
uncommitted output.

`make check-contracts` exports both descriptions without a listener, a database
or a provider, validates them with the pinned `@redocly/cli` and `@asyncapi/cli`
versions, then runs the HTTP and WSS contract tests: stable `VerbResource`
operation identifiers, one domain tag, the shared problem schema on every
failure response, embedded shared wire schemas and stated authentication. The
pinned validators must also reject the violating fixtures under
`test/fixtures/contract`, so a validator that silently stopped reporting cannot
pass.

PostgreSQL-backed integration tests are explicit and use a disposable,
loopback-only database:

```sh
go test -tags=integration ./test/integration/...
```

`make test` does not run them.

## Check scope and suppressions

`make format-check`, `make lint`, `make lint-sql` and `make check-contracts`
cover the scopes below. Everything else in `code-architecture.md` remains a
code-review responsibility; these checks do not prove domain ownership,
migration safety or runtime behavior.

| Command | Scope |
| --- | --- |
| `make format-check` / `make format` | gofmt over tracked and nonignored untracked handwritten `*.go`; Jet output and `generated/` are excluded |
| `make lint` | pinned golangci-lint standard set and the 500-line limit for handwritten Go, TypeScript/TSX, CSS and shell files; generated files are exempt |
| `make lint-sql` | both desired schemas, the native catalog checks and the Schemalint/SQLFluff rules described in `docs/sql-conventions.md`, plus the violating fixture those rules must reject |
| `make check-contracts` | syntax, resolved references and the reported route, message, authentication and error coverage of both published descriptions, plus the violating fixtures the pinned validators must reject |

Suppressions are scoped and reviewable: a Go `//nolint` suppression states its
reason and needs maintainer review, native SQL rules accept an exact three-part
identifier in `scripts/quality/sql-lint-ignore.sql`, and Schemalint uses its
`ignores` facility. Executed or skipped migration and data scripts keep their
bytes, so they are never reformatted automatically.

## Running the API process

`cmd/pin-glide-api` requires a reachable PostgreSQL database and reads its
configuration from the environment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `PIN_GLIDE_ENV` | `development` | `development` or `production` |
| `PIN_GLIDE_API_ADDRESS` | `:8080` | Listen address of the API process |
| `PIN_GLIDE_DATABASE_URL` | local development DSN | PostgreSQL connection string; required in production |
| `PIN_GLIDE_WECHAT_APP_ID` | none | AppID of an approved WeChat mobile application; set with the secret to enable WeChat |
| `PIN_GLIDE_WECHAT_APP_SECRET` | none | AppSecret of that application; never logged |
| `PIN_GLIDE_QQ_APP_ID` | none | AppID of an approved QQ mobile application; set with the AppKey to enable QQ |
| `PIN_GLIDE_QQ_APP_KEY` | none | AppKey of that application; never logged |
| `PIN_GLIDE_BAILIAN_API_KEY` | none | Parent API key of the approved Bailian Beijing Workspace; required in production, never logged |

WeChat and QQ configuration is optional at startup. A missing or partial provider pair fails only when that provider is used. The API and database tools do not require either provider in any environment.

Temporary model credentials come from that parent key plus the enabled non-secret `pin_glide.model_configurations` row. The release Data Script installs the approved Beijing Workspace and `qwen3.7-flash` model; see `deploy/compose/README.md` for verification and change control.
The repository verifies that boundary with the fake in `test/fakes/bailian` and
the adapter's own HTTP tests; confirming the real temporary-key endpoint, its
lifetime parameter, its answer and the parent key's permissions is a separate
request-driven acceptance step.

## Running the database tools

`cmd/pin-glide-db-tools migrate` applies one Database Release from the packaged
migration image: it holds one database lock, synchronizes the `release` and
`pin_glide` desired schemas with the pinned psqldef, appends the Database
Release and then runs the Data Scripts. `snapshot` writes and validates one
custom-format archive.

| Variable | Meaning |
| --- | --- |
| `PIN_GLIDE_ENV` | `production` for a real release |
| `PIN_GLIDE_DATABASE_URL` | Database to migrate; required in production |
| `PIN_GLIDE_RELEASE_VERSION` | Target `X.Y.Z`, literal and without `v` |
| `PIN_GLIDE_RELEASE_INITIAL` | `true` only for a verified fresh database |
| `PIN_GLIDE_RELEASE_INITIAL_RETRY` | `true` to retry that initial version |
| `PIN_GLIDE_SNAPSHOT_PATH` | Existing host directory for `snapshot` |
| `PIN_GLIDE_SNAPSHOT_KEEP` | Positive count of completed snapshots to retain |

Scripts run in the order of the `db/migrations/order.txt` manifest packaged with
the release. Without that manifest, a checkout derives the same order from its
complete first-parent Git history; shallow history and SQL outside the
supported migration streams are rejected.

## Local development and installation

Set an existing snapshot directory before the first local start:

```sh
mkdir -p /path/to/pin-glide-snapshots
export PIN_GLIDE_SNAPSHOT_PATH=/path/to/pin-glide-snapshots
make dev
```

`make dev` is destructive for this checkout's local project only. It validates
the directory before deleting local state, starts an isolated PostgreSQL on
loopback, restores the newest regular `*.dump` by modification time, migrates,
applies `dev/anon`, and only then starts the api. An empty directory starts a
fresh database. Ctrl-C stops application descendants but keeps PostgreSQL for
`make dev-code-only`; `make dev-stop` removes the local container, network and
volume. Local WeChat, QQ and Bailian credential variables are cleared, so provider-bound
operations fail before network I/O. A restored snapshot keeps its published
Database Release history, so local migration records the checkout's
`version.json` instead of a synthetic `0.0.0` target.

`install.sh` with `deploy/compose` installs and upgrades one Pin Glide instance
from published images: prepare, pull, stop the api and its scheduled database
work, take a stopped-writer recovery backup, migrate, start and restore the
intended scheduling state. `deploy/compose/README.md` documents the operator
prerequisites, commands and recovery procedure, including the whole-table
retention inventory, its clocks and the scheduled cleanup job.

## Release publication

`CI` runs on every pull request and `main` push. Publication is a separate,
human-controlled step, so the merged source, the tag, the images and both
contract assets always come from one commit.

1. A release-eligible push to `main` (anything except `version.json`,
   documentation and workflow files) makes the `auto-release` workflow open or
   refresh the release pull request against `main`, bumping the patch version in
   `version.json`.
2. Merging that pull request with squash or rebase, never a merge commit, starts
   the `publish-release` workflow, which:
   - re-runs the complete `CI` workflow at the merged commit: format, static
     checks, SQL checks, generated drift, tests and both published contracts;
   - exports both descriptions from that commit, validates them with the pinned
     validators and proves they are the committed descriptions the contract
     tests cover;
   - publishes `ghcr.io/buddylabsco/pin-glide-api:<version>` and
     `ghcr.io/buddylabsco/pin-glide-db-tools:<version>` from the exact commit;
   - publishes `ghcr.io/buddylabsco/pin-glide-postgres:<tag>` from
     `deploy/compose/postgres.Dockerfile` when that image is missing, and reuses
     it otherwise, because `deploy/compose/docker-compose.yml` is the authority
     for that distribution;
   - creates the `v<version>` tag and the GitHub Release, attaching
     `openapi.yaml` and `asyncapi.yaml` while the Release is still a draft and
     publishing it only once both are attached.

Re-running the same merged pull request reuses the tag, the images and the
Release assets after checking each one still matches the release commit and
carries the validated descriptions. A conflicting tag, image or published asset
fails instead of being overwritten, and corrections to a published contract use
a new release.

### Release automation configuration

| Item | Value |
| --- | --- |
| Repository secrets | `APP_ID` and `APP_PRIVATE_KEY` of the release GitHub App |
| App permissions | Pull requests read/write: the App opens and refreshes the release pull request, never the branch push |
| `auto-release` token | The App installation token for `gh`; the checkout's workflow `GITHUB_TOKEN` remains the branch push credential |
| `publish-release` token | The automatic workflow `GITHUB_TOKEN` with `contents: write`, `pull-requests: read` and `packages: write`; no personal token |
| Deployment pull credential | A personal access token (classic) with at least `read:packages`; GHCR package access does not accept fine-grained tokens |
| Pins | Node.js 24, `@redocly/cli` 2.62.1, `@asyncapi/cli` 6.2.0, Go from `go.mod` |

The automation has its own local checks:

```sh
node --test scripts/release/auto-release-pr.test.mjs
node --test scripts/release/publish-release.test.mjs
```

Both run as part of `make test` and fake only `gh` and Docker, so they need no
credentials and no registry. Real PostgreSQL migration behavior stays in
`go test -tags=integration ./test/integration/...`, and the real Compose
install/upgrade/restore journey stays in
`node test/smoke/release-install.mjs --integration-ready`; neither suite is a
publication gate. The real local restore/mask/lifecycle check is explicit:
`node test/smoke/dev-snapshot.mjs --docker`.

The deployed LAN smoke runner checks a published release against `local-server`
without accepting an ambiguous target:

```sh
PIN_GLIDE_SMOKE_RELEASE=0.1.6 node test/smoke/runner.mjs --check
PIN_GLIDE_SMOKE_RELEASE=0.1.6 PIN_GLIDE_SMOKE_WRITE_AUTHORIZATION=pin-glide-dev \
  node test/smoke/runner.mjs --integration-ready
```

`--check` is read-only and exits `2` after prerequisites pass because it does
not execute restore. `--integration-ready` creates a stopped-writer container
snapshot and restores it into a disposable database. It then seeds and removes
run-owned SQL fixtures, exercises the Sender credential/session lifecycle
through the deployed HTTP and WebSocket surface, calls the real service-issued
Bailian flow directly, and writes redacted evidence. The Bailian flow verifies
strict text-only OTP and no-OTP inference plus controlled authority,
configuration, parent-key and connectivity failures. It waits for the
provider-reported temporary-key expiry (about eleven minutes) and briefly
recreates the API container for the invalid-parent-key fault.
After both releases are published, this command proves fresh installation and
the declared upgrade with disposable Compose projects and the real release
images:

```sh
node test/smoke/published-install.mjs --integration-ready --from 0.1.5 --to 0.1.6
```

See `deploy/compose/README.md` for the target guards,
upgrade-origin rule, evidence and provider-proof exclusions.

The repository is implemented ticket by ticket, and the published descriptions
describe only the surface delivered so far: the WSS definitions declare no
channel until a feature ticket serves one.
