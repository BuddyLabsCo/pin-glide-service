# PinGlide Technical Overview

## Goals

PinGlide forwards SMS verification codes from Android to Windows. Android receives candidate OTP SMS messages, calls Alibaba Cloud Model Studio (Bailian) directly to extract the OTP, and encrypts the full SMS body and OTP for cloud forwarding to Windows. The receiver decrypts the message and displays an app notification popup with a “Copy OTP” button and a configurable global shortcut.

The phone and desktop communicate over the public internet. With normal network conditions, all required phone permissions granted, and the desktop online, the target time from SMS arrival to the desktop popup is **within 5 seconds**, subject to end-to-end measurement.

## Repositories and Responsibilities

See the [technology stack overview](pin-glide-tech-stack-overview.md) for technology choices, build, testing, and deployment.

| Repository | Responsibilities |
| --- | --- |
| `pin-glide-android` | WeChat login, device and rule management, key agreement, initial SMS filtering, temporary API key caching, direct Bailian extraction, result validation, and encrypted uploads |
| `pin-glide-service` | WeChat login validation and account association, device authorization, login credential issuance, model configuration queries, Bailian temporary API key requests, encrypted pairing message relay, and real-time ciphertext routing |
| `pin-glide-windows` | QR code pairing, key agreement, background reception and decryption, app popups, copy buttons, global shortcuts, and clipboard operations |

Cloudflare infrastructure uses free plans. D1 stores users, devices, credential digests, forwarding rules, Bailian model configuration, and processing status logs. Durable Objects coordinate real-time connections. A future static product homepage belongs to `pin-glide-service`.

## Identity and Device Binding

- Android signs in through the WeChat SDK. Workers validate the authorization code, verify the WeChat identity, and create or associate a user account. WeChat integration uses an approved mobile application.
- Windows requests a pairing session and displays a one-time QR code. The user scans it with the authenticated Android app and confirms the desktop and receiving relationship.
- During initial device binding, each client generates an asymmetric key pair locally. They agree on a shared symmetric key through an encrypted pairing channel authenticated by the QR scan. Public keys, private keys, and shared keys remain in secure local storage on the clients.
- The desktop QR code contains its local public key and locally generated pairing authentication data. Workers relay key agreement messages as ciphertext. Binding results sent to the server contain only device identifiers and authorization status.
- Each phone-desktop pair establishes its own communication key. Reinstalling a client or replacing a key requires pairing again by QR code.
- The server automatically issues separate login and device credentials for each client. The phone user session manages the user's devices and rules. Upload credentials submit ciphertext and request temporary API keys; receiving credentials subscribe to ciphertext assigned to that receiver.
- Android lists the user's paired desktops and their online status, with options to rename, remove, and revoke authorization. Revocation closes the corresponding connection and removes the desktop from forwarding rules.

## Model Extraction and Temporary API Keys

- The model adapter runs on the sender and supports only Bailian. D1 defines the model in use, and Workers provide its current configuration to the sender.
- The platform's permanent Bailian API key is stored as a Workers Secret and authorized for the required models. After authenticating the device, Workers request a temporary API key from Bailian and return it with the model configuration and key expiration time.
- Android caches the temporary API key and its model configuration. It reuses a valid cache and requests a new key from Workers when the cache is missing or expired.
- Android calls Bailian directly over HTTPS in non-thinking mode to obtain a concise structured result, then validates the OTP against the original text locally.
- SMS plaintext is processed only by the sender, Bailian inference service, and receiver. Workers handle credential requests, configuration, routing metadata, and ciphertext. Device encryption keys remain exclusively on the clients.

## Encryption, Delivery, and Display

- Android configures “sender phone → target desktop” rules, which are stored in D1. The server selects recipients using authenticated device identities and forwarding rules.
- The sender uses established authenticated encryption to encrypt the full body and OTP separately for each target device using that pair's communication key. Message identifiers support deduplication, receipt acknowledgments, and replay protection.
- Uploads are event-driven. On systems requiring the default SMS app role, the sender fulfills that role's responsibilities.
- Windows runs a native background application in the user session, maintains a WSS connection, validates messages, and decrypts them. The app popup shows the full SMS body. Its button copies that message's OTP; the shortcut copies the latest cached OTP.
- The server holds message ciphertext temporarily in memory. It releases ciphertext after successful delivery, failure, an offline target, or request rejection. D1 and application logs store only message identifiers, devices, timestamps, and status. Reconnected clients receive only new messages.
- The sender releases its processing copies of the body and OTP when processing finishes. The receiver releases the body when the popup closes. It caches the OTP only in memory and clears it after the locally configured cache duration.
- Core business logic and platform integration are separate layers. Protocols are defined in `pin-glide-service`. The backend can later be replaced with Node.js, SQLite, and a conventional WebSocket service.

## Client Installation Flow

```mermaid
flowchart TD
    subgraph ANDROID[Android Sender]
        A1["Download and install the sender app"] --> A2["Launch the app"]
        A2 --> A3{"Does the system require the default SMS app role?"}
        A3 -->|Required| A4["Set as the default SMS app"]
        A3 -->|Not required| A5["Grant SMS access permissions"]
        A4 --> A5
        A5 --> A6["Configure background operation and battery optimization permissions"]
        A6 --> A7["Setup complete; proceed to WeChat login and device binding"]
    end

    subgraph WINDOWS[Windows Receiver]
        D1["Download and install the receiver app"] --> D2["Launch the app"]
        D2 --> D3["Configure the copy shortcut"]
        D3 --> D4["Configure launch at login"]
        D4 --> D5["Setup complete; proceed to QR code pairing"]
    end
```

## Sequence: SMS Arrival to Copying the OTP

