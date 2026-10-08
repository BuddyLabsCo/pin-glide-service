# Android clients from versioned service contracts

Researched on 2026-10-08. This report evaluates available methods; examples are proposed consumer configurations, not an approved Android architecture or an executed build.

## Verified repository baseline

[Wayfinder map: Android sender requirements](https://github.com/BuddyLabsCo/pin-glide-android/issues/1), updated 2026-10-08, specifies Kotlin application logic, Jetpack Compose + Compose Material 3, Gradle Wrapper/AGP, HTTPS service access, and direct Android-to-Bailian extraction. [Clarify Android code architecture, quality gates, and persistence evolution](https://github.com/BuddyLabsCo/pin-glide-android/issues/4), also updated 2026-10-08, remains open with no resolution and requires agreement on generated-code placement and protocol consumption. HTTP/WSS libraries, generator configuration, minSdk, and exact toolchain versions are therefore **undecided**, not existing implementations.

The [repository snapshot `7a044348`](https://github.com/BuddyLabsCo/pin-glide-android/tree/7a044348a3f86ad8fb9c3a2d9ea5c785d5b3710c) contains only `AGENTS.md` and `docs/agents/`; there is no application, Gradle setup, glossary, ADR, or `code-architecture.md`. [GitHub tree API](https://api.github.com/repos/BuddyLabsCo/pin-glide-android/git/trees/7a044348a3f86ad8fb9c3a2d9ea5c785d5b3710c?recursive=1).

## HTTP: native Kotlin generation is available

OpenAPI Generator's stable `kotlin` client generator offers `jvm-okhttp4`, `jvm-retrofit2` (including `useCoroutines`), `jvm-ktor`, and `multiplatform`. Its date options include Java time, kotlinx-datetime, and strings. These are alternatives for the architecture discussion. [Kotlin generator options](https://openapi-generator.tech/docs/generators/kotlin/).

An illustrative OkHttp route, using a separately pinned CLI version. The tag `v1.2.3` below is an example of a future service release, not an existing release:

```sh
openapi-generator-cli validate -i contracts/v1.2.3/openapi.yaml
openapi-generator-cli generate -g kotlin --library jvm-okhttp4 \
  -i contracts/v1.2.3/openapi.yaml -o generated/service-client \
  --additional-properties=packageName=com.buddylabs.pinglide.serviceapi,omitGradleWrapper=true
```

The Gradle plugin provides `openApiValidate` and `openApiGenerate`, with `generatorName`, `inputSpec`, `outputDir`, and `configOptions`; generation tasks can be build dependencies. Integrate generated sources into the chosen source set or consume the generated JVM library, using the application's agreed Gradle/Kotlin versions. Declare the generated runtime dependencies and run Android compilation/lint, rather than treating generation success as platform validation. [Gradle plugin](https://openapi-generator.tech/docs/plugins/).

Dependency versions printed in the generator option documentation lag the actual templates. For example, the [v7.26.0 build template](https://github.com/OpenAPITools/openapi-generator/blob/v7.26.0/modules/openapi-generator/src/main/resources/kotlin-client/build.gradle.mustache) uses Kotlin 2.4.0, OkHttp 5.4.0, Retrofit 3.0.0, and Ktor 3.5.1. Pin the generator and inspect its generated build; these versions are not approved for this project. The tool is Apache-2.0 licensed. [License](https://github.com/OpenAPITools/openapi-generator/blob/master/LICENSE).

Android compatibility depends on the generated code, not just its transport: OkHttp supports API 21+, but Java time/file APIs require a minSdk/desugaring assessment. The generator's `supportAndroidApiLevel25AndBelow` option explicitly documents a temporary-file vulnerability, so it is not a blanket compatibility switch. [OkHttp requirements](https://github.com/square/okhttp#requirements), [generator options](https://openapi-generator.tech/docs/generators/kotlin/), [Android API desugaring](https://developer.android.com/studio/write/java8-support). For Ktor, select an Android-capable engine; its OkHttp engine supports WebSockets, whereas its Android engine does not. [Ktor engines](https://ktor.io/docs/client-engines.html).

### Huma's OpenAPI version

Huma generates OpenAPI 3.1 YAML by default and exposes a 3.0.3 representation for older tools. OpenAPI Generator still labels 3.1 support **beta**. Validate actual nullability, unions, formats, and generated serialization before accepting the client. If conversion is needed, the consuming repository must pin that conversion and verify schema equivalence; this does not require an additional service asset beyond the requested two files. [Huma OpenAPI generation](https://huma.rocks/features/openapi-generation/), [generator compatibility](https://github.com/OpenAPITools/openapi-generator#11---compatibility).

## WSS: distinguish models from transport

| Method | Generated result | Android and cost boundary |
| --- | --- | --- |
| AsyncAPI CLI / Modelina Kotlin | Kotlin classes/enums for message payloads; **models only** | Apache-2.0 tooling. Serialization and WSS integration remain consumer work. [Modelina](https://github.com/asyncapi/modelina), [CLI model command](https://github.com/asyncapi/cli/blob/master/docs/usage.md#asyncapi-generate-models-language-file). |
| Generated models + OkHttp or Ktor | Models plus **handwritten connection glue** | Existing Android-capable transports; useful if the team wants Kotlin and open-source tooling. This does not become automatic full SDK generation. [OkHttp](https://github.com/square/okhttp), [Ktor WebSocket engine support](https://ktor.io/docs/client-engines.html). |
| Fern Java SDK | **Complete generated transport candidate** with typed HTTP/WSS methods | Fern WSS generation is an **Enterprise** feature; Java WSS generation is enabled by default. Kotlin can call Java APIs, but Android acceptance is still required. [Fern WSS](https://buildwithfern.com/learn/sdks/deep-dives/websocket-clients), [Kotlin Java interoperability](https://kotlinlang.org/docs/java-interop.html). |

The ready models command is:

```sh
asyncapi generate models kotlin contracts/v1.2.3/asyncapi.yaml \
  --packageName com.buddylabs.pinglide.serviceevents --output generated/service-events
```

The official CLI's native **client** targets are Dart, Java, JavaScript, and Python. The inspected official Java WSS template uses Quarkus and Java release 21; inference: it is not a drop-in Android SDK. This finding does not claim every third-party template is unsuitable. [CLI client command](https://github.com/asyncapi/cli/blob/master/docs/usage.md#asyncapi-generate-client-language-asyncapi), [Java template](https://github.com/asyncapi/generator/blob/ab1f79a6064b1be2678e6dbae1c412be7d54decb/packages/templates/clients/websocket/java/quarkus/template/pom.xml).

Fern accepts both contracts under one `api.specs` list, permitting a single Java SDK:

```yaml
api:
  specs:
    - openapi: ../contracts/v1.2.3/openapi.yaml
    - asyncapi: ../contracts/v1.2.3/asyncapi.yaml
```

Select and pin `fernapi/fern-java-sdk`; local filesystem output is supported. [Fern configuration](https://buildwithfern.com/learn/sdks/reference/generators-yml).

The [Fern Java fixture](https://github.com/fern-api/fern/tree/d80641b797eaaa78c521f90c8ea691a80e791d3d/seed/java-sdk/websocket) supplies `connect`, typed send, and `onReceive` methods over OkHttp. Its build uses OkHttp 5.2.1, Jackson 2.18.6, and Java 8 source/target. It also uses `CompletableFuture`, `Optional`, and Java time. These make API 26+ or a verified desugaring configuration an evaluation requirement; Java 8 bytecode alone proves neither Android runtime nor R8 compatibility. [Fixture build](https://github.com/fern-api/fern/blob/d80641b797eaaa78c521f90c8ea691a80e791d3d/seed/java-sdk/websocket/build.gradle), [Android desugaring](https://developer.android.com/studio/write/java8-support). No Android end-to-end build was run.

## Release consumption and recommendation

Requested service handoff: `openapi.yaml` and `asyncapi.yaml` accompany the matching versioned service GitHub Release. Consumers own generation/conversion and should lock the service tag, asset hashes, generator/template versions, configuration, and runtime dependencies. A tag-specific download is supported by [GitHub CLI](https://cli.github.com/manual/gh_release_download):

```sh
gh release download v1.2.3 -R BuddyLabsCo/pin-glide-service \
  --pattern openapi.yaml --pattern asyncapi.yaml --dir contracts/v1.2.3
```

Evaluate native OpenAPI Generator Kotlin for HTTP. For WSS, compare the open-source models-plus-transport route against Fern's combined Java SDK and commercial boundary. Before selection, settle minSdk/toolchain, serializer, generated-code placement, contract-version compatibility, and whether Android actually consumes WSS operations; validate generated code in a shrunk APK and on the minimum supported Android API.

These methods preserve the [planning boundaries](https://github.com/BuddyLabsCo/pin-glide-android/issues/1): Bailian extraction stays directly on Android; service contracts carry opaque ciphertext with live-only delivery. Generators do not decide product retry, encryption, acknowledgment, or offline-storage policy.
