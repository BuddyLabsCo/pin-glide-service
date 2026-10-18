# Helm Gateway Go, SQL, and Deployment Reuse Report

Research date: 2026-10-10. Status: the user approved reusing the inspected Helm Go/SQL/deployment baseline. This inventory supports [the architecture review draft](../../code-architecture.md); concrete target-specific implementation details await final human review.

Reference: `BuddyLabsCo/helm-gateway`, commit `ba375819b98bcd76fc27d7a15b2523aa62837c6f`. Its current `main` matched this commit when checked. All source links below are fixed to that commit. Inspection used a read-only checkout and authenticated GitHub APIs. No tools were installed, reference application checks executed locally, containers started, or deployments performed.

## 1. Conclusion and approval boundary

Reuse the existing Go/SQL verification and release/deployment machinery as the baseline, including its pinned tools, SQL conventions, script ledger, installer state machine, backup/recovery behavior, and associated tests. Do not replace it with a newly designed set of linters or a second migration/deployment framework.

Literal copying without adaptation will not work. The reference includes a React Console, separate API/worker images, Redis, River, Helm business SQL, hardcoded product/module/schema names, and legacy migration compatibility. Pin Glide currently requires one active Go process plus PostgreSQL, a one-off migration tool, pg_cron, and two public contract descriptions. Those differences require bounded substitutions and removals; they do not justify reinventing the shared machinery.

The user approved architecture discussion Q1-Q3, then approved this reuse baseline and pg_cron job deployment through the existing Data Scripts mechanism. Earlier Q4-Q7 recommendations were not adopted. In particular, the reference does not select extra depguard/context linters, race checks, a 30-day backup retention policy, or a new cumulative application-version upgrade policy.

## 2. Tools and versions actually used

| Area | Tool/configuration | Reference version or setting |
| --- | --- | --- |
| Go toolchain | `go.mod`; CI `go-version-file` | Go `1.27.1` |
| Go HTTP | Huma | `v2.39.1` |
| Typed SQL generation | Jet, invoked as `go tool jet` | `v2.16.0`; Go `tool` entry |
| PostgreSQL access | pgx stdlib with `database/sql` | pgx `v5.11.0` |
| Declarative schema | `go tool psqldef` | sqldef `v3.11.24`; Go `tool` entry |
| Go lint | `.golangci.yml`, wrapper, CI action | golangci-lint `2.14.0`, config version `2` |
| Go format | `gofmt`; golangci formatter configuration | Go toolchain version |
| Source size | `scripts/quality/check-file-size.sh` | Maximum 500 physical lines for selected source extensions |
| SQL catalog lint | Repository-owned `sql-lint.sql` | PostgreSQL catalog queries, no separate third-party binary |
| SQL object rules | Schemalint plus repository plugins | `2.3.2`, exact root npm dependency and lockfile |
| SQL text/style | SQLFluff through `uvx` | `4.3.0` |
| Release SQL AST guard | `wasilibs/go-pgquery` | `v0.0.0-20250409022910-10ac41983c07` |
| Disposable and production database | Docker PostgreSQL | `postgres:18-alpine` |
| Script execution/tests | Bash, Node, Git; Node test runner | CI Node `24` |
| Deployment | Docker Engine, Compose v2, jq, flock | Compose must support `up --wait` and `config --format json` |
| Existing optional Console | React tooling, Prettier, ESLint, Orval | Orval `8.33.0`; unrelated to client generation in Pin Glide |

Sources: [go.mod][go-mod], [lint configuration][golangci], [Makefile][makefile], [SQL entrypoint][sql-lint-entry], [root npm manifest][npm], [CI][ci]. PostgreSQL/Node/Docker builder tags are not full patch/digest pins; `ubuntu-latest`, action version tags, and uv setup are also not immutable environment pins. Matching this baseline does not imply a fully hermetic build.

## 3. Go verification: commands and enforcement

These are existing Helm commands, not commands runnable in the current documentation-only Pin Glide repository.

