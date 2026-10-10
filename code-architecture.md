# Code Architecture

Status: review draft for [Clarify service code architecture, quality gates, and persistence evolution](https://github.com/BuddyLabsCo/pin-glide-service/issues/3). The functional contract is adopted; this document consolidates the architecture decisions and concrete proposed implementation paths for final HITL review. It does not claim those paths, tools, tables, tests or release assets already exist.

## 1. Start here

Read [the approved functional specification](docs/specs/service-requirements.md) for behavior and [GLOSSARY.md](GLOSSARY.md) for domain language. For Go/SQL verification and deployment, reuse `BuddyLabsCo/helm-gateway@ba375819b98bcd76fc27d7a15b2523aa62837c6f`; the [reuse report](docs/research/helm-gateway-go-sql-deployment-reuse.md) identifies its implementation, dependency closure and limitations. Copy the existing machinery and tests, substituting product identity and removing unrelated Console, worker, Redis/River and legacy Helm history. Do not build another linter suite, migration framework or deployment state machine.

The service uses one Go module, one active Go process, Huma HTTP, authenticated Go WSS and PostgreSQL/Jet. Android owns inference/encryption and Windows owns decryption/display. Service-owned shared contracts are versioned release assets; client generation/adaptation remains client-owned.

Privacy is a hard storage boundary: PostgreSQL and logs contain no SMS/OTP plaintext, client encryption keys, pairing/message ciphertext, raw service credentials, or raw secret-bearing provider responses. Provider parent credentials and TLS secrets stay in protected deployment configuration. No offline message mailbox, payload recovery, distributed routing or repository performance-testing program is introduced.

## 2. Repository placement

Existing artifacts are `AGENTS.md`, `GLOSSARY.md`, `docs/agents/`, `docs/specs/service-requirements.md`, source drafts and research reports. Everything in the implementation table below is **proposed**, including command/config paths. Implement these entries when the corresponding product or tooling work is authorized; do not scaffold empty modules merely to fill the table.

| Path | Responsibility |
| --- | --- |
| `go.mod`, `go.sum` | One module; runtime/tool dependencies and checksums |
| `cmd/pin-glide-api/main.go` | Compose dependencies, start/stop the service; offline `-openapi` export branch |
| `cmd/pin-glide-release/main.go` | Adapted Helm release CLI and migration entrypoint |
| `cmd/pin-glide-contracts/main.go` | Offline shared-schema and explicit WSS contract assembly |
| `internal/identity/` | Account/device identity, scoped credentials, renewal/replay/logout/revocation rules |
| `internal/pairing/` | Pairing Session state, Sender authority, capacity, dual completion, replacement and Receiver recovery |
| `internal/model/` | Enabled configuration snapshot and temporary-key acquisition use case; no inference |
| `internal/delivery/` | Per-entry validation, atomic Delivery claim, duplicate joining, terminal statuses and result lookup |
| `internal/protocol/` | Canonical Go HTTP/WSS wire types, error codes, message identifiers and channel metadata |
| `internal/adapters/httpapi/` | Huma registrations, authentication extraction, request decoding and business-to-wire mapping |
| `internal/adapters/wss/` | Upgrade/admission, owned connection slots, writes, Receipts, heartbeat, presence and opaque live relay |
| `internal/adapters/postgres/` | Jet-backed identity/pairing/model/delivery stores and PostgreSQL diagnostic log writer |
| `internal/adapters/wechat/`, `internal/adapters/bailian/` | Provider requests, response validation, timeouts and sanitized failures |
| `internal/db/`, `internal/db/jetgen/` | Connection setup and generated `release`/`pin_glide` Jet packages, retaining Helm's generation shape |
| `internal/config/` | Validated deployment configuration; secret redaction |
| `internal/release/` | Adapted Helm manifest/ledger/SQL guard/schema sync/audit execution |
| `protocol/asyncapi.yaml` | Handwritten WSS channels/operations/security/correlation/error/Close descriptions; references generated schemas |
| `generated/protocol/schemas.json` | Generated shared schema registry, including WSS-only types |
| `generated/protocol/asyncapi.yaml` | Assembled AsyncAPI input with generated components |
| `openapi.yaml`, `asyncapi.yaml` | Complete, self-contained generated contract outputs, committed and released together |
| `db/schema/release/`, `db/schema/pin_glide/` | Sorted declarative desired-schema SQL, retaining Helm's object-file conventions |
| `db/migrations/release/`, `db/migrations/pin_glide/`, `db/scripts/` | Immutable structural scripts and Data Scripts; initial setup/explicit cron job changes |
| `db/migrations/order.txt` | Exact-commit packaging output from full first-parent Git history; not a handwritten ordering list |
| `scripts/quality/`, `scripts/db/`, `scripts/codegen/`, `scripts/release/` | Adapted Helm checks, disposable database, generation and packaging/publication machinery |
| `scripts/dev/`, `test/integration/`, `test/smoke/` | Isolated local checks, public HTTP/WSS scenarios, database evolution and deployed-development acceptance |
| `deploy/compose/` | Compose, environment example, service/migrate/PostgreSQL Dockerfiles and operator README |
| `install.sh` | Adapted Helm host installer, attempt state, backup/retry/recovery lifecycle |
| `.github/workflows/ci.yml`, release workflows | Helm quality/generated/test/build and release automation, with explicit protocol additions |

Use concrete files such as `internal/identity/credentials.go`, `internal/pairing/sessions.go`, `internal/delivery/service.go` and `internal/adapters/postgres/deliveries.go` as code arrives. File splits follow responsibility and the inherited handwritten-source size limit, not a prebuilt framework.

## 3. Module interfaces and dependency rules

Business modules own behavior and small I/O interfaces at real seams; adapters implement those interfaces. Constructors accept required dependencies. A separate interface is justified by actual external I/O/testing variation, not by wrapping every concrete function. Avoid a generic Repository/Service hierarchy or pass-through module.

Permitted directions:

- `cmd` assembles business modules, adapters and configuration.
- HTTP/WSS adapters import business modules and protocol types. PostgreSQL/provider adapters import the business interfaces/types they implement.
- Business modules use standard Go and their own domain types. They do not import Huma, raw HTTP/WebSocket connection types, Jet, pgx, PostgreSQL adapters or provider SDK types. Keep use-case orchestration and authorization in the owning business module.
- `internal/protocol` has no imports of business modules, transport, persistence or deployment. Mapping code at adapters prevents wire changes from becoming database/domain coupling.
- Jet artifacts are imported by database adapters and release persistence only. `internal/release` orchestrates target database changes and owns release metadata; it does not import identity/pairing/delivery modules.
- Cross-business calls carry explicit authority/results through small interfaces and remain acyclic. For example, Delivery obtains a checked admission result and freezes it rather than reimplementing Pairing authority or pre-send revocation rules.

An import from `internal/delivery` to `internal/adapters/postgres` violates this document. The adopted Helm lint configuration does **not** automatically reject that direction: reviewers enforce it. Do not claim depguard/custom architecture checks exist. New enforcement is a separate explicit change, not part of this baseline.

At trust boundaries, transport validates framing/encoding/required fields and extracts authority; business modules validate scope, ownership, eligibility and state transitions; PostgreSQL enforces atomicity/uniqueness/serialization. Provider adapters validate untrusted responses. Translate errors once to the shared protocol representation, including middleware, routing fallbacks and pre-101 failures. Preserve per-entry upload validation so one malformed entry does not suppress valid entries.

WSS owns I/O lifetimes and connection-ID cleanup; Delivery owns admission/result lifecycle. Registered deliveries survive HTTP disconnect within their existing budget. Shutdown stops admission, closes WSS with the agreed meanings and finalizes interrupted work when possible; restart never resumes ciphertext. Use bounded contexts/deadlines and explicit cancellation/joining. Coordinate Compose shutdown grace with those bounds; an exited container alone is not proof every admitted Delivery completed.

## 4. Protocol sources, generation and release

### Canonical definitions

`internal/protocol` owns shared wire types and error/message identifiers. Actual Huma route registration owns HTTP operation descriptions. `protocol/asyncapi.yaml` owns explicit WSS channel/operation descriptions and references generated shared schemas, rather than duplicating payload definitions.

The API's offline `-openapi` path registers the same HTTP routes and error schemas without starting database/provider/listener dependencies, then serializes Huma OpenAPI 3.1 YAML. Raw WSS upgrade endpoints also have registered HTTP handshake metadata: address, credentials, subprotocol, 101 and rejection responses. Huma generation alone does not describe the later WSS conversation.

The protocol exporter enumerates all required Go wire types directly into `generated/protocol/schemas.json`; do not recover event-only schemas from a Huma HTTP document after unused-schema pruning. Explicitly validate the common JSON Schema subset used by OpenAPI 3.1 and AsyncAPI 3's default Draft-07-based schemas. Reject unsupported constructs or supply a tested explicit representation; no implicit lossless conversion is assumed. Shared names, required fields, nullable values, enums and error meanings must agree across both documents.

### Proposed exact command interface

These commands become runnable with implementation; they are not current test results:

```sh
make generate
go run ./cmd/pin-glide-api -openapi > openapi.yaml
go run ./cmd/pin-glide-contracts
npm exec -- redocly lint openapi.yaml
npm exec -- asyncapi validate asyncapi.yaml --fail-severity error
make check-contracts
```

`cmd/pin-glide-contracts/main.go` is the bounded shared-schema/WSS assembly exporter; it belongs with generation tooling and introduces no runtime endpoint or client SDK. `scripts/codegen/generate.sh` retains Helm's disposable-schema Jet generation for `release` and `pin_glide`, exports HTTP YAML, then invokes this exporter. It emits the assembled AsyncAPI input; pinned AsyncAPI bundling writes root `asyncapi.yaml`. Any external OpenAPI references are likewise bundled before committing root `openapi.yaml`. Both root documents contain only internal references and need no running server or network fetch to consume.

Use exact root devDependency pins `@asyncapi/cli@6.2.0` and `@redocly/cli@2.62.1`, committed npm lockfile and inherited Node 24. Sources: [AsyncAPI CLI](https://www.asyncapi.com/docs/tools/cli/usage), [Redocly lint](https://redocly.com/docs/cli/commands/lint), [bundle](https://redocly.com/docs/cli/commands/bundle). Selected versions and actual exporter dialect behavior must be exercised during implementation. `.redocly.yaml` holds explicit structural/reference validation; AsyncAPI uses its pinned parser via the command above. These are target-specific protocol additions, not replacements for Helm's Go/SQL tools.

`make check-contracts` invokes `scripts/quality/check-contracts.sh`: both validators, self-contained-reference check, regeneration/working-tree check and the public contract tests under `test/integration/contracts/`. Tests compare actual HTTP/WSS registration inventory to descriptions, validate both directions/authentication/handshake/error/Receipt/Close schemas, and exercise approved behavior through adapter interfaces. A processable upload must remain 207 with one result per original input index. Arrival Receipt never means decryption/display success.

Commit generated Jet and root YAML. Preserve Helm's tracked diff check and also detect unexpected untracked files under generated output paths; `git diff` alone misses them. This is a bounded output-consistency fix, not another generation framework. Contract tests check compatible v1 additions against immutable release fixtures selected in `test/integration/contracts/fixtures/`; retain supported older shapes and negative cases rather than testing a schema against itself.

### Release and consumer compatibility

Reuse Helm's release-PR/tag/source/image identity, private GHCR and retry/no-overwrite checks. Build service and migrate images, both descriptions and checksums from the **same tag/source commit**. Extend the publisher only to create an unpublished draft Release, attach and verify both complete YAML assets, then publish. A failure leaves the Release unpublished; retries verify identity/content rather than silently replacing an already published asset. Corrections need a new release.

Clients retrieve a pinned release at build time, for example:

```sh
gh release download v1.0.0 --repo BuddyLabsCo/pin-glide-service \
  --pattern openapi.yaml --pattern asyncapi.yaml --dir contracts/v1.0.0
```

Private repository retrieval needs a build-time credential with repository/release read access; never ship it in an app or require client runtime downloads. Android/Windows pin their own generators/adapters and own generated clients.

Continue supporting released v1 clients. Compatible optional additions are allowed; required fields, field meanings, credential authority and Receipt milestone remain stable. A new server-initiated WSS type cannot be sent to an old v1 client that treats unknown types as protocol errors. Breaking semantics require a new negotiated HTTP/WSS version plus explicit migration/retirement decisions. A service release version, database milestone and protocol version are different identifiers; release notes list supported protocol versions and upgrade origins.

## 5. Go/SQL quality gates

Reuse the reference versions/configuration, including Go 1.27.1, Huma 2.39.1, Jet 2.16.0, pgx 5.11.0, psqldef 3.11.24, golangci-lint 2.14.0, Schemalint 2.3.2 and SQLFluff 4.3.0. Preserve corresponding module/tool entries, npm lock, wrappers and regression tests. Update pins together with relevant evidence; mutable infrastructure tags are not falsely described as digest pins.

| Check | Configuration/source | Proposed root command | Scope and execution |
| --- | --- | --- | --- |
| Go formatting | `scripts/quality/format.sh`, `.golangci.yml` | `make format-check`; fix with `make format` | Existing handwritten tracked/untracked Go, excluding generated Jet/output; editor, developer, quality CI |
| Go static analysis | `.golangci.yml`, `scripts/quality/golangci-lint.sh` | `make lint` | Helm standard errcheck/govet/ineffassign/staticcheck/unused; pinned binary; developer/quality CI |
| File size | `scripts/quality/check-file-size.sh` | Included in `make lint` | Inherited 500-line scope/exclusions; developer/quality CI |
| SQL shape/metadata/style | `.sqlfluff`, `.schemalintrc.js`, `scripts/quality/sql-lint.sql`, plugin directory and ignore file | `make lint-sql`; desired-schema fix with `make format-sql` | Disposable PostgreSQL + psqldef apply, catalog rules, Schemalint and SQLFluff; developer/quality CI |
| Generated drift | `scripts/codegen/generate.sh`, output working-tree checks | `make generate` followed by tracked/untracked drift check | Jet and both YAML outputs; developer/generated CI |
| Contract structure/coverage | `.redocly.yaml`, `scripts/quality/check-contracts.sh`, contract fixtures/tests | `make check-contracts` | Both descriptions, references, HTTP/WSS inventory and compatible behavior; developer/CI |
| Behavioral tests | `scripts/quality/test.sh` | `make test` | Node script tests + `go test ./...`, no Console tests; developer/test CI |
| Compilation | `Makefile` | `make build` | `go build ./...`, no Console build; developer/build CI |
| Database evolution | `test/integration/release/` and retained Helm fixtures | `go test -tags=integration ./test/integration/release` | Explicit isolated Docker/PostgreSQL suite; developer/pre-release development validation |

`govet` is part of golangci; there is no additional standalone vet requirement. Do not add extra linters or a mandatory race/performance/coverage gate under the name of parity. Use existing Go tests for cancellation, single ownership, concurrency and error handling; static analysis does not prove those properties.

Preserve Helm SQL conventions/checkers: named primary/foreign keys, standalone unique indexes rather than unique constraints, object-name shapes, comments and parameter documentation. SQLFluff formats desired schema only; immutable processed scripts are never reformatted. The lint loader applies target schema but does not prove upgrade convergence or script rerunnability. The release executable separately parses SQL AST, checks immutable ledger/manifest and runs apply/check.

Checks fail on reported violations/drift/build/test failures; no unreviewed warning baseline or blanket suppression. A necessary local exception states its reason, scope and removal condition and is reviewed by the repository maintainer. Use inherited SQL/Schemalint exception mechanisms separately; one allowlist does not suppress the other. Generated exclusions do not exempt generated code from compiling.

CI retains Helm's quality/generated/test/build separation. Explicit integration-tag and real deployment/restore smoke are separate release-validation commands, not falsely claimed as default Helm CI coverage. All generation, evolution tests and fixtures use disposable databases and synthetic data, never production copies or credentials.

## 6. PostgreSQL ownership and retention

### One table, one scheduled cleanup policy

There are exactly three tiers: no scheduled deletion, seven days, thirty days. **No table mixes tiers**, including active/historical variants. No per-row retention class, `status` exception, nullable time exemption or generic mixed event table is used. Business operations may physically delete no-scheduled-deletion rows when authorized; this is independent of cron retention. Configuration does not require universal soft deletion or a history of every field update.

The table names and timestamp choices below are concrete review proposals implementing that rule. Use the inherited SQL naming/comment rules and one desired object per appropriate source file. Every cleanup timestamp is non-null and indexed. Times are server-generated UTC instants, not client-provided SMS/display timestamps.

| Table | Owning module; key/constraint | Data and sensitivity | Scheduled cleanup |
| --- | --- | --- | --- |
| `pin_glide.identity_accounts` | identity; internal ID, unique AppID/OpenID | Account mapping; sensitive identity | None |
| `pin_glide.identity_devices` | identity; device ID and Account ownership | Installation identity, role, revocation and Receiver issuance generation | None; preserve minimum permanently revoked identity needed for agreed 409/404 behavior |
| `pin_glide.pairing_associations` | pairing; Pairing ID, unique active Sender/Receiver association | Effective authorization and own alias; no keys | None; authorized removal/replacement can physically remove old association |
| `pin_glide.model_configurations` | model; config ID/revision and enabled-selection invariant | Non-secret Bailian settings and response snapshot | None |
| `pin_glide.identity_receiver_credentials` | identity; Receiver ID, unique digest/current generation | Long-lived R digest/revocation metadata only | None; separate from expiring credentials |
| `release.release_migration_scripts` | release; schema/filename and checksum | Authoritative processed structural scripts, SQL snapshot/executor | None; retain Helm ledger semantics |
| `release.release_data_scripts` | release; filename/checksum | Authoritative processed Data Scripts and snapshots | None |
| `release.release_deploy_history` | release; append milestone ID | Database Release history, including latest required milestone | None |
| `cron.job` and its extension metadata | deployment; stable job name/owner | Cleanup definitions, configured target and active state | None; extension owns its schema, explicit scripts manage jobs |
| `pin_glide.identity_sender_credentials` | identity; digest, Sender/family/scope | M/U/F digest, issue/expiry/consumption/revocation metadata; no raw token | 7 days using `issued_at` |
| `pin_glide.identity_login_families` | identity; family ID, Sender/current generation | Renewal/revocation state, latest accepted refresh time | 7 days using `last_refreshed_at`; every successful login/refresh updates it atomically |
| `pin_glide.identity_refresh_operations` | identity; family/operation ID, accepted attempt | Predecessor/ordering guards; preserve old-operation semantics | 7 days using first `accepted_at`; later attempts do not reset it |
| `pin_glide.pairing_sessions` | pairing; session ID, allocated Receiver/pending Pairing IDs | Fixed mode/participants, B digest/expiry, reservations, completion, issuance grant/attempt state; no relay payload | 7 days using `created_at` |
| `pin_glide.delivery_transactions` | delivery; unique Sender/message/Receiver | Admission/dedup, metadata, processing deadline and terminal result | 7 days using `created_at` |
| `pin_glide.delivery_rejections` | delivery; rejection ID/request/index | Per-entry safe metadata and internal reason; does not replace admitted Delivery | 7 days using `created_at` |
| `pin_glide.service_logs` | runtime logging; event ID/request correlation | Sanitized runtime/provider/auth/connection diagnostic events | 30 days using `occurred_at` |
| `release.release_schema_audits` | release; audit ID | psqldef apply/check output, exit, elapsed time, executor | 30 days using inherited `executed_at` |
| `cron.job_run_details` | pg_cron | Job execution diagnostics, including interrupted runs | 30 days using proposed project-added `recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP`, with an index |

Do not put long-lived R into the seven-day credential table. Do not put authoritative migration ledgers into the audit table. A still-active login family may be older than seven days: its renewal timestamp slides with F validity, while every row in that table follows the same time predicate. Expiry/credential validity is enforced at request time; waiting for cron never extends authority.

The pending Receiver and Pairing references live in the seven-day session until activation creates effective configuration records. Activated associations and R live independently of that session; deleting a session cannot revoke them. First valid R presentation can occur after the old grant/session has been purged and must still authenticate using the durable R/generation. Preserve the Receiver's current issuance-generation guard in its durable device state so old grants cannot affect new recovery. Public IDs alone never grant authority. Expired sessions release capacity and stop redemption at five minutes; retaining a row for seven days never extends that lifecycle.

Configuration IDs stored in Delivery/log/control history are historical identifiers, not cascade-delete links. Business deletion of a Pairing/device must not erase seven/thirty-day records early. Avoid cross-tier `ON DELETE CASCADE`; use domain transactions for authorized lifecycle changes and non-cascading historical identifiers where necessary. Session/B cleanup must not cascade into R/Pairing configuration. Same-tier control rows must also remain safe when family/credential cleanup runs concurrently with refresh; serialize renewal/issuance/revocation with the relevant database locks. A delayed request cannot upsert a purged/expired family or recreate deleted operation authority; it affects only its original family, never a later fresh-login family.

### Direct cron cleanup

Reuse Data Scripts to register/update the jobs with a stable deployment owner/name and verify resulting `cron.job` state.

Native pg_cron `start_time` and `end_time` can both be null before actual execution; `COALESCE` is not a complete cleanup clock. The concrete review proposal adds `recorded_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP` and an index to `cron.job_run_details` through an explicit immutable Data Script after extension setup; Data Scripts run on fresh installation too. Existing rows with unknown age receive the installation/backfill time once, retaining them for at most another thirty days plus daily scheduling lag without fabricating their age. This is a target-specific schema addition to extension-owned history, not psqldef management of the `cron` schema or an extension fork. Extension upgrades must preserve and test the column and index. Sources: [native nullable timestamps](https://github.com/citusdata/pg_cron/blob/v1.6.7/pg_cron--1.2--1.3.sql), [InsertJobRunDetail](https://github.com/citusdata/pg_cron/blob/v1.6.7/src/job_metadata.c).

The proposed application schema is `pin_glide`; examples are SQL contracts for the implementation, not executed migrations:

```sql
DELETE FROM pin_glide.delivery_transactions
WHERE created_at < CURRENT_TIMESTAMP - INTERVAL '7 days';

DELETE FROM pin_glide.identity_login_families
WHERE last_refreshed_at < CURRENT_TIMESTAMP - INTERVAL '7 days';

DELETE FROM pin_glide.service_logs
WHERE occurred_at < CURRENT_TIMESTAMP - INTERVAL '30 days';

DELETE FROM cron.job_run_details
WHERE recorded_at < CURRENT_TIMESTAMP - INTERVAL '30 days';
```

Apply the corresponding simple timestamp filter to every seven/thirty-day table; no application job queue or Go cleanup fallback. Keep the adopted daily 03:00 Asia/Shanghai schedule; a missed run extends actual retention, and daily deletion gives an approximate 7-8/30-31-day online lifetime rather than exact second-level expiry. A transaction rollback can delay cleanup; monitor through thirty-day diagnostic data.

Time-filter cleanup must preserve the sliding seven-day F lifetime: credentials/operations cannot be deleted while still needed for valid-token/replay/attempt evaluation. A new refresh cannot revive a logically expired family merely because cron has not deleted it. Received/failed Delivery rows use creation time, not last query/duplicate attempt, so status lookup does not extend dedup lifetime. Existing retained-row existence controls dedup, not an independent age filter.

PostgreSQL owns diagnostic events via the adapter in `internal/adapters/postgres/logs.go`; standard Go slog can retain Helm's stdout output for operators. Persist only explicit sanitized fields. Diagnostic writes do not replace authoritative business-state transactions. Database log-write failure must avoid recursive logging and may fall back to operator-managed stderr; no impossible PostgreSQL logging guarantee is made during database failure. Docker/host logs are entirely operator-managed and have no application-selected TTL.

### Other stores and memory

Development/production each own distinct Compose projects, databases, named volumes, endpoints and protected `.env`/secret files. Host attempt JSON, `.env.bak` and full-database dump files retain Helm's format/state semantics and manual retention; no seven/thirty-day SQL policy applies. Preserve unfinished recovery dependencies during operator cleanup. These local backups are not an off-host disaster-recovery guarantee or immediate physical erasure when online rows are deleted.

WSS slots, connection IDs, heartbeat/presence, live Delivery owners/waiters and ciphertext are memory-only. There is no database row for every Ping/Pong or an offline payload queue. Temporary Bailian keys are returned to the authorized Sender without adding a secret-bearing database cache. Protocol/YAML/Jet artifacts and image/source releases persist in version control/registry, not runtime log tables.

## 7. Evolution, deployment and recovery

### Desired schema versus ordered changes

Keep target DDL in `db/schema/{release,pin_glide}` and immutable structural/Data Scripts in their separate paths. Generate Jet against a disposable database with that desired schema. The migration image bundles the target DDL, all required immutable history, exact-commit manifest and pinned psqldef plus adapted release binary. Directly copying a binary without its SQL/manifest is not a matching artifact.

Preserve Helm order: release scripts -> release schema apply/check -> deferred ledger/legacy work applicable to this project -> application scripts -> application schema apply/check -> Database Release milestone -> Data Scripts. There is no River phase or Helm legacy-ledger import in a new Pin Glide database. Data failure after the milestone is still a failed attempt; milestone alone is not success. Each application/data script commits with its ledger; release bootstrap can replay committed scripts before ledgering. All scripts must be safe to rerun and processed checksums remain immutable.

Support upgrades only from published origins explicitly exercised/listed by the target release. Initial release supports explicit fresh install; future releases bundle needed history and declare compatible origins in release notes. Do not invent a new fixed N-1 window or promise every old version automatically works because a target schema exists. Host prep retains Helm's numerical Database Release downgrade rejection/equal-version redeploy behavior; bypassing the installer does not inherit all its checks.

Use additive evolution and backfill before removal when old representation matters. Example: add an optional device field with a safe default, generate Jet, backfill/validate existing rows if required, then let new code consume it without breaking v1. For changing credential/status representation, introduce the new representation alongside the old, transform/verify with immutable scripts, preserve existing valid credentials/meaning and supported v1 output, and remove the old representation only in a separately validated cutover. Opaque token digests cannot be rederived from missing raw tokens: use compatible interpretation or an explicit approved credential transition, not a fabricated backfill.

### Installer and pg_cron adaptation

Retain Helm `install.sh` behavior, attempt JSON, project-directory flock, database advisory lock, guarded backups, retained migration-container reconciliation, cancellation restrictions and standalone-start escape hatch. Substitute service names coherently; service and one-off migrate images share release/source, PostgreSQL persists independently. No normal release resets a database/volume or silently changes PostgreSQL major version.

First install: prepare protected configuration/private registry access and matching images -> start the pg_cron-capable database -> explicit initial schema/data/cron setup -> start one service -> health and development acceptance. Updates: explicit prep -> pull matching images -> stop admission/application writes -> control cron/external writers -> complete stopped-writer backup -> ordered migration/apply/check/data -> verify job state -> healthy service startup -> restore intended scheduling. Keep Docker/registry/proxy setup as documented operator prerequisites.

The PostgreSQL image installs a compatible pinned pg_cron library, configures preload, target metadata database and Asia/Shanghai timezone before extension creation. An ordinary stock Helm PostgreSQL image is insufficient. Extension creation is deployed explicitly; extension-version upgrade is separately tested, not inferred from psqldef's name-based comparison. `cron` objects remain extension-owned, outside application desired-schema/Jet generation and application naming rules.

Use existing Data Scripts for named schedule updates, explicit active-state changes and retired-job deletion. Same owner/name avoids duplicates; scheduling alone does not restore active state. psqldef check does not validate job rows. PostgreSQL must retain the extension image/configuration during restore. Dumping the same database containing cron configuration covers those rows subject to permissions; verify restoration with the selected actual role.

Before freezing an upgrade/restore backup, prevent new cron runs and verify existing job SQL has exited; scheduler pause can cancel running work and reload acknowledgement alone is insufficient. Host-side privileged maintenance controls belong to the installer/configuration phase; do not insert prohibited ALTER SYSTEM/server-global commands into Helm's database-local SQL script stream. Preserve prior scheduling state, keep it off during partial migration/restore, and re-enable only after job/database/role verification. An unresolved running writer or unknown maintenance state blocks automated progression. Validate this bounded extension of Helm's stop/backup flow in isolated development.

### Failure and recovery

Applications remain stopped after failed/uncertain migration. Earlier scripts/psqldef phases may have committed; there is no release-wide atomicity. Reconcile retained container version/exit and attempt state before retry or cancel, retaining the original baseline. Unknown outcome fails closed.

For a failed migration with a verified stopped-writer backup, retain Helm's explicit cancel/restore: restore protected environment, recreate only the application database, restore the complete dump and start original service after required checks. There is no Redis cache to flush. No valid baseline authorizes no automated database deletion/restore. Keep cron paused through restoration; restored active jobs must not race incomplete data recovery.

The proposed `recorded_at` addition requires an explicit restore prerequisite hook: a full dump's extension configuration data does not prove it recreates project-added extension-member table DDL. When the backup includes `recorded_at`, restore extension pre-data, apply the project-owned `recorded_at`/index DDL before restoring job-history data, then restore remaining data and post-data, keeping the scheduler paused throughout. Preserve whole-database restore and installer attempt-state semantics; this bounded pg_cron hook does not introduce a new restore framework. Verify the complete backup/restore round trip in isolated development before relying on this proposal.

Once migration succeeds, cancel is forbidden even if startup fails. Use a documented equal/higher repair-release path; a manual downgrade is not an ordinary rollback. Standalone start remains Helm's intentional weaker escape hatch and must be documented as such. Database Release is not application readiness: health must exercise the declared database/service availability without claiming it checks release/schema/image compatibility automatically. Operator procedure verifies selected artifact identity and applied job state separately.

### Required evolution and development evidence

Keep and adapt Helm migration/installer/publisher regression tests. Run isolated acceptance for fresh install, declared older origins, same-version retry, changed/missing processed script rejection, target-schema convergence, data validation, release-bootstrap replay, interrupted/failed script, audit failure and full-backup restore. Include assertions that unrelated configuration survives, no secret/payload is persisted, time cleanup respects per-table tiers, current R survives session cleanup, and active/sliding renewal state is not deleted.

Representative command interface:

```sh
make lint-sql
make generate
make test
make build
go test -tags=integration ./test/integration/release
go test -tags=integration ./test/integration/contracts
node test/smoke/release-install.mjs --integration-ready
node test/smoke/service-contracts.mjs --environment development
```

The final smoke command is a proposed Pin Glide adapter-level harness using explicit development endpoints and disposable test identities; it must reject production targets. Deployed-development checks cover auth/pairing/refresh/revocation, mixed 207, duplicate/timeout/interruption, single WSS admission, pg_cron setup/job mutation/cleanup and restore scheduling. Cross-client simulated-message extraction/decryption/popup/copy/shortcut acceptance remains required. Run the same tagged images/configuration sequence in isolated development before production. No performance/load/latency-sampling gate is added.

## 8. Agent change-placement guide

| Change | Destination and allowed dependencies | Required evidence; architecture update trigger |
| --- | --- | --- |
| Authentication rule | `internal/identity`; adapter maps protocol/provider/store input | Rule tests, scope/replay/old-family cases; update this document only if ownership/storage/interface changes |
| Pairing/capacity/recovery rule | `internal/pairing`; small identity/store/connection seams | Concurrent capacity/replacement/grant tests and relevant HTTP/WSS coverage; revise storage/authority rules when changed |
| Delivery rule/status | `internal/delivery`; checked admission/store/router seams | Atomic duplicate, mixed batch, late Receipt/interruption tests; update contract compatibility and table policy if affected |
| Provider interaction | `internal/adapters/{wechat,bailian}`; implement business-owned interfaces | Invalid/expired/timeout/sanitization tests; update document only for new ownership or secret/persistence scope |
| SQL field/table | Correct desired-schema object file plus immutable structural/Data Scripts as needed | SQL lint, isolated old-origin evolution, privacy and table-tier/FK checks; update inventory for any new/changed store or cleanup clock |
| Jet change | Regenerate `internal/db/jetgen` through adapted Helm tooling | Reproducible tracked/untracked output, compile and store/evolution tests; generated files are never hand-fixed |
| HTTP/WSS contract | `internal/protocol`, real HTTP registration or explicit AsyncAPI source | Both output exports/validators, route/message coverage, old v1 fixture behavior; revise protocol policy for incompatible proposals |
| Local/deployed-development test | Existing module tests or `test/integration`/`test/smoke` and `scripts/dev` | Synthetic isolated resources, explicit dev target; update commands here if public test interface changes |
| Deployment/cron change | Copied installer/release/Compose machinery; bounded target additions | Installer faults, actual backup restore, paused cron, matching artifact identities; update failure/state/retention rules in the same change |

## 9. Review checklist and maintenance

Reviewers can reject a change by citing these rules:

- Business authority/state transitions live in their modules; handlers do not contain a second rule implementation or bypass owner/scope checks.
- Domain interfaces contain no Huma/HTTP/WSS/Jet/pgx/provider adapter types; dependency directions remain acyclic and meaningful.
- No server persistence/log carries prohibited payloads or raw secrets; sanitized errors preserve the accepted HTTP/207/WSS distinction.
- Admission/dedup is atomic and prior authorization is frozen; late ACK, duplicate upload and interruption never initiate a second send.
- Connection slots are owned/cleaned by connection ID; cancellation/deadlines/resources and shutdown are tested at the real seam.
- Each PostgreSQL table has exactly one cleanup tier and one valid indexed time predicate; no status-based TTL exemption or cross-tier cascade deletes.
- Long-lived R is separated from expiring credentials; session deletion does not delete activated authorization; sliding renewal and replay guards survive their valid lifetime.
- Effective config is not cron-purged; business deletion is authorized and does not silently erase retained Delivery/log history.
- Ledger/version records are not expiring logs; processed SQL/manifest checksums, phase order and safe rerun/error handling remain intact.
- Jet and both protocol outputs regenerate cleanly; shared schemas/auth/errors/directions/Receipts and supported v1 behavior agree with runtime.
- Release source/tag/images/assets match, both YAML assets precede publication, and published content is immutable.
- Tests/generation avoid production; real migration/restore/development acceptance evidence is distinguished from static checks/default CI and mocks.
- Reuse remains recognizable: no unrelated Helm runtime is imported merely for tooling, and no new framework/linter/deployment policy is hidden under copying.

Update this document in the same change whenever module ownership, public command/config paths, tool policy, persistent tables/cleanup clocks, contract ownership/compatibility, release ordering or recovery guarantees change. Keep business behavior in the functional specification and domain terms in the glossary. This document is the placement/review contract; executable config becomes authoritative for exact installed values once implemented, and discrepancies must be corrected together.

## 10. Draft review status

Adopted: business-oriented modules, shared Go wire definitions/Huma HTTP plus explicit WSS description, released-v1 compatibility, Helm Go/SQL/deployment reuse, PostgreSQL-only three retention tiers, business physical deletion, per-table separation, direct timestamp-filter cron cleanup, manual backups and operator-managed Docker logs.

For final review: the concrete proposed table names and cleanup clocks (especially sliding login-family renewal and project-added pg_cron history `recorded_at` with its restore prerequisite hook), exporter/validator command interface and target-specific pg_cron/protocol integration details above. No schema/tool/application/deployment implementation or successful runtime checks are claimed. The architecture issue remains open until the user adopts the complete artifact and its reviewable version is linked from the resolution.
