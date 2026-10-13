# Pin Glide Service

The service side of Pin Glide: one Go process that authenticates Android
Senders, authorizes Pairings, supplies model credentials, relays live ciphertext
to Windows Receivers and owns the shared HTTP/WSS contract.

## Local build and tests

Prerequisites: the Go toolchain from `go.mod`, Node.js 24 for the shell-helper
tests, and Docker with the root npm dependencies (`npm ci`) and `uvx` for the
disposable SQL checks and the tagged PostgreSQL integration tests.

```sh
make build          # compile every Go package
make test           # node helper tests, then go test ./...
make format-check   # gofmt check over handwritten Go sources
make lint           # pinned golangci-lint, then the source-size check
make lint-sql       # disposable PostgreSQL load, catalog, Schemalint, SQLFluff
make format-sql     # SQLFluff fixes for the desired schemas
```

The default tests need no credentials and no database. They start the composed
service on a real loopback listener and assert the sanitized `/health` contract,
routing fallbacks, the shared problem-details error model and bounded shutdown.

`make lint-sql` loads `db/schema/release` and `db/schema/pin_glide` into a
disposable loopback PostgreSQL container, runs the native catalog naming checks,
Schemalint with the repository rules and SQLFluff, then removes the container.
It never connects to a development or production database, and it finishes by
loading `test/fixtures/sql/desired_schema_violations.sql` and requiring each
rule family to reject it, so a check that stopped checking fails the command.
`make format-sql` formats only the desired schemas; migration and data scripts
are linted but never reformatted automatically.

PostgreSQL-backed integration tests are explicit and use a disposable,
loopback-only database:

```sh
go test -tags=integration ./test/integration/...
```

## Check scope and suppressions

`make format-check`, `make lint` and `make lint-sql` cover the scopes below.
Everything else in `code-architecture.md` remains a code-review responsibility;
these checks do not prove domain ownership, migration safety or runtime
behavior.

| Command | Scope |
| --- | --- |
| `make format-check` / `make format` | gofmt over tracked and nonignored untracked handwritten `*.go`; Jet output and `generated/` are excluded |
| `make lint` | pinned golangci-lint standard set and the 500-line limit for handwritten Go, TypeScript/TSX, CSS and shell files; generated files are exempt |
| `make lint-sql` | both desired schemas, the native catalog checks and the Schemalint/SQLFluff rules described in `docs/sql-conventions.md`, plus the violating fixture those rules must reject |

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

## Not available yet

The repository is implemented ticket by ticket. Schema generation, the release
CLI, Compose installation and local development, CI workflows and the published
HTTP/WSS contract assets arrive with their own tickets.