```mermaid
sequenceDiagram
    autonumber
    participant A as Android Sender
    participant W as Workers Service
    participant L as Bailian
    participant D as Windows Receiver
    participant C as Windows Clipboard
    actor U as User

    Note over A,D: Devices are paired, communication keys agreed, and desktop WSS connected
    A->>A: Receive SMS and filter candidate OTP messages
    alt Temporary API key cache is missing or expired
        A->>W: Request a temporary API key with device credentials
        W->>L: Request a temporary API key using the platform's permanent key
        L-->>W: Return temporary API key and expiration time
        W-->>A: Return temporary API key and model configuration from the database
        A->>A: Cache temporary API key and model configuration
    else Cache is still valid
        A->>A: Reuse cached temporary API key and model configuration
    end
    A->>L: Submit SMS body directly to the configured model
    L-->>A: Return OTP or no result
    A->>A: Validate extraction result locally
    alt Result is valid
        A->>A: Encrypt full body and OTP
        A->>W: Upload ciphertext for the target device
        alt Desktop is online and delivery succeeds
            W-->>D: Forward ciphertext
            D->>D: Validate, decrypt, and cache temporarily
            D-->>W: Acknowledge receipt
            W->>W: Log status metadata and release delivery ciphertext
            D-->>U: Show full SMS and copy button in an app popup
            U->>D: Click copy or press the configured shortcut
            D->>C: Write the corresponding OTP
            D->>D: Release body when popup closes, then clear OTP after cache duration
        else Target is offline or delivery fails
            W->>W: Log status metadata and release ciphertext
        end
        W-->>A: Return delivery status
    else No valid result
        A->>A: End processing
    end
    A->>A: Release this operation's body and OTP copies
```

## Sequence: Server Processing

```mermaid
sequenceDiagram
    autonumber
    participant A as Android Sender
    participant W as Workers API
    participant DB as D1
    participant L as Bailian Credential API
    participant B as Durable Object
    participant D as Windows Receiver

    opt Sender's temporary API key cache is missing or expired
        A->>W: Request a temporary API key with device credentials
        W->>DB: Validate device identity and query enabled model configuration
        W->>L: Request a temporary API key using the platform's permanent key
        L-->>W: Return temporary API key and expiration time
        W-->>A: Return temporary API key and model configuration
    end
    Note over A,D: Model extraction and encryption take place on the sender
    A->>W: Upload encrypted payload, message ID, and target device IDs
    W->>DB: Validate credentials, device ownership, forwarding rules, and deduplication status
    alt Request is valid and message has not been processed
        loop Each authorized target desktop
            W->>B: Pass ciphertext for this desktop
            alt Target desktop is online
                B-->>D: Push ciphertext over the existing WSS connection
                alt Receipt acknowledgment arrives
                    D-->>B: Acknowledge receipt
                    B->>DB: Log successful delivery metadata
                else Sending fails or acknowledgment times out
                    B->>DB: Log failed delivery metadata
                end
            else Target desktop is offline
                B->>DB: Log offline status metadata
            end
            B->>B: Release this delivery's ciphertext
        end
    else Request is invalid or message is a duplicate
        W->>W: Reject request or return existing processing status
    end
    W->>W: Finish processing and release request ciphertext
    W-->>A: Return processing status
    Note over W,DB: Server holds identity, configuration, and status. Clients hold end-to-end encryption keys
```

## Sequence: Login, Device Binding, and Link Verification

The sender app generates a test message and processes it through the normal Bailian extraction, end-to-end encryption, and delivery flow. The test verifies the desktop popup, copy button, and shortcut.

```mermaid
sequenceDiagram
    autonumber
    actor U as User
    participant A as Android Sender
    participant X as WeChat
    participant W as Workers Service
    participant D as Windows Receiver
    participant L as Bailian

    Note over A,D: Both clients are installed and configured
    U->>A: Select WeChat login
    A->>X: Request WeChat authorization
    X-->>A: Return authorization code
    A->>W: Submit authorization code
    W->>X: Validate authorization code and obtain user identity
    X-->>W: Return identity
    W-->>A: Create or associate user account and issue phone credentials
    D->>W: Request a pairing session
    W-->>D: Return pairing session ID
    D->>D: Generate key pair and pairing authentication data locally
    D-->>U: Display one-time QR code with desktop public key and authentication data
    U->>A: Scan QR code with the app and confirm the desktop
    A->>A: Verify desktop public key and generate local key pair
    A->>W: Authorize desktop binding, submitting only device and session IDs
    W-->>D: Issue desktop device credentials
    A->>W: Submit encrypted pairing message
    W-->>D: Forward encrypted pairing message
    D->>W: Submit encrypted key agreement response
    W-->>A: Forward encrypted key agreement response
    Note over A,D: Clients complete authenticated key agreement and store the shared symmetric key locally
    A->>W: Submit binding completion status and selected receiving targets
    W-->>A: Save rules and return the user's device list
    D->>W: Establish authenticated WSS connection
    U->>A: Click send test message
    opt Temporary API key cache is missing or expired
        A->>W: Request temporary API key and model configuration
        W-->>A: Return temporary API key and model configuration
        A->>A: Cache temporary API key and model configuration
    end
    A->>L: Call Bailian directly to extract OTP from simulated SMS
    L-->>A: Return OTP
    A->>A: Validate locally and encrypt
    A->>W: Upload ciphertext of simulated SMS and OTP
    W-->>D: Forward ciphertext
    D->>D: Decrypt and show full simulated SMS with copy button
    U->>D: Test copying with both the button and configured shortcut
    D-->>U: Verify by pasting the copied OTP, confirming link verification passes
```
