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
make dev            # disposable PostgreSQL, migrations, then the api on loopback
```

The default tests need no credentials and no database. They start the composed
service on a real loopback listener and assert the sanitized `/health` contract,
routing fallbacks, the shared problem-details error model and bounded shutdown.

Every disposable database in those commands runs the derived PostgreSQL 18 +
pg_cron image built from `deploy/compose/postgres.Dockerfile`, because pg_cron
needs a preloaded extension the base image does not ship. `make dev` starts only
that database from the same Compose file with its own project, volume, port and
Database Release, runs `cmd/pin-glide-release` against it, then runs the api.

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
| `PIN_GLIDE_WECHAT_APP_ID` | none | AppID of the approved WeChat mobile application; required in production |
| `PIN_GLIDE_WECHAT_APP_SECRET` | none | AppSecret of that application; required in production, never logged |
| `PIN_GLIDE_BAILIAN_API_KEY` | none | Parent API key of the approved Bailian Beijing Workspace; required in production, never logged |

Temporary model credentials come from that parent key plus the enabled
non-secret `pin_glide.model_configurations` row an operator maintains; see
`deploy/compose/README.md` for the deployment steps.
The repository verifies that boundary with the fake in `test/fakes/bailian` and
the adapter's own HTTP tests; confirming the real temporary-key endpoint, its
lifetime parameter, its answer and the parent key's permissions is a separate
request-driven acceptance step.

## Running the release CLI

`cmd/pin-glide-release` applies one Database Release from the packaged migration
image: it holds one database lock, synchronizes the `release` and `pin_glide`
desired schemas with the pinned psqldef, appends the Database Release and then
runs the Data Scripts.

| Variable | Meaning |
| --- | --- |
| `PIN_GLIDE_ENV` | `production` for a real release |
| `PIN_GLIDE_DATABASE_URL` | Database to migrate; required in production |
| `PIN_GLIDE_RELEASE_VERSION` | Target `X.Y.Z`, literal and without `v` |
| `PIN_GLIDE_RELEASE_INITIAL` | `true` only for a verified fresh database |
| `PIN_GLIDE_RELEASE_INITIAL_RETRY` | `true` to retry that initial version |

Scripts run in the order of the `db/migrations/order.txt` manifest packaged with
the release. Without that manifest, a checkout derives the same order from its
complete first-parent Git history; shallow history and SQL outside the
supported migration streams are rejected.

## Local development and installation

`make dev` starts a disposable local PostgreSQL with pg_cron, runs the packaged
release CLI against it and then runs the api on loopback; it refuses
non-development configuration and cannot reach a deployment installation.

`install.sh` with `deploy/compose` installs and upgrades one Pin Glide instance
from published images: prepare, pull, stop the api and its scheduled database
work, take a stopped-writer recovery backup, migrate, start and restore the
intended scheduling state. `deploy/compose/README.md` documents the operator
prerequisites, commands and recovery procedure.

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
     `ghcr.io/buddylabsco/pin-glide-migrate:<version>` from the exact commit;
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
publication gate.

The repository is implemented ticket by ticket, and the published descriptions
describe only the surface delivered so far: the WSS definitions declare no
channel until a feature ticket serves one.
