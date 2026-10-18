# Code Architecture

This document is the executable placement contract for all code changes in this repository. 
It defines where source, generated artifacts, schema, tests, and deployment support belong.

## 1. Non-negotiable runtime shape

One Go module, one long-running `api` process for HTTP and WSS. `cmd/pin-glide-api`
wires the API server, domain services and external clients. The offline
`cmd/pin-glide-db-tools` CLI owns migration, snapshot and local preparation; it
never serves network traffic.

## 2. Repository layout

```text
pin-glide-service/
├── cmd/
│   ├── pin-glide-api/         # API entrypoint and offline HTTP/WSS contract export
│   └── pin-glide-db-tools/   # Migration and snapshot CLI
├── internal/
│   ├── identity/             # Accounts, devices and credentials
│   ├── pairing/              # Pairings and Pairing Sessions
│   ├── model/                # Model configuration and temporary credentials
│   ├── delivery/             # Admission, deduplication and results
│   ├── api/
│   │   ├── server.go         # HTTP/WSS server composition and lifecycle
│   │   ├── httpapi/           # Huma routes, middleware and HTTP mapping
│   │   └── wss/               # Upgrades, frames and connection lifecycle
│   ├── adapters/             # Third-party system clients
│   │   ├── wechat/           # WeChat HTTP client
│   │   └── bailian/          # Bailian HTTP client
│   ├── protocol/             # Shared HTTP/WSS wire types and errors
│   ├── config/               # Environment configuration
│   ├── db/                   # Connections and Jet output
│   ├── observability/        # Logging, correlation and redaction
│   ├── release/              # Manifest, ledgers and migration execution
│   └── snapshot/             # Validated PostgreSQL snapshot publication
├── db/
│   ├── schema/{pin_glide,release}/
│   ├── migrations/{pin_glide,release}/
│   └── scripts/
├── dev/anon/                 # Local-only Anonymizer labels
├── protocol/asyncapi.yaml     # Handwritten WSS definitions
├── generated/
│   └── protocol/
│       ├── openapi.yaml       # Generated HTTP contract
│       └── asyncapi.yaml      # Generated WSS contract
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

### 3.1 Domain package

```text
internal/<domain>/
├── <domain>.go       # Domain types, states and errors
├── service.go        # Business operations and PostgreSQL transactions
├── store.go          # Domain-owned Jet queries and result mapping
└── adapter.go        # Adapter over another domain's service, where consumed
```

### 3.2 API package

```text
internal/api/
├── server.go         # HTTP/WSS composition and lifecycle
├── httpapi/
│   ├── router.go     # Huma route registration
│   ├── middleware.go
│   ├── errors.go     # Shared HTTP error mapping
│   └── identity/     # Repeat for other domains
│       ├── routes.go # HTTP input -> domain service -> response
│       └── dto.go    # HTTP wrappers; shared payloads in internal/protocol
└── wss/
    ├── router.go     # WSS route registration and refused-subscription reporting
    ├── connection.go # Pairing frame I/O and Close semantics
    ├── delivery.go   # Delivery and Receipt frames of the receiver channel
    ├── pairing.go    # Restricted pairing-session channel (M and receiver-side B)
    └── receiver.go   # Receiver delivery channel: upgrade, slot, heartbeat and presence
```

### 3.3 Adapter package

```text
internal/adapters/
├── wechat/
│   └── client.go     # WeChat HTTP protocol
└── bailian/
    └── client.go     # Bailian HTTP protocol