| Command | Actual execution | What it checks |
| --- | --- | --- |
| `make format-check` | Web Prettier check, then Go formatting script using `gofmt -l` | Formatting; any unformatted handwritten Go file fails |
| `make format` | Web Prettier write, then `gofmt -w` | Formatting changes |
| `make lint` | Web ESLint; pinned golangci wrapper; file-size script | Go static analysis and selected source-file size |
| `make test` | `node --test scripts/*/*.test.mjs`; `go test ./...`; web tests | Behavioral checks, separate from static analysis |
| `make build` | `go build ./...`; web TypeScript/Vite build | Compilation/bundling |
| `go test -tags=integration ./test/integration/release` | Explicit integration-tag suite using disposable PostgreSQL and migration tools | Real migration behavior; not included by the default untagged test command |

The pinned golangci standard set is **errcheck, govet, ineffassign, staticcheck, unused**. The repository enables no additional linters or custom settings. `govet` runs within golangci; there is no separate `go vet` command in the Makefile pipeline. The wrapper rejects absent or wrong golangci versions. The CI golangci action also verifies configuration and runs lint before `make lint` runs the wrapper again. [Sources: configuration and wrapper][golangci], [standard set at the pinned upstream version][standard-linters], [CI][ci].

Generated Jet and other generated output directories are excluded from handwritten formatting/lint diagnostics. The formatting script enumerates tracked and nonignored untracked Go files. The 500-line check covers Go, TypeScript/TSX, CSS and shell files; it does not cover SQL, Markdown, YAML, JSON or MJS. Generated files are exempt. These executable scopes are narrower than a blanket rule for every handwritten file. [Sources: formatting and size scripts][quality-dir].

There is no custom Go AST/import-boundary checker, depguard policy, configured gosec/errorlint/noctx/contextcheck, `-race` command, coverage threshold, or proof of goroutine lifetime in this pipeline. Architecture and ownership rules in `code-architecture.md` are enforced through code review unless another explicit executable check covers them. Standard errcheck settings are not equivalent to rejecting every deliberately discarded error or unchecked type assertion.

**Reuse:** retain the same Go check configuration, wrapper, size policy and script tests. Remove Console commands when there is no Console. Do not add a frontend to make copied quality commands succeed. Keep generated-path exclusions consistent with the selected paths. [Source: Makefile][makefile].

## 4. SQL verification: four complementary checks

`make lint-sql` invokes `scripts/quality/lint-sql.sh`. It is partly static checking and partly execution against a real, isolated database:

1. Start disposable `postgres:18-alpine` with a random loopback port and known test-only credentials. A trap removes the container and temporary directory on exit.
2. Concatenate desired SQL in sorted filepath order; apply `release` then `helm` with pinned `go tool psqldef`, scoped to each target schema. This loader runs **apply only**, not apply/check.
3. Run native PostgreSQL catalog checks and Schemalint against the resulting database.
4. Run SQLFluff over desired schema, structural migrations and data scripts.

Sources: [database loader][db-loader], [SQL entrypoint][sql-lint-entry]. The loader exports disposable connection settings; neither generation nor lint is intended to connect to development or production databases.

| Check | Actual rules | Limit |
| --- | --- | --- |
| PostgreSQL/psqldef load | Valid target DDL and references for a fresh schema | Does not execute historical upgrade/data scripts or prove old-data preservation |
| Native catalog SQL | PK `pk_<table>`; FK `fk_<target_table>_<target_columns>`; standalone index `idx_`/`uidx_<table>_<key_columns>`; unique constraints forbidden in favor of unique indexes | Expression indexes skip name checking; INCLUDE columns are excluded; exact exceptions can be listed in `sql-lint-ignore.sql` |
| Schemalint naming plugin | Object-name shapes, including view/function/procedure/check/trigger conventions; native SQL handles composite FKs | Regex checks do not prove semantic pluralization, domain ownership, every column name, or one object per source file |
| Schemalint comment plugin | Nonblank table/column/view/enum/routine comments; parameter names must be documented for routines | Presence does not prove useful/correct English documentation |
| SQLFluff | PostgreSQL dialect, raw templating, layout/capitalisation groups; 80 columns, four spaces, uppercase keywords, lowercase types, trailing commas; identifier-capitalisation rule CP02 excluded | Parses/styles SQL; does not prove script semantics or safe reruns |

