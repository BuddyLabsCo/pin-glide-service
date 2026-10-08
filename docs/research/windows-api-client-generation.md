# Windows API client generation

Verified on 2026-10-08. Scope: the planned Windows stack and existing tools that turn service-owned YAML contracts into Rust types or callable clients. This is research, not a library selection or a Windows execution result.

## Project baseline and ownership

[Wayfinder map: Windows receiver requirements](https://github.com/BuddyLabsCo/pin-glide-windows/issues/1), updated 2026-10-08, plans Rust/Cargo business logic, Slint with Fluent styling, the Rust MSVC toolchain, `windows` crate integration, and NSIS installers. Async HTTP/WSS and cryptography libraries remain open. [Clarify Windows code architecture, quality gates, and persistence evolution](https://github.com/BuddyLabsCo/pin-glide-windows/issues/4), updated 2026-10-05, still requires functional agreement before an English `code-architecture.md` settles code placement, checks, and storage evolution. The inspected [main tree at `9a41538`](https://github.com/BuddyLabsCo/pin-glide-windows/tree/9a415380d19e51a60c5e3afd2e90e44f76b29d93) contains agent instructions, not a Cargo application; no glossary or ADR exists there.

Required boundary from the user: `pin-glide-service` publishes `openapi.yaml` and `asyncapi.yaml` as release assets. Windows owns their download, conversion/generation configuration, generated crate, and integration. These are required future release outputs; this research does not establish that releases already contain them.

## HTTP: a generated Cargo client is available

OpenAPI Generator's `rust` client supports `reqwest`, `reqwest-trait`, and Hyper templates. Its default is `reqwest` with async functions (`supportAsync=true`), and its generated crate defaults to native TLS. This is an existing HTTP client generator, not just a schema/type generator. [Rust generator documentation](https://openapi-generator.tech/docs/generators/rust/)

The official [generated API sample](https://github.com/OpenAPITools/openapi-generator/blob/ba2d6aa311a3bcfef690eb3c3db9c1493d008088/samples/client/petstore/rust/reqwest/petstore-async/src/apis/pet_api.rs) contains callable functions such as `get_pet_by_id`, typed parameters/results/errors, and awaited HTTP execution. It also emits model modules and [Cargo dependencies/features](https://github.com/OpenAPITools/openapi-generator/blob/ba2d6aa311a3bcfef690eb3c3db9c1493d008088/samples/client/petstore/rust/reqwest/petstore-async/Cargo.toml).

Illustrative caller-local generation, after installing a pinned generator version:

```bash
openapi-generator-cli generate -g rust --library reqwest \
  -i contracts/openapi.yaml -o generated/service-http \
  --additional-properties packageName=pin_glide_service_client,supportAsync=true
```

The app can consume that generated crate without publishing it:

```toml
[dependencies]
pin_glide_service_client = { path = "generated/service-http" }
```

Cargo resolves this path relative to the app's manifest. The application supplies runtime, base URL, and credentials and calls generated operations with `.await`. [Cargo path dependencies](https://doc.rust-lang.org/cargo/reference/specifying-dependencies.html#specifying-path-dependencies), [reqwest async runtime](https://docs.rs/reqwest/latest/reqwest/)

TLS must be checked against the pinned output: the inspected OpenAPI sample uses reqwest 0.13 and offers crate features `native-tls` (default) and `rustls`. Native TLS uses Windows SChannel; this is documented platform support, not proof that this project builds or runs on Windows. Feature names differ between reqwest versions. [Sample manifest](https://github.com/OpenAPITools/openapi-generator/blob/ba2d6aa311a3bcfef690eb3c3db9c1493d008088/samples/client/petstore/rust/reqwest/petstore-async/Cargo.toml), [native-tls implementation](https://docs.rs/native-tls/latest/native_tls/)

## WSS: distinguish types from callable transport

| Existing option | Generated capability | Boundary |
| --- | --- | --- |
| AsyncAPI CLI / Modelina | Rust structs/tuples/enums, configurable serde support | Generates message models; no Rust connection/send/receive SDK is supplied by the CLI's native client command. |
| Fern Rust SDK | Combined HTTP APIs and WebSocket connection, typed sends, typed receive enum, close methods | WebSocket generation is an Enterprise feature; protocol and dependency compatibility require evaluation. |
| Cortex Rust generator | HTTP clients plus WebSocket `connect`, `send_<channel>`, and typed `on_<channel>` callbacks | Open-source candidate with a specific JSON envelope and limited handshake configuration; not established as compatible with this service. |

AsyncAPI CLI currently lists Dart, Java, JavaScript, and Python for `generate client`; Rust is listed under `generate models`. For example, `asyncapi generate models rust contracts/asyncapi.yaml -o generated/ws-models` generates data types, not callable WSS operations. Modelina explicitly documents Rust model/serde generation. [CLI commands](https://github.com/asyncapi/cli/blob/master/docs/usage.md#asyncapi-generate-client-language-asyncapi), [Modelina](https://github.com/asyncapi/modelina)

Fern accepts both files in `api.specs`, using separate `openapi` and `asyncapi` entries. Rust configuration uses `enableWebsockets: true`; `generateWebSocketClients` is a source-defined alias. Its inspected seed has callable HTTP and WSS code using reqwest 0.12, Tokio, and tokio-tungstenite 0.24 with native TLS. [Combined inputs](https://buildwithfern.com/learn/sdks/reference/generators-yml), [Rust config schema](https://github.com/fern-api/fern/blob/d80641b797eaaa78c521f90c8ea691a80e791d3d/generators/rust/codegen/src/custom-config/BaseRustConfigSchema.ts), [HTTP method sample](https://github.com/fern-api/fern/blob/d80641b797eaaa78c521f90c8ea691a80e791d3d/seed/rust-sdk/websocket/src/api/resources/status/status.rs), [Seed manifest](https://github.com/fern-api/fern/blob/d80641b797eaaa78c521f90c8ea691a80e791d3d/seed/rust-sdk/websocket/Cargo.toml)

Fern's seed disables reqwest default features and supplies no HTTP TLS feature. HTTPS therefore needs explicit configuration: its documentation shows `extraDependencies.reqwest` with version `0.12` and features `["rustls-tls"]`, or `defaultFeatures: true`. This HTTP requirement is separate from the seed's native-TLS-enabled WebSocket dependency. The generated typed JSON channel exposes send/recv methods but rejects binary frames; the underlying transport's binary support does not establish a generated typed binary API. [Rust dependency configuration](https://buildwithfern.com/learn/sdks/generators/rust/configuration#extradependencies), [Typed channel source](https://github.com/fern-api/fern/blob/d80641b797eaaa78c521f90c8ea691a80e791d3d/seed/rust-sdk/websocket/src/api/websocket/realtime.rs)

Cortex's Rust template always sends `{channel,payload}`. Receive dispatch expects `channel`/`type`/`event` and extracts `payload`/`data`; these heuristics do not support an arbitrary existing wire protocol. Its connect path passes only a URL, so required handshake headers need separate verification/adaptation. The common Cargo template enables native TLS for tokio-tungstenite, while the separate WebSocket plugin manifest omits TLS features: inspect the final generated manifest for the selected pipeline. [Rust WebSocket template](https://github.com/cortex-docs/cortex/blob/db50bf4e30efe3442cf248be5f46bc496a741ced/packages/codegen/src/languages/rust/templates/websocket/client.ejs), [Common manifest](https://github.com/cortex-docs/cortex/blob/db50bf4e30efe3442cf248be5f46bc496a741ced/packages/codegen/src/languages/rust/templates/cargo-toml.ejs), [WebSocket plugin](https://github.com/cortex-docs/cortex/blob/db50bf4e30efe3442cf248be5f46bc496a741ced/packages/codegen/src/languages/ws-template-plugin.ts)

OpenAPI Generator and Modelina are Apache-2.0 open-source tools; Cortex is MIT. Fern's documented WebSocket feature requires its Enterprise plan; no free full Rust WSS entitlement was established. [OpenAPI Generator license](https://github.com/OpenAPITools/openapi-generator/blob/master/LICENSE), [Modelina license](https://github.com/asyncapi/modelina/blob/master/LICENSE), [Cortex license](https://github.com/cortex-docs/cortex/blob/db50bf4e30efe3442cf248be5f46bc496a741ced/LICENSE), [Fern WebSocket availability](https://buildwithfern.com/learn/sdks/deep-dives/websocket-clients)

## Caller workflow and recommendation

Pin one service release tag and both asset digests; pin generator version, configuration, and Cargo.lock in the Windows repo. Regenerate locally and review changes before updating that pin. Check the actual schema dialect, optional/null fields, unions, authentication, frame representation, TLS, and generated Windows MSVC build; none was executed here.

Evaluate OpenAPI Generator first for HTTP. For full callable WSS generation, evaluate Fern if Enterprise is acceptable, or Cortex only if its envelope/handshake limits fit the agreed contract. A free Modelina path still leaves transport integration to Windows. Do not choose networking, crypto, reconnection/retry, buffering, or ACK policy from a generator's defaults.

Preserve the source baseline: live delivery only, reconnect receives new messages, keys stay on clients, OTP stays in memory, and SMS bodies are released when popups close. The business milestone for a Windows receipt remains unresolved; successful frame transmission is not that decision. [Windows receiver map](https://github.com/BuddyLabsCo/pin-glide-windows/issues/1)
