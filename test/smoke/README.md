# Deployed smoke acceptance

This directory owns the unattended service-level acceptance runner, its protocol peers, guarded SQL fixtures, fault controls, and redacted evidence. The runner tests one published release against the isolated development installation; it does not replace client interoperability, real-provider login, or operator recovery checks.

## Recorded acceptance baseline

The acceptance record for issue #40 used these immutable inputs:

| Input | Value |
| --- | --- |
| SSH host and LAN address | `local-server`, `192.168.10.17` |
| HTTP and WSS origins | `http://192.168.10.17:33489`, `ws://192.168.10.17:33489` |
| Compose project and deployment directory | `pin-glide-dev`, `/srv/pin-glide-dev` |
| Database | `pin_glide` |
| Shared snapshot directory | `/mnt/share/workspace/snapshots/pin-glide` |
| GitHub Release | `v0.1.6` |
| Release source commit | `b1afea015d39e035f3f0d7fe1518d54d3da50f0f` |
| `openapi.yaml` SHA-256 | `0df5093f3a11442c09c578136a5f09cfff61ce8f0962311d12630c088f229d0b` |
| `asyncapi.yaml` SHA-256 | `aa543067cda8a172c4159d1e0415f1f25d95e989c388efe72813b93578115a15` |

The acceptance change adds no HTTP or WSS operation or message. The generated descriptions remain the two published `v0.1.6` assets identified above.

The runner refuses any other host, origin, project, deployment directory, database, snapshot directory, or undeclared release transition. Server snapshots run through passwordless `sudo`, matching the verified permission boundary for the shared directory.

The repository is private. Server-only values prepared in `deploy/compose/.env.example` are approved deployment inputs, not smoke evidence. Per-run reports omit raw credentials, provider keys, SMS text, OTP values, and ciphertext.

## Evidence classes

Every runner report assigns each scenario one of three statuses:

- `passed`: the scenario completed and its assertions held.
- `failed`: the scenario ran and an assertion or service outcome failed.
- `blocked`: an explicit prerequisite was unavailable, such as Docker, Go, a snapshot, or a provider input.

Every scenario also has an `evidenceType`: `smoke` for deployed-service behavior or `deterministic` for a referenced Go test. The status is that evidence's execution outcome, not a claim that deterministic evidence crossed the deployed boundary. The report may contain `not_run` or `not_applicable` facts inside a passed scenario when a live boundary cannot observe the behavior. Such facts do not count as passes. A smoke exit code of `0` means the requested journey and its referenced deterministic checks passed, `1` means one failed, and `2` means a prerequisite was blocked or a read-only check completed without restore.

The runner executes exact Go test names and records deterministic evidence only when the test reports `--- PASS`. Missing Go or Docker blocks that evidence; a failed test produces a failed report. Tests using provider fakes are deterministic evidence, never real-provider smoke.

## Coverage matrix

The recorded #40 integration run `smoke-1792640175846-fcdbff38b601` passed all 66 scenario results: 64 deployed smoke scenarios and two deterministic checks. No included smoke scenario failed or was blocked. The full `make test` and tagged integration runs also passed. The excluded provider-fake references passed separately as seven deterministic tests.