Sources: [native catalog SQL][sql-catalog], [Schemalint configuration][schemalint], [plugins][schemalint-plugins], [SQLFluff configuration][sqlfluff].

`make format-sql` runs SQLFluff fix on desired schemas only. Historical structural/data scripts are linted but not automatically reformatted: changing processed file bytes invalidates their checksums. Native SQL exceptions and Schemalint exceptions are separate mechanisms; one does not exempt the other.

**Reuse:** carry the complete set of `.sqlfluff`, `.schemalintrc.js`, root npm manifest/lock, plugins, catalog checks, ignore file, disposable PostgreSQL/psqldef wrappers, targets and tests. Replace application schema/database identity consistently. Root Node/npm remains necessary for Schemalint even without any frontend. Do not replace this set with a new SQL checker.

## 5. Generation and CI

`make generate` uses the same disposable target-schema database:

- Jet generates **both release and application schemas** into `internal/db/jetgen/<schema>`. The script also hardcodes the database name in Jet's temporary output path; changing only the schema loop is insufficient.
- `go run ./cmd/helm-api -openapi` registers actual Huma routes and writes `generated/openapi/native.json`, without starting the HTTP listener or database connection.
- Orval generates the Console TypeScript client from that JSON. This is Helm's local Console integration, not a choice for Pin Glide's Android/Windows clients.
- CI runs `make generate` then `git diff --exit-code`. This catches tracked-output drift; it does not by itself detect entirely new untracked generated files.

Sources: [generator][generation], [Huma router/export][router], [CI][ci]. The architecture document's abbreviated generation section omits the release schema; executable scripts generate both. There is no AsyncAPI pipeline or independent OpenAPI compatibility validator in the inspected chain.

CI has separate **quality**, **generated**, **test**, and **build** jobs. The fixed commit's [CI run][ci-run] completed successfully, including format, static checks, SQL checks, generation drift, tests and build. This is remote historical evidence for Helm, not a local rerun or proof of Pin Glide integration. The default tests exclude `//go:build integration` release tests and do not run the real Compose release smoke. SQL lint and generation still use real disposable PostgreSQL.

## 6. Deployment contents and exact workflow

The reference deploys private GHCR images for **web, api, worker, migrate**, all with one application release tag. PostgreSQL and Redis have independent infrastructure tags and remain running during ordinary application upgrades. The migrate image is a one-off tool, not a long-running application.

The server needs Docker/Compose, Bash, jq, flock and Linux utilities; it does not need Go, Node or PostgreSQL client installations. Database clients execute inside PostgreSQL; application/SQL tools are already packaged in images. The operator copies Compose YAML, `.env.example` as `.env`, and root `install.sh` into one installation directory, fills secrets, and authenticates private GHCR pulls with `docker login`. The installer does not install host dependencies or automatically update those support files. [Sources: deployment guide][deploy-guide], [installer][installer].

Important configuration behavior:

- Stable, unique `COMPOSE_PROJECT_NAME` selects networks and persistent volumes; changing it is not an upgrade.
- `.env` contains one literal `RELEASE=X.Y.Z`, without `v` or interpolation. Docker invocations unset inherited `RELEASE`; the file is not shell-evaluated.
- Secrets are environment variables and protected files, not Docker secrets. Database credentials interpolated into a URL must be URL-safe. Editing environment credentials does not rotate an existing database volume.
- Production PostgreSQL/Redis have no host ports. API/web host ports require the operator's HTTPS/WSS reverse proxy and network configuration.
- Ordinary application upgrades do not delete volumes or upgrade PostgreSQL major versions. There is no automatic backup-retention/log-age policy in this installer.

