# Code Architecture

This document is the executable placement contract for all code changes in this repository. 
It defines where source, generated artifacts, schema, tests, and deployment support belong.

## 1. Non-negotiable runtime shape

One Go module, one long-running `api` process for HTTP and WSS. `cmd/pin-glide-api`
wires domain services, adapters and lifecycle. The release CLI runs only during deployment.

## 2. Repository layout

```text
pin-glide-service/
├── cmd/
│   ├── pin-glide-api/         # API entrypoint and offline HTTP/WSS contract export
│   └── pin-glide-release/     # Release CLI
├── internal/
│   ├── identity/             # Accounts, devices and credentials
│   ├── pairing/              # Pairings and Pairing Sessions
│   ├── model/                # Model configuration and temporary credentials
│   ├── delivery/             # Admission, deduplication and results
│   ├── adapters/
│   │   ├── httpapi/           # Huma routes and HTTP lifecycle
│   │   ├── wss/               # Frames and connection lifecycle
│   │   ├── postgres/          # Domain stores and log sink
│   │   ├── wechat/            # WeChat client
│   │   └── bailian/           # Bailian client
│   ├── protocol/             # Shared HTTP/WSS wire types and errors
│   ├── config/               # Environment configuration
│   ├── db/                   # Connections, transactions and Jet output
│   ├── observability/        # Logging, correlation and redaction
│   └── release/              # Manifest, ledgers and migration execution
├── db/
│   ├── schema/{pin_glide,release}/
│   ├── migrations/{pin_glide,release}/
│   └── scripts/
├── protocol/asyncapi.yaml     # Handwritten WSS definitions
├── generated/protocol/       # Shared schemas
├── openapi.yaml              # Generated HTTP contract
├── asyncapi.yaml              # Generated WSS contract
├── deploy/compose/
├── test/
│   ├── integration/
│   ├── contract/
│   ├── smoke/
│   ├── fakes/
│   └── fixtures/
├── scripts/
├── .github/workflows/
├── Makefile
├── install.sh
├── go.mod
└── docs/
```

Use `lower_snake_case.go` for Go files and directory names for packages. Name
helpers by responsibility; avoid catch-all `common`, `utils`, `shared` or `misc` packages.

## 3. Backend package rules

### 3.1 Domain package template

```text
internal/<domain>/
├── <domain>.go       # Types, states, invariants and errors
├── service.go        # Business operations
└── ports.go          # Required I/O interfaces, when needed

internal/adapters/postgres/<domain>/
└── store.go          # Jet queries and domain/result mapping
```

Domain services own business operations and their atomicity requirements. Keep
transport and database types in adapters. Expose small I/O interfaces owned by the
consuming domain; the PostgreSQL adapter implements atomic operations with `*sql.Tx`.

Place cross-domain workflows in the domain owning the outcome. Provider adapters
handle external protocols. Pass contexts through I/O, preserve error causes with
`%w`, and give each goroutine a cancellation and shutdown path.

### 3.2 API package

`internal/adapters/httpapi` owns `server.go`, `router.go`, `middleware.go` and
`errors.go`. Each domain has `<domain>/routes.go` for Huma registrations and
`<domain>/dto.go` for HTTP wrappers and domain-to-wire mapping.

Handlers validate/map transport input, call domain services and map results.
Shared payloads belong in `internal/protocol`. Use shared error mapping, stable
`VerbResource` operation IDs and one domain tag per operation.

### 3.3 WebSocket adapter

`internal/adapters/wss` owns `router.go` for upgrades, `connection.go` for frame I/O,
heartbeat and shutdown, and `delivery.go` / `pairing.go` for domain-to-wire mapping.

Keep authorization and state transitions in domain services. Connection cleanup
uses connection-ID ownership; an HTTP connection does not own a business operation's lifetime.

### 3.4 File splitting

Split handwritten files approaching 500 lines by aggregate, use case or transport
operation: `service_login.go`, `store_credentials.go`. Keep each invariant together.

## 4. PostgreSQL, migrations, and Jet

### 4.1 Database and schema boundaries

