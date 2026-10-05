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
- WeChat SDK: WeChat authorization and login. Android SMS APIs: SMS reception.
- Cross-platform-compatible cryptography library + Android Keystore: End-to-end encryption and local key protection. HTTPS: Direct Bailian access and service calls.
- Gradle Wrapper + Android Gradle Plugin: Dependency management and APK builds.

### Testing

- Android Emulator: Local emulation, accelerated by KVM on Linux or Windows Hypervisor Platform on Windows.
- ADB: Installation, SMS injection, logs, screenshots, and background state testing.
- JUnit, Compose Test, and UI Automator: Business logic, UI, and system interaction tests.
- Test identities and simulated SMS in development builds: Integration testing. Physical Android devices: Acceptance testing for WeChat login, QR scanning, real SMS, background reliability, latency, and power consumption.

### Deployment

- [Gradle release builds and APK signing](https://developer.android.com/build/build-for-release): Generate release packages.
- ADB: Local test installation. Signed APK downloads: Initial distribution method.

## Cloudflare

Repository: `pin-glide-service`.

### Development Environment Setup

- IDE: VS Code with built-in TypeScript support.
- Tools: Git, Node.js LTS, and npm. Install [Wrangler](https://developers.cloudflare.com/workers/wrangler/) in the project.
- Skills: Install official [Cloudflare Skills](https://github.com/cloudflare/skills), using `wrangler`, `durable-objects`, and `workers-best-practices`.
- Account: Set up a Cloudflare account and authenticate Wrangler.

### Build

- TypeScript + Hono: Authentication, device management, configuration, and ciphertext routing APIs.
- npm + Wrangler: Dependency management and Workers builds.
- SQL migration files: D1 schema management.
- WeChat server APIs and Bailian temporary API key API: Identity validation and model credential requests.

### Testing

- All service tests run against deployed Cloudflare environments. No local testing or local runtime simulation.
- HTTP/WebSocket test scripts: Access the cloud test service to verify authentication, device routing, pairing relay, receipt acknowledgments, and failure handling.
- Android and Windows clients connect to the cloud test environment: End-to-end integration and public internet latency testing.
- Wrangler real-time logs and Cloudflare logs: Cloud troubleshooting and result verification.

### Deployment

- Cloudflare Workers free plan: Network service.
- Cloudflare D1 free plan: Business configuration and status logs.
- Durable Objects free SQLite mode + WebSocket Hibernation: Real-time connections and ciphertext delivery.
- Wrangler: Deploy services, apply database migrations, and configure Workers Secrets.
- Wrangler environments: Deploy separate `staging` and `production` environments with independent D1 databases, Durable Objects namespaces, and Secrets.
- Deploy versions to `staging` for cloud testing before releasing to `production`.
- Cloudflare Pages free plan: Future static product homepage.

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