Sources: [Compose][compose], [environment example][env-example], [installer][installer].

Existing first-install commands:

```sh
./install.sh --version 0.1.0 --prep --initial-install
./install.sh --version 0.1.0 --initial-deploy
```

Existing update commands:

```sh
./install.sh --version 1.0.1 --prep
./install.sh --version 1.0.1 --full-release
```

`prep` creates an attempt JSON, backs up `.env`, and writes the target release. Normal prep reads Database Release from the database, rejects missing/unreadable version history and downgrades, and permits equal-version redeployment. Initial mode is explicit and follows its separate checks; it never means deleting an existing volume.

Full release executes **pull matching images -> stop application containers -> verify they are stopped -> freeze a complete database backup -> migrate -> start with Compose health waiting**. Initial deployment starts PostgreSQL/Redis first and applies fresh schema/data before applications. The installer also exposes individual operations; keep their existing state semantics when reusing them. [Source: installer][installer].

## 7. SQL release engine: reuse the entire mechanism

The migration tool consists of `cmd/helm-release`, `internal/release`, `db/schema/release`, application desired schemas, structural/data script streams, pinned psqldef, a Git-history-derived manifest, and matching Docker packaging/tests. Copying just psqldef or the host installer loses essential behavior.

Four release tables hold deployment milestones, structural-script ledger, data-script ledger and schema-application audits. Script records store names, SHA-256 checksums, SQL snapshots, timing and executor. Processed scripts changed or deleted from the release are rejected. A complete first-parent Git history determines script introduction order; packaging from the exact release commit writes `db/migrations/order.txt`. Runtime validates that the manifest matches bundled scripts. Local generation and published packaging are different: a plain checkout lacking that manifest is not an equivalent migration artifact. [Sources: packaging][packaging], [ordering][ordering], [release implementation][release-dir].

Before execution, the engine parses desired SQL and all scripts using a PostgreSQL AST parser. It rejects explicit transaction control and selected server-global/database/role/tablespace changes and external COPY programs. SQL remains trusted code: functions/procedural bodies are not sandboxed, and arbitrary script idempotence is not proved. This guard runs in the release executable, **not** `make lint-sql`.

The actual reference phase order is:

1. Validate sources, manifest, target version and execution options; acquire a database advisory lock.
2. Validate installation state and processed-script ledger.
3. Run release-schema structural scripts.
4. Apply/check release desired schema, record deferred release-script ledger entries and import legacy metadata.
5. Run River's own migration machinery.
6. Run Helm application structural scripts, then apply/check its desired schema.
7. Append Database Release.
8. Run data scripts.

Sources: [migration orchestration][migrations], [ledger][ledger], [psqldef process wrapper][psqldef-runtime]. Database Release is recorded before data scripts: it is not proof that the entire migration or application startup succeeded. The host attempt JSON and retained migration-container outcome carry additional execution state.

Each application/data script and its ledger entry share a transaction. Release-schema scripts commit before their later ledger insertion, deliberately leaving a replay window during bootstrap/schema sync failures. Fresh installation records structural scripts as skipped and executes data scripts. Scripts must be safe to rerun. Separate psqldef processes, schema phases and scripts do not form one release-wide transaction.

**Reuse:** preserve ordering, ledger/audit semantics, SQL guard, locks, retry rules and tests. Remove River calls and Helm-only legacy-history import when they have no target counterpart; substitute the application schema/module/environment names. Keep release metadata in the release schema. Do not silently change the reference milestone or recovery semantics while claiming parity.

## 8. Backup, interruption and recovery

The installer writes restricted, atomically replaced `attempt-X.Y.Z.json`, holds a nonblocking installation-directory `flock`, and retains the named migration container for exit/log reconciliation. The Go tool also holds a database advisory lock. These locks coordinate cooperating invocations; they do not stop arbitrary external writers.

