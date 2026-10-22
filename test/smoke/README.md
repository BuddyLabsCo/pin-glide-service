# Local smoke acceptance

This directory owns the unattended service-level acceptance runner, its protocol
peers, guarded SQL fixtures, fault controls, and evidence. The runner tests one
published release against an isolated local Compose installation. It does not
replace client interoperability, real-provider login, or operator recovery
checks.

## Layout

Business domains are top-level directories. Cross-domain infrastructure lives in
`harness/`, `environment/`, and `support/`.

```text
test/smoke/
├── runner.mjs
├── release-support.json
├── credentials/
├── pairing/
├── receiver/
├── delivery/
├── model/
├── retention/
├── installation/
├── local-lifecycle/
├── harness/
├── environment/
└── support/
```

- `harness/` owns session state, fixtures, HTTP/WSS peers, evidence, contract
  checks, diagnostics, and deterministic test runners.
- `environment/` owns local provisioning, the Compose installation, release
  validation, database facts, snapshot recovery, provider faults, and writer
  guards.
- `support/` owns process execution, shell quoting, JSON, SQL literal escaping,
  path validation, errors, and cleanup composition.
- A domain may import `harness/`, `environment/`, and `support/`. `harness/` and
  `environment/` must not import a business domain.

## Local target

The runner accepts only the adopted local installation:

| Input | Value |
| --- | --- |
| HTTP and WSS origins | `http://127.0.0.1:33489`, `ws://127.0.0.1:33489` |
| Compose project and deployment directory | `pin-glide-smoke`, `/srv/pin-glide-smoke` |
| Database | `pin_glide` |
| Snapshot directory | `/tmp/snapshots/pin-glide` |
| API host bind | `127.0.0.1:33489` to container `8080` |

The smoke host port `33489` is disjoint from `make dev` (`8080`, `55432`, and
the debugger at `2345`). `make dev` keeps its checkout-derived
`pin-glide-dev-<sha256-prefix>` Compose project. The deployment template uses
`pin-glide-service`.

The runner refuses any other origin, project, deployment directory, database,
snapshot directory, or undeclared release transition. Installation commands run
locally through passwordless `sudo`; SSH is not used.

The runner creates `/srv/pin-glide-smoke` and `/tmp/snapshots/pin-glide`, copies
the deployment Compose file, `.env.example`, and `install.sh`, then patches the
smoke `.env` to `COMPOSE_PROJECT_NAME=pin-glide-smoke`,
`PIN_GLIDE_API_BIND=127.0.0.1`, and
`PIN_GLIDE_SNAPSHOT_PATH=/tmp/snapshots/pin-glide`. Other values, including
provider credentials and the PostgreSQL password, are left unchanged. An
existing smoke volume is queried before the runner chooses a same-version
redeploy or a declared upgrade. The runner clears inherited Compose and service
variables before invoking Compose and verifies the three smoke values, so a
stray shell export cannot retarget or re-expose the installation. `install.sh`
writes snapshots as the invoking `SUDO_UID` and `SUDO_GID` when called under
`sudo`, so the operator can read the archive.

No YAML asset hash is validated. The runner still rejects draft or prerelease
tags, checks the release source commit, validates the declared upgrade origin,
and verifies that every scenario names a published HTTP operation or WSS
message. Repository-wide contract checks remain part of `make check-contracts`.

## Evidence classes

Every runner report assigns each scenario one of three statuses:

- `passed`: the scenario completed and its assertions held.
- `failed`: the scenario ran and an assertion or service outcome failed.
- `blocked`: an explicit prerequisite was unavailable, such as Docker, Go, or a
  provider input.

Every scenario also has an `evidenceType`: `smoke` for local-service behavior
or `deterministic` for a referenced Go test. The status is that evidence's
execution outcome, not a claim that deterministic evidence crossed the deployed
boundary. A smoke exit code of `0` means the requested journey and its referenced
deterministic checks passed, `1` means one failed, and `2` means a prerequisite
was blocked.

The runner executes exact Go test names and records deterministic evidence only
when the test reports `--- PASS`. Missing Go or Docker blocks that evidence; a
failed test produces a failed report. Tests using provider fakes are
deterministic evidence, never real-provider smoke.

Runner output is not redacted. Raw credentials, provider keys, SMS text, OTP
values, and ciphertext can appear in the console and the evidence file. The
separate diagnostic privacy sweep still checks that the deployed service does
not retain those values.

## Coverage matrix