| Scope | Deployment evidence | Deterministic evidence | Recorded #40 outcome | Boundary |
| --- | --- | --- | --- | --- |
| Account and Sender credentials (#34) | Scope, expiry, refresh, lost response, logout, Account switch, registration failure, and seeded ownership facts | Full repository unit and integration suites | Smoke and deterministic passed | Accounts and initial credentials come from guarded SQL and are not provider-verified identities |
| Ordinary Pairing (#35) | New Receiver, add Sender, relay, both completion reports, first-R redemption, replacement, association, capacity, and expiry | Full repository unit and integration suites | Smoke and deterministic passed | Opaque protocol fixtures replace Android and Windows clients |
| Receiver connection (#36) | Exclusivity, presence, malformed frames, issuance supersession, restart, heartbeat loss, and reconnect without replay | Selected race, stale-close, shutdown, policy-close, recovery, and revocation tests | Smoke and deterministic passed | Live 1008 policy close needs an excluded owner-proof flow, so that close is deterministic-only |
| Bailian model profile (#37) | Deployed temporary-key issuance, requested and actual TTL, text-only strict-schema OTP and null output, expiry, and reversible faults | Adapter and model boundary tests | Smoke and deterministic passed | Provider calls are real; platform inference, key storage, and UI behavior are client scope |
| Delivery (#38) | Exact opaque bytes and correlation, indexed outcomes, duplicates, retained metadata, Receipts, timeout, disconnect, restart, PostgreSQL outage, and storage uncertainty | Delivery integration tests | Smoke and deterministic passed | Opaque envelopes prove transport behavior, not platform decryption or display |
| Retention and privacy (#39) | All policies, strict cutoffs, live families, durable authority, pg_cron execution, purge, restart, and payload scans | Selected scheduled-cleanup lifecycle, privilege, clock, and retry tests | Smoke and deterministic passed | Docker log retention and backup expiry remain operator-managed |
| Release, fixtures, and recovery (#33 and #40) | Image and contract pins, ledger convergence, fixture cleanup, stopped-writer snapshot, and disposable restore | Release, snapshot, fixture, and installer tests | Smoke and deterministic passed | Real historical upgrades only use releases declared in `release-support.json` |
| Real-provider identity paths | `excluded`; no live pass is claimed | Seven selected identity and Pairing integration tests with provider fakes | Smoke excluded; deterministic passed | Covers fresh WeChat and QQ login, re-login family replacement, provider namespace separation, owner-proof Sender revocation, and owner-proof Receiver device revocation and recovery |

The excluded deterministic references live in `lib/excluded_provider.mjs`. Receiver and retention references live with their deployed suites. The runner records the exact test names and source packages, so renaming or skipping a referenced test fails the evidence instead of producing an empty pass.

## Fixture bootstrap and cleanup

The runner creates two synthetic QQ-namespace Accounts, twenty Sender registrations, and fresh scoped credentials. Raw M/U/F and Receiver tokens remain in runner memory. PostgreSQL receives lowercase SHA-256 digests, normal lifetimes, and active registration state only.

Fixture transactions check `current_database() = 'pin_glide'` and use run-owned identifiers. Cleanup removes rows in dependency order, then re-queries Accounts, Senders, credentials, refresh operations, Receivers, Receiver credentials, Pairings, Pairing Sessions, Deliveries, diagnostics, schema audits, cron jobs, and cron runs. Cleanup passes only when every category reports zero remaining rows.

The immutable Data Scripts remain separate from per-run fixtures. They install the approved model configuration and retention inventory/job lifecycle; test identities and expiring credentials never enter the release ledger.

## Commands and evidence

Read-only preflight:

```sh
PIN_GLIDE_SMOKE_RELEASE=0.1.6 node test/smoke/runner.mjs --check
```

Guarded deployed journey:

```sh
PIN_GLIDE_SMOKE_RELEASE=0.1.6 \
PIN_GLIDE_SMOKE_WRITE_AUTHORIZATION=pin-glide-dev \
  node test/smoke/runner.mjs --integration-ready
```

Evidence defaults to the ignored `test/smoke/evidence/` directory with mode `0600`. Set `PIN_GLIDE_SMOKE_EVIDENCE` to choose another path. The run holds `/tmp/pin-glide-dev-smoke.lock` on `local-server`, restores every reversible fault in `finally` paths, and releases the lock before exiting.

Disposable installation checks remain explicit:

```sh
node test/smoke/release-install.mjs --integration-ready
node test/smoke/published-install.mjs --integration-ready --from 0.1.5 --to 0.1.6
```

## Explicit exclusions

Unattended smoke does not obtain real QQ or WeChat authorization codes. It does not claim successful fresh-provider login, login-family replacement, owner-proof Sender or Receiver device-security revocation, or owner-proof `recover_receiver` as deployed passes. The selected deterministic tests cover those business paths with provider fakes.

Platform cryptography, key storage, QR scanning, Android and Windows UI behavior, popup, clipboard, and shortcut verification remain client work. Performance and load gates, latency sampling, service-side cryptography or inference, offline ciphertext backlog, additional replay or AAD policy, and future homepage work remain outside this acceptance suite.