The backup is a **complete application-database** custom-format `pg_dump -Fc`, verified as nonempty and readable by `pg_restore --list`. It covers SQL/release metadata and other objects in that database, but not cluster roles/server configuration or other databases. It is neither PITR nor a restore-tested backup guarantee. Migration requires a stopped-writer baseline; retries preserve the original backup after migration has started. [Source: installer backup/migrate functions][installer].

Failure handling is intentionally conservative:

- Migration failure leaves applications stopped; earlier commits may remain.
- Retry reconciles the retained container's exact version and exit status before deciding whether to rerun. An unknown/missing/inconsistent migration outcome blocks automatic recovery.
- `--cancel` refuses once that attempt's migration succeeded, even if application startup subsequently failed. Later repair uses the documented equal/higher release path; no automatic old-image rollback is promised.
- For a failed migration with a valid stopped baseline, cancel restores environment, recreates only the target application database, restores the complete dump, flushes Helm's dedicated Redis cache, then starts old applications. It is a privileged operation, not normal upgrade behavior.
- No valid baseline means no automatic database deletion/restore or volume deletion. Failed restoration remains stopped and retains recovery state.
- Standalone `--start-helm` is an intentional escape hatch: it does not require successful migration/backup and archives the attempt after successful startup. Its weaker guarantee must not be mistaken for the guarded full-release path.

Sources: [installer][installer], [deployment recovery guide][deploy-guide]. Do not promise whole-release atomic rollback or automatic failure classification beyond these actual rules.

Existing installer tests simulate Docker and cover state/order/fault paths. Real PostgreSQL migration tests require the explicit integration tag; real installer/Compose smoke requires its separate integration-ready invocation. Default CI does not establish those deployment/restore scenarios passed. [Sources: integration tests][release-integration], [release smoke][release-smoke].

## 9. Image and GitHub Release publication

The existing release automation is reusable as a complete chain:

- Main pushes open/update an `auto-release` version PR using GitHub App credentials. This does not itself publish.
- A qualifying merged release PR reruns CI at the merged SHA.
- The publisher checks version progression, merge/tag/source identity and private GHCR package policy.
- It creates/reuses the source tag, builds/publishes or verifies matching images, then creates a GitHub Release with notes.
- Correct same-source images can be reused after partial failure; conflicting tag/image identity fails instead of overwriting.

Sources: [publisher][publisher], [publish workflow][publish-workflow], [auto-release workflow][auto-release-workflow]. Hardcoded organization, source URL, image prefix and four-service list need target substitutions. Image builds use an exact-commit `git archive` and generated migration manifest. The existing GitHub Release has notes/tag, **no OpenAPI/AsyncAPI attachments**, and no deployment job. The current repository Release list was empty when queried; the successful [auto-release run][auto-release-run] only proves the release-PR step ran, not publication or production deployment.

Delivered in this repository: `scripts/release/auto-release-pr.mjs` and `scripts/release/publish-release.mjs` carry the same chain with the pin-glide-service identity, the `api`/`db-tools` release images and the derived `pin-glide-postgres` distribution the Compose file names. The publisher keeps its Helm behavior for version progression, merge/tag/source identity, private package policy, same-source reuse and no-overwrite refusal, and adds what Helm had no counterpart for: both `openapi.yaml` and `asyncapi.yaml` are exported and validated at the release commit and attached to the Release before it is published, with a conflicting published asset failing instead of being replaced.

## 10. Direct-reuse matrix for Pin Glide

