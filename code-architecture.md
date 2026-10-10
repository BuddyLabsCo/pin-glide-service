# Code Architecture

This document is the placement contract for code changes in this repository. It
defines where source, generated artifacts, schema, tests, and deployment support
belong. Paths describe the target layout; create files as their code is needed.

Product requirements and decisions live in the [architecture ticket](https://github.com/BuddyLabsCo/pin-glide-service/issues/3)
and [functional specification](docs/specs/service-requirements.md). Domain language
is defined in [GLOSSARY.md](GLOSSARY.md).

## 1. Non-negotiable runtime shape

Pin Glide has one long-running application unit:

| Unit | Entrypoint or artifact | Runtime responsibility | Allowed dependencies |
| --- | --- | --- | --- |
| `api` | `cmd/pin-glide-api` | HTTP APIs and authenticated WSS connections | PostgreSQL, WeChat, Bailian temporary-key API |

The release CLI is a one-off deployment tool, not another application process.

### 1.1 Legal call directions

```text
Android Sender ──HTTPS/WSS──► api
Windows Receiver ──HTTPS/WSS──► api

api ──► PostgreSQL          identity, authorization, configuration, metadata
api ──► WeChat              identity verification
api ──► Bailian             temporary-key acquisition
api ──WSS──► Receiver       live ciphertext and control messages

release CLI ──► PostgreSQL  schema sync and ordered scripts
pg_cron ──► PostgreSQL      configured cleanup
```

### 1.2 Forbidden dependencies

Reject changes that introduce:

- Business packages importing transport adapters, Jet/pgx, `cmd`, or deployment code.
- HTTP/WSS handlers executing SQL or business state transitions.
- Provider or WSS network calls inside a PostgreSQL transaction.
- Generated Jet models used as domain types or API DTOs.
- A second database or a database-per-domain split.
- PostgreSQL/log persistence of message payloads, client encryption keys or raw credentials.

## 2. Repository layout

```text
pin-glide-service/
├── cmd/
│   ├── pin-glide-api/         # API process entrypoint; offline HTTP export
│   ├── pin-glide-release/     # Release CLI entrypoint
│   └── pin-glide-contracts/   # Shared-schema and WSS contract export
├── internal/
│   ├── adapters/
│   │   ├── httpapi/           # Huma routes and HTTP server lifecycle
│   │   ├── wss/               # Upgrade, frame I/O and connection lifecycle
│   │   ├── postgres/          # Domain-owned Jet stores and log sink
│   │   ├── wechat/            # WeChat HTTP client
│   │   └── bailian/           # Bailian HTTP client
│   ├── identity/              # Account, devices and credentials
│   ├── pairing/               # Pairing and Pairing Session use cases
│   ├── model/                 # Model configuration and temporary credentials
│   ├── delivery/              # Admission, deduplication and delivery results
│   ├── protocol/              # Canonical HTTP/WSS wire types
│   ├── config/                # Environment configuration
│   ├── db/                    # Connection/executor support and Jet output
│   ├── observability/         # Structured logs, correlation and redaction
│   └── release/               # Release manifest, ledger and migration execution
├── db/
│   ├── schema/
│   │   ├── pin_glide/         # Desired application schema
│   │   └── release/           # Desired release metadata schema
│   ├── migrations/
│   │   ├── pin_glide/         # Application schema transition scripts
│   │   └── release/           # Release schema transition scripts
│   └── scripts/               # Post-sync initialization and data repairs
├── protocol/
│   └── asyncapi.yaml          # Handwritten channel/operation definitions
├── generated/
│   └── protocol/              # Shared schemas and contract assembly output
├── openapi.yaml               # Generated HTTP release contract
├── asyncapi.yaml              # Generated WSS release contract
├── deploy/
│   └── compose/               # Compose, Dockerfiles and operator runbook
├── test/
│   ├── integration/           # Go tests using one-off PostgreSQL
│   ├── contract/              # Public HTTP/WSS contract harnesses
│   ├── smoke/                 # Compose and release smoke tests
│   ├── fakes/                 # Controllable WeChat and Bailian clients
│   └── fixtures/              # Frozen wire and database fixtures
├── scripts/                   # Tooling grouped by function; see section 8
├── .github/workflows/
├── Makefile
├── install.sh
├── go.mod
└── docs/
```

Do not create `common`, `utils`, `shared`, or `misc` packages. Name helpers by
responsibility, such as `config`, `db`, or `observability`.

General naming rules:

- Go files use `lower_snake_case.go`; package names match their directories.
- Go tests are colocated as `*_test.go`.
- SQL object names follow section 4.2.
- Workflows/scripts use `kebab-case` or the tool-required filename.
- Generated output belongs only in the locations listed in section 6.
- Apply Helm's 500-line handwritten-source check. Split by aggregate, use case or
  transport operation, not arbitrary line count.

## 3. Backend package rules

### 3.1 Domain package template

Each domain owns its types, invariants, state transitions and I/O interfaces:

```text
internal/<domain>/
├── <domain>.go       # Concepts, states, value objects and errors
├── service.go        # Business operations
└── ports.go          # Required I/O interfaces; add only when needed

internal/adapters/postgres/<domain>/
└── store.go          # Jet queries for this domain's relations
```

Keep the agreed business/adapters separation: Helm's domain `store.go` role is
implemented in the PostgreSQL adapter, not imported into the business package.

When several aggregates need separate files, split by aggregate:

```text
internal/identity/
├── identity.go
├── service_login.go
├── service_refresh.go
├── service_revocation.go
└── ports.go

internal/adapters/postgres/identity/
├── store_devices.go
├── store_credentials.go
└── store_refresh.go
```

Rules:

- `service.go` or `service_<aggregate>.go` owns the business operation and its
  atomicity requirements. The store interface exposes the required atomic I/O;
  the PostgreSQL adapter owns `*sql.Tx` and query execution.
- `store.go` or `store_<aggregate>.go` in the adapter is the normal place for Jet.
  Accept the shared executor so queries work with `*sql.DB` or `*sql.Tx`.
- Domain types live in the domain package. Map generated result structs inside
  the adapter; never expose them through a domain interface or public DTO.
- WeChat/Bailian clients contain HTTP protocol handling, not SQL or domain state
  transitions.
- A cross-domain workflow belongs to the domain owning the visible outcome.
  Compose explicit domain interfaces; do not create a generic orchestrator.
  Resolve cycles by placing the shared invariant in its owning domain.
- Pass contexts through I/O. Each goroutine has an owner, cancellation path and
  shutdown/join path. Handle errors and preserve typed causes with `%w`.

### 3.2 API package

Inbound HTTP belongs in `internal/adapters/httpapi`. Domain packages do not import
`net/http` or know that Huma calls them.

```text
internal/adapters/httpapi/
├── server.go
├── router.go
├── middleware.go
├── errors.go
├── identity/
│   ├── routes.go
│   └── dto.go
├── pairing/
│   ├── routes.go
│   └── dto.go
├── model/
│   ├── routes.go
│   └── dto.go
└── delivery/
    ├── routes.go
    └── dto.go
```

Naming and ownership:

- `<domain>/routes.go` registers Huma operations.
- `<domain>/dto.go` owns HTTP parameter wrappers and domain-to-wire mapping.
  Shared JSON payload types stay in `internal/protocol`; do not redeclare them.
- Handlers adapt transport to a domain service call. No SQL or multi-step
  business rules in handlers.
- Operation IDs are stable `VerbResource` names. Every operation has one stable
  domain tag.
- Middleware, routing fallbacks and pre-upgrade rejections use the shared error
  mapping. Do not duplicate error-envelope definitions per route.

### 3.3 WebSocket adapter

`internal/adapters/wss` owns WebSocket transport and connection coordination:

```text
internal/adapters/wss/
├── router.go         # Upgrade registration and handshake mapping
├── connection.go     # Owned slots, reads/writes, heartbeat and shutdown
├── delivery.go       # Delivery frame and Receipt mapping
└── pairing.go        # Opaque relay and Pairing Session notifications
```

These files translate frames to domain calls and map results back to protocol
messages. They do not contain SQL or duplicate authorization/state transitions.
Use connection-ID ownership for cleanup. Business-operation lifetimes remain
independent of an individual HTTP connection as specified by the use case.

### 3.4 File splitting

When handwritten code approaches 500 lines, split at a natural boundary:

- Domain: aggregate, then use case, such as `service_login.go` and `service_refresh.go`.
- PostgreSQL adapter: aggregate, such as `store_devices.go` and `store_credentials.go`.
- HTTP: domain, then operation.
- WSS: channel or connection responsibility.
- Do not split one invariant across files merely to satisfy the limit.

## 4. PostgreSQL, migrations, and Jet

### 4.1 Database and schema boundaries

Use one physical PostgreSQL database. Application relations live in `pin_glide`;
release metadata lives in `release`. A domain is not a database/schema boundary.

`internal/release` owns release metadata and orchestrates application Schema Sync.
The `cron` schema belongs to pg_cron; do not include extension objects in
application Jet generation or naming checks.

### 4.2 Source schema and explicit migrations

```text
db/
├── schema/
│   ├── pin_glide/
│   │   ├── 00_extensions.sql
│   │   ├── 10_enums/<enum>.sql
│   │   ├── 20_tables/<table>.sql
│   │   ├── 30_views/<view>.sql
│   │   ├── 40_functions/<function>.sql
│   │   └── 50_procs/<procedure>.sql
│   └── release/
├── migrations/
│   ├── pin_glide/issue_<number>_<sequence>_<summary>.sql
│   └── release/issue_<number>_<sequence>_<summary>.sql
└── scripts/
    └── issue_<number>_<sequence>_<summary>.sql
```

Derive separate desired-schema inputs from `db/schema/pin_glide` and
`db/schema/release`; do not commit concatenated inputs.

Keep each primary database object in its own file. A table file owns its columns,
constraints, keys, indexes and comments. The owning domain's PostgreSQL adapter is
the normal writer for its relations.

Normal structural changes edit desired SQL. Add a Migration Script only to bridge
a transition declarative Schema Sync cannot handle; place it under the schema it
prepares.

Initialization, repairs and versioned data transformations belong in Data Scripts.
If structural preparation includes data changes, also represent the required data
logic in a Data Script: a fresh database skips Migration Scripts and must still
receive its data.

Every script must be safe to rerun without duplicated effects or corruption.
Ledger deduplication does not replace this requirement. Never edit, delete or
reformat a script already recorded as executed or skipped. Files introduced in one
commit run in filename order; use ordered sequence suffixes.

SQL naming, comments and formatting follow [docs/sql-conventions.md](docs/sql-conventions.md).
Run `make lint-sql` before committing schema changes.

Storage rules:

- Each table has one retention policy. Do not mix durable configuration,
  temporary transactions or diagnostic logs in one table.
- Keep long-lived Receiver credentials separate from expiring session data.
  Session deletion cannot delete activated Pairings or credentials.
- Configuration deletion cannot cascade into retained transaction/log records.
  Use historical scalar identifiers where a live foreign key would violate this.
- Cleanup policies identify target tables, retention days and date columns. The
  cleanup function reads them and executes sequential time-filter DELETEs.
  Quote identifiers separately and bind cutoff values; validate configuration
  and restrict write/execution privileges.
- Add no per-table hardcoded cleanup clock or progress-checkpoint mechanism.
- Persist no message payloads, client encryption keys or raw credentials.

### 4.3 Jet generation and generated Go types

The release executable reads bundled `db/schema/release` and `db/schema/pin_glide`
SQL directly, so Schema Sync uses the matching release artifact.

Jet generation loads desired schemas into one-off PostgreSQL, never development
or production, and overwrites:

```text
internal/db/jetgen/{pin_glide,release}/
├── table/   # SQL builder objects
├── view/    # SQL builder objects
├── enum/    # Enum values
└── model/   # Result-mapping structs
```

Generated `table.*` objects are SQL builders, not domain objects or ownership
declarations. Generated `model.*` structs are query results. PostgreSQL adapters
map them to types declared in `internal/<domain>`.

Database access rules:

- `internal/db/db.go` opens the shared pgx-stdlib `*sql.DB`.
- Keep transaction/executor support in `internal/db`; prevent accidental nested
  transactions. Do not leak those types into business interfaces.
- Use Jet by default; use parameterized raw SQL where necessary.
- Keep business rules in Go. Use SQL functions/procedures only for database-local
  work that needs them, including the configured cleanup function.
- No generic repository, DAO or shared-query package.
- Generate both schemas with the same disposable-schema loader used by SQL checks.
  Never hand-edit generated Go files.

## 5. Domain ownership map

This table defines code ownership. Schema prefixes are relation-name prefixes,
not separate PostgreSQL schemas.

| Domain | Go package | Owns | API files | Store files | Schema prefix |
| --- | --- | --- | --- | --- | --- |
| Identity | `internal/identity` | Accounts, Device Registrations and credential lifecycle | `httpapi/identity` | `postgres/identity` | `identity_` |
| Pairing | `internal/pairing` | Pairings, Pairing Sessions and authorization completion | `httpapi/pairing`, `wss/pairing.go` | `postgres/pairing` | `pairing_` |
| Model | `internal/model` | Enabled model configuration and temporary-key use case | `httpapi/model` | `postgres/model` | `model_` |
| Delivery | `internal/delivery` | Admission, deduplication, results and result queries | `httpapi/delivery`, `wss/delivery.go` | `postgres/delivery` | `delivery_` |
| Protocol | `internal/protocol` | Shared wire types and errors | All HTTP/WSS adapters | none | none |
| Provider clients | `internal/adapters/{wechat,bailian}` | External HTTP protocols | Called through domain interfaces | none | none |
| Observability | `internal/observability` | Structured logging, correlation and redaction | Adapter middleware | `postgres/observability` | `service_` |
| Retention | none; SQL-owned | Cleanup function and policy table | none | Data Scripts configure it | `retention_` |
| Release | `internal/release` | Manifest, ledgers, schema sync and audits | Release CLI only | Release package | `release_` |

API/store paths in the table are relative to `internal/adapters/`. A domain's store
writes its relations; diagnostic logs do not replace authoritative domain state.

## 6. Generated artifacts and modification boundaries

`make generate` performs generation in this order:

1. Start one-off PostgreSQL.
2. Load desired `release` and `pin_glide` schemas.
3. Run Jet for both schemas and replace their generated packages.
4. Export HTTP OpenAPI from actual Huma registrations.
5. Export shared wire schemas and assemble/bundle explicit WSS definitions.

Commit generated output and mark it generated where the format permits. CI
regenerates it and fails on tracked drift or unexpected untracked output.

| Artifact | Source of truth | Output | Modification boundary |
| --- | --- | --- | --- |
| HTTP OpenAPI | Huma routes in `internal/adapters/httpapi` | Root `openapi.yaml` | Edit routes/types; never edit generated YAML |
| WSS AsyncAPI | `protocol/asyncapi.yaml` and Go wire types | Root `asyncapi.yaml` | Edit channel definitions/types; never edit generated YAML |
| Shared schemas | `internal/protocol` | `generated/protocol/schemas.json` | Export all required types, including WSS-only types |
| Jet builders/models | `db/schema/{pin_glide,release}` | `internal/db/jetgen/{pin_glide,release}` | Edit desired SQL; never edit generated Go |

Generation rules:

- `cmd/pin-glide-api -openapi` registers real routes without starting runtime
  database/provider connections. `cmd/pin-glide-contracts` exports shared/WSS schemas.
- Raw WSS handshakes need HTTP descriptions; Huma alone does not describe frames.
  Keep channel directions, authentication, errors and Close semantics in AsyncAPI.
- Do not recover WSS-only schemas from Huma's pruned HTTP output. Validate the shared
  JSON Schema subset across OpenAPI and AsyncAPI.
- Bundle root YAML to internal references. Test descriptions against actual
  registration and public wire behavior.
- Android/Windows client generation belongs to their repositories; generate no
  client SDK here.

## 7. Test placement

| Test kind | Location | Rule |
| --- | --- | --- |
| Go unit test | Colocated `internal/<package>/*_test.go` | Pure invariants and small helpers; do not mirror every function |
| Go integration test | `test/integration/<domain>/<scenario>_test.go` | One-off PostgreSQL; assert domain facts |
| HTTP/WSS contract test | `test/contract/{http,wss}/` | Public protocol boundaries and frozen fixtures |
| Compose/release smoke | `test/smoke/` | Images, migration, restore and service health |
| Fakes | `test/fakes/{wechat,bailian}/` | Controllable external dependencies; no real credentials |
| Fixtures | `test/fixtures/` | Raw wire and database fixtures |

Test rules:

- Prefer public HTTP/WSS/domain behavior over SDK-private or private SQL details.
- Integration tests may establish inputs or inspect durable domain facts in
  PostgreSQL; do not assert how a private query is written.
- A test needing Compose belongs in `test/smoke`; one needing only PostgreSQL
  belongs in `test/integration`.
- Keep supported-contract fixtures for compatibility changes.
- Tests/generation never use deployed development or production databases.
  Cross-client development acceptance uses the separately configured smoke harness.

## 8. Local development, Compose, and release files

These are support-file locations, not additional runtime units.

```text
Makefile
install.sh                    # Host-side Compose release orchestration
deploy/compose/
├── docker-compose.yml
├── .env.example
├── api.Dockerfile
├── migrate.Dockerfile
├── postgres.Dockerfile        # pg_cron-capable database image
└── README.md                  # Operator procedures
.github/workflows/
└── *.yml
scripts/
├── dev/                      # Local infrastructure and development processes
├── codegen/                  # Jet, OpenAPI and AsyncAPI generation
├── db/                       # Disposable PostgreSQL and schema loading
├── quality/                  # Format, lint, tests and SQL rules
└── release/                  # Packaging and publication
cmd/pin-glide-release/
internal/release/             # In-image migration implementation
```

Reuse Helm Gateway's tooling and pinned dependencies at commit
[`ba375819`](https://github.com/BuddyLabsCo/helm-gateway/tree/ba375819b98bcd76fc27d7a15b2523aa62837c6f).
Copy installer, release engine, manifest packaging and regression tests together;
substitute product identity and remove unrelated frontend/Redis/River steps. Keep
root npm for Schemalint. The [reuse report](docs/research/helm-gateway-go-sql-deployment-reuse.md)
records the exact source/configuration inventory.

| Command | Tooling/configuration |
| --- | --- |
| `make format-check` / `make format` | gofmt; `scripts/quality/format.sh` |
| `make lint` | `.golangci.yml` standard set and handwritten-source size check |
| `make lint-sql` / `make format-sql` | PostgreSQL load, catalog rules, Schemalint and SQLFluff |
| `make generate` | `scripts/codegen/generate.sh`; Jet and both contract exports |
| `make check-contracts` | `scripts/quality/check-contracts.sh`; references, coverage and compatibility |
| `make test` | Script tests and `go test ./...` |
| `make build` | `go build ./...` |
| `go test -tags=integration ./test/integration/...` | Explicit database integration tests |

Keep quality/generated/test/build CI jobs separate. Helm's standard Go linters are
errcheck, govet, ineffassign, staticcheck and unused; dependency directions remain
review rules. Fail on violations/drift. Suppressions require a scoped reason and
maintainer review.

Go/tool versions belong in `go.mod`/`go.sum`; Node tools in root npm manifest/lock.
Use `@asyncapi/cli@6.2.0` and `@redocly/cli@2.62.1` for contract validation. Their
commands are `npm exec -- asyncapi validate asyncapi.yaml --fail-severity error`
and `npm exec -- redocly lint openapi.yaml`.

- `make format-sql` changes desired schemas only. Processed scripts are immutable.
- Integration-tag and Compose smoke tests are not implied by default `make test`.
- Bundle desired SQL, immutable scripts, manifest and pinned psqldef in the
  matching migration image. Preserve Helm's ledger/phase/recovery behavior.
- Manage cron definitions through Data Scripts; image/preload configuration and
  scheduler control belong to deployment support.
- Publish matching images and both validated YAML assets from one source commit;
  attach both before Release publication. Published artifacts are immutable.
- Keep operator installation/backup/retry/restore procedures in the Compose runbook;
  keep product acceptance and decisions in the architecture ticket.
