# Code Architecture

Use this guide for code placement and review. Product behavior belongs in [service-requirements.md](docs/specs/service-requirements.md); decisions and open questions belong in the [architecture ticket](https://github.com/BuddyLabsCo/pin-glide-service/issues/3). Use [GLOSSARY.md](GLOSSARY.md) for names.

The paths below define the target layout. Create files when needed.

## Layout

```text
cmd/
  pin-glide-api/         # Service composition; offline -openapi export
  pin-glide-release/     # Deployment/migration CLI
  pin-glide-contracts/   # Shared-schema and WSS contract exporter
internal/
  identity/             # Account, Device Registration, credential rules
  pairing/              # Pairing and Pairing Session rules
  model/                # Model configuration and temporary-key use cases
  delivery/             # Admission, deduplication, delivery results
  protocol/             # Shared HTTP/WSS wire types and error codes
  adapters/
    httpapi/            # Huma routes, DTO mapping, HTTP errors
    wss/                # Connections, frame I/O, heartbeat, Receipt mapping
    postgres/           # Jet stores and PostgreSQL log writer
    wechat/             # WeChat HTTP client
    bailian/            # Bailian HTTP client
  db/                   # Connection setup
    jetgen/             # Generated release/pin_glide SQL builders and models
  config/               # Process configuration and secret handling
  release/              # Helm-derived migration, ledger and audit code
protocol/asyncapi.yaml   # WSS channel/operation definitions
generated/             # Shared schemas and intermediate contract output
db/                    # Desired schemas, migration scripts, Data Scripts
scripts/               # dev/, db/, quality/, codegen/, release/
test/                  # integration/, smoke/, fixtures/
deploy/compose/        # docker-compose.yml, .env.example, Dockerfiles, README.md
install.sh              # Helm-derived host installer
openapi.yaml             # Generated HTTP release contract
asyncapi.yaml            # Generated WSS release contract
```

## Go boundaries

- `cmd` wires dependencies. Put no business rules there.
- Business modules own types, errors, use cases and required I/O interfaces. Adapters implement those interfaces.
- Business modules do not import Huma, HTTP/WSS connection types, Jet, pgx or adapter packages. Keep cross-module calls acyclic.
- Transport adapters decode/validate wire input, call a use case and map its result. No SQL or multi-step business logic in handlers.
- Keep Jet models inside PostgreSQL/release code. Map them to business types; never expose them as domain objects or API DTOs.
- `internal/protocol` owns wire types only. It imports neither business modules nor adapters. Put mapping at the transport edge.
- Each PostgreSQL store owns its domain's relations. Cross-domain changes go through the owning module's interface.
- Business use cases define transaction scope through a store operation. The PostgreSQL adapter executes it atomically; keep transaction objects inside the adapter.
- Pass contexts through I/O. Give each goroutine an owner, cancellation path and shutdown/join path. Keep transport disconnection separate from the business operation lifetime defined by the spec.
- Handle returned errors; preserve typed causes with `%w`. Map public errors at transport boundaries and redact secret-bearing input.
- Use `lower_snake_case.go`, matching package/directory names, and colocated `*_test.go`. Split by use case; keep handwritten Go/shell files within the inherited 500-line limit.
- Add interfaces at actual I/O seams. Avoid generic repositories, pass-through services and `common/utils/shared/misc` packages.

Import boundaries are review rules; Helm's standard lint set does not enforce them automatically.

## PostgreSQL and Jet

Use PostgreSQL schemas `pin_glide` for application data and `release` for deployment metadata. Extension-owned `cron` objects stay outside application schema generation.

```text
db/schema/<schema>/
  00_extensions.sql
  10_enums/<object>.sql
  20_tables/<table>.sql
  30_views/<view>.sql
  40_functions/<function>.sql
  50_procs/<procedure>.sql
db/migrations/<schema>/issue_<number>_<sequence>_<summary>.sql
db/scripts/issue_<number>_<sequence>_<summary>.sql
```

- One primary object per desired-schema file. Keep a table's columns, constraints, indexes and comments in its file.
- Tables: `<domain>_<plural_noun>`. Views/functions/procedures: `<domain>_vw_*`, `<domain>_fn_*`, `<domain>_sp_*`.
- Keys/indexes: `pk_<table>`, `fk_<referenced_table>_<referenced_columns>`, `idx_<table>_<key_columns>`, `uidx_<table>_<key_columns>`. Preserve column order; omit INCLUDE columns from names. Use unique indexes instead of unique constraints.
- Checks/triggers: `chk_<description>`, `trg_<table>_<description>`. Use lowercase snake case identifiers.
- Add `COMMENT ON` for tables, columns, views, enums and routines. Routine comments list `parameters:` and each parameter name.
- Use Jet for normal queries. Parameterize raw SQL where required. Never concatenate values into SQL.
- Edit desired SQL for target structure. Add structural transition scripts only when schema diff is insufficient; put initialization/backfill/repair in Data Scripts.
- Scripts must be safe to rerun. Never edit, delete or reformat processed scripts. Generate `order.txt` during exact-commit packaging from full Git history.
- Generate Jet from desired schema in disposable PostgreSQL. Commit generated output; never hand-edit it.
- Add/backfill/validate before dropping or renaming data. Test every declared upgrade origin; a matching target schema alone does not prove a safe upgrade.
- Give each table one retention policy. Separate durable configuration, temporary transactions and diagnostic logs; do not mix them by row status.
- Keep long-lived Receiver credentials separate from expiring Sender/session data. Do not cascade configuration deletion into retained transactions/logs or session deletion into activated authorization.
- Store cleanup targets, retention days and date columns in `pin_glide.retention_policies`. Put its table/function DDL in `20_tables/retention_policies.sql` and `40_functions/retention_fn_cleanup.sql`.
- The cleanup function reads policy rows and runs sequential dynamic DELETEs. Quote schema/table/column separately with `%I`; bind cutoff values with `USING`. Validate targets/types and restrict configuration writes/job privileges. Do not hardcode per-table date columns or add restart checkpoints.
- Keep message payloads, client keys and raw credentials out of database rows/logs. Use historical scalar IDs where mutable configuration must not control history retention.

## Generated contracts

| Edit source | Generate output |
| --- | --- |
| `internal/protocol` Go wire types | `generated/protocol/schemas.json` |
| Actual Huma registrations in `internal/adapters/httpapi` | Root `openapi.yaml` |
| `protocol/asyncapi.yaml` plus generated shared schemas | Root `asyncapi.yaml` |
| `db/schema/{release,pin_glide}` | `internal/db/jetgen/{release,pin_glide}` |

- Export HTTP with `go run ./cmd/pin-glide-api -openapi`; export shared/WSS definitions with `go run ./cmd/pin-glide-contracts`. Wire both into `scripts/codegen/generate.sh`.
- Register descriptions for raw WSS HTTP handshakes as well as Huma routes. Keep message directions, authentication, errors and Close behavior in AsyncAPI.
- Export WSS-only types directly from the registry; Huma may prune types unused by HTTP. Validate the shared JSON Schema subset across OpenAPI/AsyncAPI.
- Bundle root YAML to internal references only. Edit sources, never generated YAML/Jet files.
- Check actual route/message inventory and wire behavior against both descriptions. Keep compatibility fixtures for supported contract releases.
- Regenerate and inspect both tracked drift and unexpected untracked output.

## Checks and tests

Copy Go/SQL checks and pinned dependencies from [Helm Gateway at ba375819](https://github.com/BuddyLabsCo/helm-gateway/tree/ba375819b98bcd76fc27d7a15b2523aa62837c6f). Use the [reuse inventory](docs/research/helm-gateway-go-sql-deployment-reuse.md) when bringing over tooling. Remove unrelated frontend/Redis/River steps; preserve root npm for Schemalint.

These are the target command interfaces; add their scripts/configs with the tooling implementation.

| Command | Checks/configuration |
| --- | --- |
| `make format-check` / `make format` | gofmt; `scripts/quality/format.sh` |
| `make lint` | `.golangci.yml`: standard errcheck/govet/ineffassign/staticcheck/unused; source-size script |
| `make lint-sql` / `make format-sql` | Disposable schema load, catalog checks, Schemalint, SQLFluff; `.schemalintrc.js`, `.sqlfluff`, `scripts/quality/` |
| `make generate` | Disposable PostgreSQL/Jet and both contract exports; `scripts/codegen/generate.sh` |
| `make check-contracts` | Validators, references, coverage and compatibility; `scripts/quality/check-contracts.sh`, `.redocly.yaml` |
| `make test` | Node script tests and `go test ./...` |
| `make build` | `go build ./...` |
| `go test -tags=integration ./test/integration/release` | Database evolution and migration fixtures |
| `go test -tags=integration ./test/integration/contracts` | HTTP/WSS contract fixtures |

- Pin Go tools in `go.mod`/`go.sum`; Node tools in root `package.json`/lockfile. Add `@asyncapi/cli@6.2.0` and `@redocly/cli@2.62.1` for contract validation.
- Contract validator commands: `npm exec -- redocly lint openapi.yaml` and `npm exec -- asyncapi validate asyncapi.yaml --fail-severity error`.
- Keep quality/generated/test/build CI jobs separate. Fail on violations/drift; require a scoped reason and maintainer review for suppressions.
- Run `make format-sql` only on desired schema; historical scripts are immutable.
- Put pure-rule tests beside Go code, database tests under `test/integration/<domain>/`, public wire fixtures under `test/integration/contracts/`, and Compose/development harnesses under `test/smoke/`.
- Test public behavior, not private SQL/helper structure. Use disposable local databases and synthetic fixtures; never production data/credentials.
- Default `make test` does not run integration-tag or deployment smoke tests. Run those explicitly for relevant schema/release changes.

## Deployment code

- Adapt Helm's `install.sh`, `internal/release`, manifest packaging, migration image, publisher and regression tests together. Keep artifact/source/version identity checks and recovery semantics.
- Use `deploy/compose/{api,migrate,postgres}.Dockerfile`; keep Compose and its runbook beside them. Put tooling under `scripts/{dev,db,codegen,quality,release}`.
- Bundle desired SQL, immutable scripts, manifest and pinned psqldef in the matching migration image.
- Keep `release` ledger/version records separate from expiring schema audits. Preserve phase ordering and replay-safe bootstrap.
- Manage cron jobs through Data Scripts; configure the pg_cron image/preload separately from application DDL. Preserve scheduler state during upgrade/restore.
- Publish matching images and both validated YAML assets from the same source commit. Attach both before publishing a Release; published assets are immutable.
- Keep operational install/backup/retry/restore commands in `deploy/compose/README.md`, not in this guide. Add no custom cleanup recovery mechanism.

## Review

- Correct module owns the change; imports respect the boundaries above.
- No business rules/SQL in transport and no generated models outside PostgreSQL/release code.
- Authorization and atomic state changes remain at their owning seam; contexts/errors/resources are handled.
- Schema, scripts, Jet and contract outputs change together where required.
- One table has one retention policy; cleanup cannot erase durable authorization or retained history by cascade.
- No payload/secret leakage, duplicated wire schema, destructive untested upgrade or edited migration history.
- Relevant checks and public-behavior tests pass; generated output is reproducible.

Update this guide when ownership, dependency direction, public tooling interfaces or generated-artifact placement changes. Keep product rules, storage inventories, acceptance scenarios, decision history and open questions in the specification/ticket.