| Area | Preserve from Helm | Necessary target adaptation or gap |
| --- | --- | --- |
| Go checks | Same golangci standard configuration/version, gofmt, size check, tests | Product paths and generated exclusions; remove web lint/format calls; no new linter suite |
| SQL checks | Same SQLFluff/Schemalint/catalog conventions and disposable-schema loader | Application schema/database identity, plugin scopes/fixtures; keep root npm |
| Jet | Target-schema-driven generation with pinned tools | Both release/application schemas; database-dependent output path and generated imports |
| API export | Huma registration/export machinery | Actual Pin Glide routes/types; required YAML output under generated/protocol/ rather than Helm Console JSON; no Orval ownership transfer |
| Release SQL | Manifest, checksums, immutable scripts, AST validation, audits, ledger and transaction/replay behavior | Remove River and nonexistent Helm legacy migration streams; update names consistently |
| Installer | Same attempt state, version checks, locks, frozen backup, reconciliation and recovery | One service instead of web/api/worker; remove Redis startup/cache flush; update matching CLI messages/fixtures |
| Runtime artifacts | Separate service and one-off migrate images, persistent PostgreSQL volume, Compose workflow | Module/image/env names; no Redis/River/Console/worker solely for script compatibility |
| Publishing | Existing release PR, same-SHA image/tag checks and retry/no-overwrite chain | Repository/image identities and service list; the YAML attachment chain is delivered in this repository rather than inherited |
| Tests | Existing script and migration regression coverage | Update fixtures together with all renamed/removed dependencies; migrated tests must remain runnable |

The following **existing Pin Glide requirements cannot be met by an unchanged copy**:

1. **pg_cron:** the reference PostgreSQL image/configuration does not set up the extension. Stopping application containers leaves PostgreSQL running and does not pause/drain scheduled database writers. Restore/bootstrap/cleanup scheduling needs a target-specific addition.
2. **Shared release contracts:** Huma JSON export does not supply complete `generated/protocol/openapi.yaml` and `generated/protocol/asyncapi.yaml`, WSS descriptions, their validation/compatibility coverage, or attaching both before publication. Those service-owned additions are already required, not a reason to replace Helm's Go/SQL/deployment tools.
3. **PostgreSQL retention:** the reference does not implement Pin Glide's seven-day transaction/temporary-control and thirty-day database-log cleanup with one policy per table. The adopted target addition is a configuration table supplying target tables, retention days and date columns, read by a cron function executing dynamic DELETEs. No per-table date column is fixed during architecture planning. Configuration/authoritative release ledgers have no scheduled deletion; business deletion remains allowed where behavior permits it. Docker logs remain operator-managed and backups follow the inherited manual retention/recovery workflow. SQL naming/lint and the installer do not implement this target cleanup mechanism.
4. **Live WSS shutdown:** stopping containers and checking that none run is not proof of graceful completion of admitted deliveries. API shutdown is bounded at 5 seconds; Compose has no explicit `stop_grace_period`. Pin Glide's already-approved delivery interruption semantics must be honored when adapting the service.
5. **Health scope:** actual Helm `/healthz` checks PostgreSQL connectivity and tolerates Redis degradation; worker health checks process existence. Health does not verify image/database-release/schema compatibility. Pin Glide's route name and required health behavior must be implemented without claiming stronger checks already exist in Helm.

Sources: [Pin Glide approved specification](../specs/service-requirements.md), [Compose][compose], [actual system health][system-health], [API shutdown][api-server], [publisher][publisher].

### Verified pg_cron deployment boundary

psqldef `v3.11.24` supports `CREATE EXTENSION`, but its existing-extension comparison is name-based and does not supply automatic extension-version upgrades. The database image must already contain pg_cron, preload it and restart PostgreSQL as required. Job definitions are rows in `cron.job`, not objects reconciled by psqldef schema diff/check.

Use the adopted Data Scripts path for explicit job changes. In inspected pg_cron `v1.6.7`, named `cron.schedule` upserts by `(jobname, username)` and changes schedule/command/database, preserving job identity; it does not converge `active`. Keep a stable owner/name, set active state explicitly with `cron.alter_job`, and explicitly remove retired jobs with `cron.unschedule`. A later change needs a new immutable script; deleting an old declaration does not remove the deployed job. Query job metadata separately to validate the result.