| Scope | Deployment evidence | Deterministic evidence | Boundary |
| --- | --- | --- | --- |
| Account and Sender credentials | Scope, expiry, refresh, lost response, logout, Account switch, registration failure, and seeded ownership facts | Full repository unit and integration suites | Accounts and initial credentials come from guarded SQL and are not provider-verified identities |
| Ordinary Pairing | New Receiver, add Sender, relay, both completion reports, first-R redemption, replacement, association, capacity, and expiry | Full repository unit and integration suites | Opaque protocol fixtures replace Android and Windows clients |
| Receiver connection | Exclusivity, presence, malformed frames, issuance supersession, restart, heartbeat loss, and reconnect without replay | Selected race, stale-close, shutdown, policy-close, recovery, and revocation tests | Live 1008 policy close needs an excluded owner-proof flow, so that close is deterministic-only |
| Bailian model profile | Deployed temporary-key issuance, requested and actual TTL, text-only strict-schema OTP and null output, expiry, and reversible faults | Adapter and model boundary tests | Provider calls are real; platform inference, key storage, and UI behavior are client scope |
| Delivery | Exact opaque bytes and correlation, indexed outcomes, duplicates, retained metadata, Receipts, timeout, disconnect, restart, PostgreSQL outage, and storage uncertainty | Delivery integration tests | Opaque envelopes prove transport behavior, not platform decryption or display |
| Retention and privacy | All policies, strict cutoffs, live families, durable authority, pg_cron execution, purge, restart, and payload scans | Selected scheduled-cleanup lifecycle, privilege, clock, and retry tests | Docker log retention and backup expiry remain operator-managed |
| Release, fixtures, and recovery | Image pins, ledger convergence, fixture cleanup, stopped-writer snapshot, and disposable restore | Release, snapshot, fixture, and installer tests | Real historical upgrades only use releases declared in `release-support.json` |
| Real-provider identity paths | `excluded`; no live pass is claimed | Seven selected identity and Pairing integration tests with provider fakes | Covers fresh WeChat and QQ login, re-login family replacement, provider namespace separation, owner-proof Sender revocation, and owner-proof Receiver device revocation and recovery |

## Fixture bootstrap and cleanup

The runner creates two synthetic QQ-namespace Accounts, twenty Sender
registrations, and fresh scoped credentials. Raw M/U/F and Receiver tokens remain
in runner memory. PostgreSQL receives lowercase SHA-256 digests, normal lifetimes,
and active registration state only.

Fixture transactions check `current_database() = 'pin_glide'` and use run-owned
identifiers. Cleanup removes rows in dependency order, then re-queries Accounts,
Senders, credentials, refresh operations, Receivers, Receiver credentials,
Pairings, Pairing Sessions, Deliveries, diagnostics, schema audits, cron jobs,
and cron runs. Cleanup passes only when every category reports zero remaining
rows.

The immutable Data Scripts remain separate from per-run fixtures. They install
the approved model configuration and retention inventory/job lifecycle; test
identities and expiring credentials never enter the release ledger.

## Commands and evidence

Guarded local journey:

```sh
PIN_GLIDE_SMOKE_RELEASE=0.1.6 \
  node test/smoke/runner.mjs --integration-ready
```

Evidence defaults to the ignored `test/smoke/evidence/` directory with mode
`0600`. Set `PIN_GLIDE_SMOKE_EVIDENCE` to choose another path. The run holds
`/tmp/pin-glide-smoke.lock`, restores every reversible fault in `finally` paths,
and releases the lock before exiting. The first run installs the selected
release into the freshly created smoke environment; later runs redeploy the
same release or upgrade from its current recorded release.

Disposable installation checks remain explicit:

```sh
node test/smoke/installation/release-install.mjs --integration-ready
node test/smoke/installation/published-install.mjs --integration-ready --from 0.1.5 --to 0.1.6
```

## Explicit exclusions

Unattended smoke does not obtain real QQ or WeChat authorization codes. It does
not claim successful fresh-provider login, login-family replacement, owner-proof
Sender or Receiver device-security revocation, or owner-proof `recover_receiver`
as deployed passes. The selected deterministic tests cover those business paths
with provider fakes.

Platform cryptography, key storage, QR scanning, Android and Windows UI behavior,
popup, clipboard, and shortcut verification remain client work. Performance and
load gates, latency sampling, service-side cryptography or inference, offline
ciphertext backlog, additional replay or AAD policy, and future homepage work
remain outside this acceptance suite.