```

### 3.4 File splitting

- Split handwritten files approaching 500 lines by aggregate, use case or API operation.
- Use names such as `service_login.go`, `store_credentials.go`; keep each invariant together.

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
dev/anon/
└── <table>.sql               # Local-only masking labels for one table
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
job registration belongs in Data Scripts.

Add an anon script for each table with external-service credentials, URLs/hosts,
callbacks/webhooks, or service switches. Update its labels when those columns
change so local development uses inert values. See the
[masking examples](dev/anon/README.md).

### 4.3 Jet generation and generated Go types

Generate both schemas using disposable PostgreSQL. Output goes to
`internal/db/jetgen/{pin_glide,release}/{table,view,enum,model}/`.
Each domain's store maps generated result structs to domain types.

`internal/db/db.go` opens the shared pgx-stdlib `*sql.DB`; the pool stays in
`internal/db`. Services own their transactions, and each store takes the
executor it needs. Keep external network calls outside database transactions.

Use Jet by default and parameterized raw SQL when needed. Business logic stays in
Go; SQL functions/procedures handle database-local work such as cleanup. Quote
dynamic identifiers separately and bind values. Keep queries in their owning
stores without a generic repository/DAO layer.

## 5. Domain ownership map

Each row identifies the owning package, its responsibilities and API entrypoints.
Schema prefixes name relations within the shared database.

| Domain | Go package | Owns | API files | Schema prefix |
| --- | --- | --- | --- | --- |
| Identity | `internal/identity` | Accounts, Device Registrations, login/refresh sessions and credential lifecycle | `internal/api/httpapi/identity/` | `identity_` |
| Pairing | `internal/pairing` | Pairings, Pairing Sessions, authorization and recovery completion | `internal/api/httpapi/pairing/`, `internal/api/wss/pairing.go` | `pairing_` |
| Model | `internal/model` | Enabled model configuration and temporary-key acquisition | `internal/api/httpapi/model/` | `model_` |
| Delivery | `internal/delivery` | Delivery admission, deduplication, Receipt handling and terminal results | `internal/api/httpapi/delivery/`, `internal/api/wss/delivery.go` | `delivery_` |
| WeChat adapter | `internal/adapters/wechat` | WeChat requests, responses and provider errors | Called by identity service | none |
| Bailian adapter | `internal/adapters/bailian` | Bailian temporary-key API requests, responses and provider errors | Called by model service | none |
| Protocol | `internal/protocol` | Shared HTTP/WSS payload types and error definitions | Used by HTTP/WSS packages | none |
| Observability | `internal/observability` | Structured logs, correlation, redaction and PostgreSQL log writes | API middleware and WSS connection logging | `service_` |
| Retention | none; SQL-owned | Cleanup policy table and cleanup function | none | `retention_` |
| Release and database tools | `internal/release`, `internal/snapshot` | Release manifest, migration ledgers, Schema Sync, schema audits and validated snapshots | `cmd/pin-glide-db-tools` only | `release_` |

## 6. Generated artifacts and modification boundaries

| Artifact | Edit source | Generated output |
| --- | --- | --- |
| HTTP OpenAPI | Huma registrations and wire types | `generated/protocol/openapi.yaml` |
| WSS AsyncAPI | `protocol/asyncapi.yaml` and wire types | `generated/protocol/asyncapi.yaml` |
| Jet builders/models | `db/schema/{pin_glide,release}` | `internal/db/jetgen/{pin_glide,release}/` |

## 7. Test placement

| Test kind | Location |
| --- | --- |
| Go unit tests | Colocated `*_test.go` |
| Integration tests | `test/integration/` |
| HTTP/WSS contract tests | `test/contract/{http,wss}/` |
| Compose/release/local-lifecycle smoke tests | `test/smoke/` |
| Provider fakes | `test/fakes/{wechat,bailian}/` |
| Wire/database fixtures | `test/fixtures/` |

Test public behavior and domain facts. Use disposable databases and synthetic
credentials. Keep compatibility fixtures for supported contracts.

## 8. Local development, Compose, and release files

```text
Makefile                      # Local development and check entrypoints
install.sh                    # Host-side Compose release orchestration
version.json                  # Release version the auto-release pull request bumps
deploy/compose/
├── docker-compose.yml        # API, PostgreSQL and one-off db-tools service
├── .env.example              # Deployment configuration template
├── api.Dockerfile            # API image
├── db-tools.Dockerfile       # Migration/snapshot CLI, SQL and manifest
├── postgres.Dockerfile       # PostgreSQL with pg_cron
└── README.md                 # Installation, backup, retry and restore procedures
dev/anon/
└── <table>.sql               # Local-only masking labels
.github/workflows/
└── *.yml                     # CI and GitHub Release publication
scripts/
├── dev/                      # Local PostgreSQL, masking and API processes
├── codegen/                  # Jet, OpenAPI and AsyncAPI generation
├── db/                       # Disposable PostgreSQL and schema loading
├── quality/                  # Formatting, lint and tests
└── release/                  # Packaging and publication
cmd/pin-glide-db-tools/
└── main.go                   # Migration and snapshot entrypoint
internal/release/
└── *.go                      # In-image migration implementation
internal/snapshot/
└── *.go                      # Snapshot export, validation and retention
```