Pausing a job or global scheduling is not a promise to drain existing SQL naturally: the inspected implementation cancels running jobs. Reload acknowledgement alone does not prove all executions stopped. Full dumps can include extension configuration rows such as jobs, so restore must keep scheduling controlled until job owner/database/active state is verified.

Sources: [psqldef extension support](https://github.com/sqldef/sqldef/blob/v3.11.24/cmd-psqldef.md), [name-based extension generation](https://github.com/sqldef/sqldef/blob/v3.11.24/schema/generator.go#L3285), [pg_cron setup](https://github.com/citusdata/pg_cron/blob/v1.6.7/README.md#setting-up-pg_cron), [job upsert](https://github.com/citusdata/pg_cron/blob/v1.6.7/src/job_metadata.c#L243), [running-job cancellation](https://github.com/citusdata/pg_cron/blob/v1.6.7/src/pg_cron.c#L1676).

## 11. Adopted reuse scope; no implementation performed

Use the inspected fixed Helm baseline for Go/SQL verification and deployment mechanisms, with coherent product-identity substitutions and removal of unrelated Helm runtime/frontend dependencies. Carry tests and packaging dependencies together. Treat the five target gaps above explicitly; do not silently present them as existing Helm features. pg_cron jobs use the existing Data Scripts mechanism with stable owner/name and explicit state verification; psqldef does not reconcile job rows.

Review remaining target-specific decisions in [code-architecture.md](../../code-architecture.md). The complete artifact awaits final human adoption. This report does not resolve the architecture ticket, install the tooling, or authorize production changes.

Unverified: local execution of reference application checks, destination compilation, migrated CI, pg_cron behavior, real backup restore, image publication and client interoperability. Historical Helm CI success is the only application-check execution result inspected here.

[go-mod]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/go.mod
[golangci]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/.golangci.yml
[standard-linters]: https://github.com/golangci/golangci-lint/blob/v2.14.0/pkg/lint/lintersdb/builder_linter.go
[makefile]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/Makefile
[npm]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/package.json
[ci]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/.github/workflows/ci.yml
[quality-dir]: https://github.com/BuddyLabsCo/helm-gateway/tree/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/quality
[sql-lint-entry]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/quality/lint-sql.sh
[db-loader]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/db/psqldef.sh
[sql-catalog]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/quality/sql-lint.sql
[schemalint]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/.schemalintrc.js
[schemalint-plugins]: https://github.com/BuddyLabsCo/helm-gateway/tree/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/quality/schemalint/rules
[sqlfluff]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/.sqlfluff
[generation]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/codegen/generate.sh
[router]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/internal/api/native/router.go
[ci-run]: https://github.com/BuddyLabsCo/helm-gateway/actions/runs/36443661186
[installer]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/install.sh
[deploy-guide]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/deploy/compose/README.md
[compose]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/deploy/compose/docker-compose.yml
[env-example]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/deploy/compose/.env.example
[packaging]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/release/package-release.sh
[ordering]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/release/migration-order.sh
[release-dir]: https://github.com/BuddyLabsCo/helm-gateway/tree/ba375819b98bcd76fc27d7a15b2523aa62837c6f/internal/release
[migrations]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/internal/release/migrations.go
[ledger]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/internal/release/ledger.go
[psqldef-runtime]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/internal/release/psqldef.go
[release-integration]: https://github.com/BuddyLabsCo/helm-gateway/tree/ba375819b98bcd76fc27d7a15b2523aa62837c6f/test/integration/release
[release-smoke]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/test/smoke/release-install.mjs
[publisher]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/scripts/release/publish-release.mjs
[publish-workflow]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/.github/workflows/publish-release.yml
[auto-release-workflow]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/.github/workflows/auto-release-pr.yml
[auto-release-run]: https://github.com/BuddyLabsCo/helm-gateway/actions/runs/36443661161
[system-health]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/internal/system/system.go
[api-server]: https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/internal/api/server.go
