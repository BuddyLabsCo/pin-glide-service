# PinGlide Technology Stack Overview

See the [technical overview](pin-glide-tech-overview.md) for architecture and business flows. Both clients support light and dark themes.

## Android

Repository: `pin-glide-android`.

### Development Environment Setup

- IDE: Android Studio with built-in Kotlin and Compose support.
- Tools: Git, JDK, Android SDK (including Build Tools, Platform Tools/ADB, and Emulator), and Android CLI. Enable KVM on Linux or Windows Hypervisor Platform on Windows.
- Skills: Install Google's official [Android Skills](https://github.com/android/skills). Enable `android-cli` through Android CLI and add Compose and testing skills as needed.
- UI design: Install [Impeccable](https://impeccable.style/) to communicate layout, interaction, and visual requirements while following Material 3.

### Build

- Kotlin: Application and business logic.
- Jetpack Compose + Compose Material 3 component library: UI and theming.
- WeChat and QQ SDKs: provider authorization and login. Android SMS APIs: SMS reception.
- Cross-platform-compatible cryptography library + Android Keystore: End-to-end encryption and local key protection. HTTPS: Direct Bailian access and service calls.
- Gradle Wrapper + Android Gradle Plugin: Dependency management and APK builds.

### Testing

- Android Emulator: Local emulation, accelerated by KVM on Linux or Windows Hypervisor Platform on Windows.
- ADB: Installation, SMS injection, logs, screenshots, and background state testing.
- JUnit, Compose Test, and UI Automator: Business logic, UI, and system interaction tests.
- Test identities and simulated SMS in development builds: Integration testing. Physical Android devices: Acceptance testing for WeChat or QQ login, QR scanning, real SMS, background reliability, latency, and power consumption.

### Deployment

- [Gradle release builds and APK signing](https://developer.android.com/build/build-for-release): Generate release packages.
- ADB: Local test installation. Signed APK downloads: Initial distribution method.

## Service

Repository: `pin-glide-service`.

### Development Environment Setup

- IDE: VS Code with the Go extension and Go language tooling.
- Tools: Git, the Go toolchain and Go Modules, Docker Engine, and Docker Compose v2. Pin Jet and psqldef as repository Go tools.
- Reference: Follow the Go/PostgreSQL/Huma/Jet/psqldef stack and Docker deployment workflow in [helm-gateway](https://github.com/BuddyLabsCo/helm-gateway).
- Server: Prepare the project's own Linux server with Docker access, deployment-tool prerequisites, container registry read access, and a reverse proxy providing public HTTPS/WSS endpoints.

### Build

- Go + Huma: Authentication, device management, configuration, and ciphertext routing HTTP APIs. Go handles authenticated WSS connections and live ciphertext delivery. Huma is the HTTP/API framework.
- Export the complete client-facing HTTP contract as `openapi.yaml`, including operations, parameters, authentication requirements, request/response schemas, and errors.
- Export the complete WSS contract as `asyncapi.yaml`, including endpoint addresses, handshake parameters and authentication, message directions and schemas, correlation/receipt semantics, and documented error/close behavior. Keep shared wire definitions consistent and bundle references so both descriptions are usable without a deployed service.
- Service owns these descriptions and their release artifacts. Android and Windows own how they consume a pinned contract release, choose generation/conversion tools, and produce or adapt their clients.
- Go Modules and Go build tooling: Dependency management and backend builds.
- PostgreSQL + Jet: Persistent business data and schema-generated typed database queries/models.
- psqldef: Deploy the declarative PostgreSQL schema. Keep versioned structural/data upgrade scripts distinct from target-schema application, following the reference deployment workflow.
- Generate Jet code from the target schema using a disposable PostgreSQL instance; generation does not connect to deployed databases. Validate generated artifacts before packaging.
- Docker: Package the Go backend and the matching SQL/deployment tools as versioned release images.
- WeChat and QQ server APIs plus the Bailian temporary API key API: identity validation and model credential requests.

### Testing

- Go tests and isolated local PostgreSQL instances: Business logic, database integration, schema evolution, and failure-path checks. Test fixtures and generated artifacts do not use production data or databases.
- Validate both API descriptions, reference resolution, HTTP/WSS route and schema coverage, consistency with service behavior, and supported-client compatibility. Release descriptions and examples contain no operational credentials or user message data.
- HTTP/WebSocket test scripts: Verify authentication, device routing, encrypted pairing relay, receipt acknowledgments, and failure handling against local test instances and the deployed development environment.
- Android and Windows clients connect to the deployed development environment: End-to-end acceptance and public-internet latency measurement before production release.
- Container/service logs and deployment health checks: Troubleshooting and result verification. Logs exclude SMS/OTP plaintext, message ciphertext, client encryption keys, and secret values.

### Deployment

- Own Linux server + Docker Compose: Run the Go backend and PostgreSQL. The server pulls versioned Docker images rather than building application source on the host.
- Repository deployment tool: Follow [helm-gateway's `install.sh` workflow](https://github.com/BuddyLabsCo/helm-gateway/blob/ba375819b98bcd76fc27d7a15b2523aa62837c6f/deploy/compose/README.md) for initial installation and updates.
- Initial deployment: Prepare configuration and registry access, pull matching release images, start PostgreSQL, apply the schema with psqldef and initialize required data, then start the service and verify health.
- Update deployment: Prepare and pull matching release images, stop application writes, back up PostgreSQL, run the ordered schema/data upgrade steps with psqldef apply/check, then restart the service and verify health. Preserve existing data and reject unsupported downgrades.
- Separate `development` and `production` deployments: Independent Compose projects, PostgreSQL databases/volumes, endpoints, and protected secret configuration. Validate in `development` before releasing to `production`.
- Deployment configuration supplies database credentials, optional WeChat and QQ application credentials, and the permanent Bailian API key without putting secrets in images, source files, or logs. The operator's reverse proxy terminates HTTPS/WSS.
- Publish `openapi.yaml` and `asyncapi.yaml` as GitHub Release assets from the same service tag/source commit as the Docker release. Publish the release only after both descriptions are validated and attached.
- Keep published description assets stable; corrections use a new release. Callers pin a service release tag and retrieve descriptions during development or builds, then select their own client generation/conversion workflow. Private-repository download credentials belong to the development/CI environment.
- Future static product homepage: Owned by this repository and hosted on the project's own server; its UI and implementation timing are a separate effort.

The deployment tool exposes the reference preparation and execution phases:

```sh
# First installation
./install.sh --version X.Y.Z --prep --initial-install
./install.sh --version X.Y.Z --initial-deploy

# Update or same-version redeployment
./install.sh --version X.Y.Z --prep
./install.sh --version X.Y.Z --full-release
```

## Windows

Repository: `pin-glide-windows`.

### Development Environment Setup

- IDE: VS Code with rust-analyzer and Slint extensions.
- Tools: Git, rustup (install the Rust MSVC toolchain, Cargo, Clippy, and rustfmt), MSVC Build Tools (Desktop development with C++ workload), Windows SDK, and NSIS.
- Environment: A physical Windows machine or VM for Windows builds and system integration tests. Desktop interaction tests run in a logged-in, unlocked user session.
- Skills: Install the [official Slint Skill](https://github.com/slint-ui/ai-plugins) (`slint`) and [Impeccable](https://impeccable.style/) for UI implementation and design requirements respectively, following Fluent styling.

### Build

- Rust + Cargo: Background logic and builds.
- Slint + Fluent styling: Settings, pairing UI, and app notification popups.
- Rust async networking libraries: WSS connections and service calls. Cross-platform-compatible cryptography library: End-to-end encryption.
- `windows` crate + Windows APIs: Global shortcuts, clipboard, system tray, window behavior, and local key protection.

### Testing

- Cargo Test, Clippy, and rustfmt: Business logic tests, static checks, and formatting.
- Linux: Development validation of portable business logic and Slint UI.
- Windows VM or physical machine: Acceptance testing for popups, shortcuts, clipboard, system tray, startup behavior, and resource usage.
- Android/service test environment: Device binding, encryption and decryption, disconnection handling, and end-to-end latency testing.

### Deployment

- Cargo Release: Generate Windows executables.
- [NSIS](https://nsis.sourceforge.io/Main_Page): Generate installers and uninstallers.
- [Windows SDK SignTool](https://learn.microsoft.com/en-us/windows/win32/seccrypto/signtool) + code signing certificate: Sign official releases.
- Installer downloads: Initial distribution method.