One PostgreSQL database: application relations in `pin_glide`, deployment metadata
in `release`. Domains use relation-name prefixes, not separate schemas.
`internal/release` owns deployment metadata and Schema Sync. pg_cron owns `cron`;
exclude extension objects from application Jet generation and naming checks.

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
├── migrations/{pin_glide,release}/issue_<number>_<sequence>_<summary>.sql
└── scripts/issue_<number>_<sequence>_<summary>.sql
```

Keep one primary object per desired-schema file. A table file owns its columns,
constraints, keys, indexes and comments. Derive schema inputs separately for
`pin_glide` and `release`; keep concatenated inputs untracked.

Edit desired SQL for structural changes. Migration Scripts prepare transitions
Schema Sync cannot handle. Data Scripts own initialization, repairs and data
transformations. Fresh databases skip Migration Scripts, so required data logic
must also exist in Data Scripts.

Scripts must be safe to rerun. Executed or skipped scripts are immutable; scripts
introduced in one commit run in filename order. SQL names, comments and formatting
follow [docs/sql-conventions.md](docs/sql-conventions.md).

Keep configuration, transaction and log data in separate tables with one retention
policy per table. Cleanup SQL and its policy table belong in desired schema; cron
job registration belongs in Data Scripts. Retention requirements live in the architecture ticket.

### 4.3 Jet generation and generated Go types

Generate both schemas using disposable PostgreSQL. Output goes to
`internal/db/jetgen/{pin_glide,release}/{table,view,enum,model}/`.
PostgreSQL adapters map generated result structs to domain types.

`internal/db/db.go` opens the shared pgx-stdlib `*sql.DB`; transaction/executor
support stays in `internal/db`. Store queries accept an executor usable with
`*sql.DB` or `*sql.Tx`. Keep external network calls outside database transactions.

Use Jet by default and parameterized raw SQL when needed. Business logic stays in
Go; SQL functions/procedures handle database-local work such as cleanup. Quote
dynamic identifiers separately and bind values. Keep queries in their owning
stores without a generic repository/DAO layer.

## 5. Domain ownership map

Store paths below are relative to `internal/adapters/postgres/`.

| Domain | Go package | Store | Relation prefix |
| --- | --- | --- | --- |
| Identity | `internal/identity` | `identity/` | `identity_` |
| Pairing | `internal/pairing` | `pairing/` | `pairing_` |
| Model | `internal/model` | `model/` | `model_` |
| Delivery | `internal/delivery` | `delivery/` | `delivery_` |
| Observability | `internal/observability` | `observability/` | `service_` |
| Retention | SQL function and policy table | Data Scripts configure it | `retention_` |
| Release | `internal/release` | Owned by release package | `release_` |

Each domain's store writes its relations. Shared wire types belong in
`internal/protocol`; provider clients belong in `internal/adapters/{wechat,bailian}`.

## 6. Generated artifacts and modification boundaries

| Artifact | Edit source | Generated output |
| --- | --- | --- |
| HTTP OpenAPI | Huma registrations and wire types | `openapi.yaml` |
| WSS AsyncAPI | `protocol/asyncapi.yaml` and wire types | `asyncapi.yaml` |
| Shared schemas | `internal/protocol` | `generated/protocol/schemas.json` |
| Jet builders/models | `db/schema/{pin_glide,release}` | `internal/db/jetgen/{pin_glide,release}/` |

Commit generated output; edit its source. `cmd/pin-glide-api` owns both offline
export modes: `-openapi` writes HTTP OpenAPI to stdout; `-asyncapi` writes WSS
AsyncAPI to stdout. Each exits without starting the server or connecting to
databases/providers. Export diagnostics go to stderr.

HTTP export uses actual Huma registrations. WSS export assembles the handwritten
AsyncAPI definitions with schemas from `internal/protocol`, including WSS-only
types. WSS directions, authentication, errors and Close semantics live in the
handwritten source. Bundle each root YAML independently with internal references.

Client SDK generation belongs in the Android and Windows repositories.

## 7. Test placement

| Test kind | Location |
| --- | --- |
| Go unit tests | Colocated `*_test.go` |
| PostgreSQL integration tests | `test/integration/<domain>/` |
| HTTP/WSS contract tests | `test/contract/{http,wss}/` |
| Compose/release smoke tests | `test/smoke/` |
| Provider fakes | `test/fakes/{wechat,bailian}/` |
| Wire/database fixtures | `test/fixtures/` |

Test public behavior and domain facts. Use disposable databases and synthetic
credentials. Keep compatibility fixtures for supported contracts.

## 8. Local development, Compose, and release files

`deploy/compose/` owns `docker-compose.yml`, `.env.example`, `api.Dockerfile`,
`migrate.Dockerfile`, `postgres.Dockerfile` and the operator `README.md`.
`install.sh` orchestrates host deployment; `internal/release` implements in-image
migration using bundled SQL and the release manifest.

Group scripts under `scripts/{dev,codegen,db,quality,release}/`. Root `Makefile`
exposes development tasks; `.github/workflows/` owns CI. Dependency versions live
in Go/npm manifests and lockfiles.

Tool selection, quality gates and deployment acceptance live in the architecture
ticket. Installation, backup, retry and restore procedures belong in the Compose runbook.
