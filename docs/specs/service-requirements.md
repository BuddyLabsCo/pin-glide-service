# Pin Glide Service Functional Specification

Status: approved functional specification and shared v1 contract, adopted by the user in [the functional resolution](https://github.com/BuddyLabsCo/pin-glide-service/issues/2#issuecomment-6080960446). The agreed behavior includes the standard business-error response, Sender lifecycle and concrete route/schema/crypto framing. Sender message-retry policy and repository performance testing are outside service scope. Runtime implementation, generated YAML and cross-client interoperability remain future validation.

Canonical planning context: [Wayfinder map: service requirements and shared protocol](https://github.com/BuddyLabsCo/pin-glide-service/issues/1) and [Clarify service functional requirements and shared client contracts](https://github.com/BuddyLabsCo/pin-glide-service/issues/2). The source baseline is service commit 566ec02e9c0a8a89b073f3f11cd1c3d68051c4e7.

## 1. Scope and ownership

Pin Glide forwards verification messages from Android Senders to Windows Receivers. Android reads/filter SMS, directly calls Bailian to extract an OTP, validates it against the original SMS, and independently encrypts the full SMS/OTP for each selected Pairing. The service authenticates and authorizes requests, supplies temporary provider credentials/configuration, relays pairing traffic, and routes live ciphertext. Windows acknowledges arrival, then validates/decrypts and owns popup, copy and shortcut behavior.

The service owns the shared HTTP/WSS and cryptographic profile descriptions, including explicit business-error outcomes in openapi.yaml and asyncapi.yaml. Android and Windows own their platform behavior, secure storage, client generation/adaptation, UI, and decisions about retrying message uploads after an error or uncertain response.

Use one active Go service instance with in-memory routing and exclusive WSS admission. Multi-instance deployment, distributed routing and shared connection coordination are out of scope.

No offline ciphertext backlog, historical SMS API, server-side inference, additional model provider, installation recovery secret, client replay bitmap/sequence, or per-message AAD binding is required.

## 2. Identities and authorization

- An Account is associated with the provider, configured AppID and verified OpenID. WeChat and QQ identities remain separate even when their AppID and OpenID strings match. Allocate an internal Account ID. UnionID is optional and does not automatically merge Accounts.
- Any successfully verified identity for a completely configured approved mobile app can create/associate an Account. No allowlist, invitation or approval gate.
- Neither provider pair is required at process startup. A missing or partial pair fails only an operation that uses that provider, without issuing identity.
- QQ uses the mobile server-side authorization-code flow. The service exchanges the code with its configured AppID and AppKey, then trusts only the provider-returned OpenID bound to that AppID. UnionID permission is not required.
- Sender and Receiver identities are app-installation registrations. The service assigns deviceId. Reinstallation creates a new identity and requires re-pairing.
- Sender re-login supplies fresh WeChat or QQ authorization and its saved deviceId. Restore only an active registration owned by that Account. deviceId alone is not authentication, and this is not a hardware/cloning proof.
- Ordinary Sender logout stops new local forwarding and revokes its service credentials while preserving its Pairings and local pairing keys.
- Account switching revokes the old Sender credentials, removes that Sender's own Pairings, clears its old local identity/keys, and creates a new registration.
- Ordinary management changes only the current Sender's associations. Fresh WeChat or QQ identity verification additionally permits Account-owner security revocation of any owned device. Global Receiver revocation closes its connection and affects all of its Pairings.
- A shared Receiver may have independent Pairings with multiple Senders in the same Account. Removing one Pairing does not remove the others or close their shared delivery connection.

### Sender login, security revocation and re-pairing

The user approved these lifecycle rules in [Sender lifecycle and re-pairing](https://github.com/BuddyLabsCo/pin-glide-service/issues/2#issuecomment-6080579614).

Fresh verified WeChat or QQ login restoring the same active Sender atomically revokes its previous M/U/F family and issues a new family, preserving Pairings and local keys. Serialize issuance with refresh/logout/security revocation; delayed old operations cannot restore the replaced family. A revoked previous family cannot revoke a later freshly authenticated family. Ordinary refresh keeps its approved access-token overlap rules.

Account-owner security revocation of a Sender permanently closes that registration, invalidates all its credentials, cancels its unfinished authorized pairing operations and removes its Pairings. The old deviceId cannot be restored. A fresh login without the old Sender ID creates a new registration for subsequent re-pairing. Return 409 device_registration_revoked for an owned security-revoked Sender ID; unknown or caller-invisible IDs return 404 resource_not_found. Never recreate a supplied old ID. Other Senders' Pairings and the Receiver's shared credential/connection remain unaffected. Previously admitted Deliveries retain the original authorization/lifecycle rule. Ordinary logout and refresh-family replay revocation do not permanently close a registration.

When re-pairing the same Sender/Receiver, keep the old Pairing active until both clients save and confirm the new key. Atomically activate the new Pairing ID and retire only that Sender's old Pairing. Failure/cancellation/expiry before commit preserves the old association. A pending replacement counts as the same distinct Receiver for capacity. Preserve the current own alias unless the caller supplies a replacement alias. A concurrent stale replacement whose expected active association has already changed fails with operation_superseded, preserving the current association.

### Binding capacity

Each Sender has at most 5 distinct associated Receivers. Active bindings and confirmed pending reservations must be admitted atomically; failed/canceled/expired reservations release capacity. Re-pairing the same Receiver does not consume a second slot. Removed/revoked bindings do not count.

Reject a sixth distinct Receiver with receiver_limit_reached. There is no additional approved Account/device-count quota, payload-size cap, request-size cap, per-user usage quota or billing system. Mandatory protocol validation and fixed cryptographic lengths remain. No unbounded-capacity guarantee is implied.

## 3. Credential types

All service credentials are independent, unpredictable opaque tokens. Store only digests and lifecycle/scope metadata in PostgreSQL. Send secrets through authenticated HTTPS/WSS, never URLs or logs.

| Credential | Holder and authority | Lifetime |
| --- | --- | --- |
| User Session token M | One Sender; own Pairing/device management | 3 hours |
| Uploader token U | Same Sender; uploads, own result lookup, model configuration/temporary-key requests | 3 hours |
| Refresh token F | Renew that Sender's M and U bundle | Sliding 7 days |
| Receiver token R | One Receiver; its delivery subscription/receipts and authenticated pairing-session creation | Until revocation |
| Pairing token B | One receiver-side Pairing Session; restricted handshake, completion and authorized credential redemption | 5 minutes |

### Sender renewal

A login/refresh returns M, U and F with expiry timestamps. Both foreground management and background uploading share one single-flight refresher. Refresh before a required operation when access expiry is within 5 minutes, or after an access-expired rejection that occurred before business work.

Each successful refresh generates fresh M/U/F, with expiration now + 3h/3h/7d. No additional absolute session-age cap. Seven days without successful renewal requires WeChat or QQ login. Existing M/U tokens keep their original expirations during ordinary rotation.

Use the approved operation ID and attempt ordering: original attempt 0, at most one uncertain-transport retransmission attempt 1, same operation ID. If attempt 1 reaches the service first while F is still current, it can perform the original rotation; a later attempt 0 is superseded. The 30-second overlap/original-expiry check applies only when a higher attempt uses an already-consumed immediate predecessor. A recorded lower/equal attempt is superseded without new issuance or family revocation. Preserve this guard after later operations advance the family.

A genuine unexpired spent-token replay revokes the family and its M/U tokens. Unknown/expired tokens do not revoke guessed or unrelated families. PostgreSQL issuance and logout are serialized. The client atomically stores a new bundle, persists pending-operation state, and rejects callbacks from an old local login/logout generation.

Explicit logout invalidates the current Sender's credentials; other installations are unaffected. A valid self access or refresh credential can authorize self-revocation, including when access has expired. Stop local forwarding immediately; do not claim remote revocation if the request never reached the service.

## 4. Pairing and Receiver credential acquisition

A Pairing Session is one-time, valid for 5 minutes, and may be confirmed by one authenticated Sender. Its mode and target Receiver are fixed at creation and cannot be switched by a later request.

| Session mode | Entry authority | Effect and credential authority |
| --- | --- | --- |
| new_receiver | New Windows context; subsequent owning Sender confirmation | Reserve capacity, establish a new Pairing, and permit first R issuance |
| add_sender | Existing R for that Receiver; owning Sender confirmation | Establish/replace only that Sender's Pairing; no R issuance or replacement |
| recover_receiver | Fresh Account-owner WeChat or QQ verification and explicit replacement approval | Confirm existing key possession where available, replace R only; no new Pairing or binding-capacity reservation |

The normal new-receiver flow is:

1. Windows requests a session. Allocate a pending Receiver/session and return restricted B only to Windows.
2. Windows generates QR and cryptographic material locally. The QR contains session/Receiver references and client handshake material. It contains neither B nor R.
3. Windows opens the restricted pairing channel with B. Android scans and confirms using M. Verify Account ownership and reserve Sender capacity.
4. Relay opaque handshake messages. Both clients derive and securely save the Pairing message key and perform authenticated key confirmation.
5. Each side reports completion using its own credential. Activate the Pairing only after both reports.
6. Notify Windows that R can be retrieved. Windows redeems it over HTTPS using B; save only R's digest and return raw R only to Windows.
7. Windows persists R before using it for delivery WSS. Valid R presentation proves possession and consumes the issuance grant.

One session has one issuance operation ID, with original attempt 0 and one retransmission attempt 1. A higher attempt may replace an unclaimed credential only while B is live and that grant remains current. Lower/equal attempts cannot overwrite it. Once R proves possession, temporary-session redemption cannot rotate it.

Each Receiver has one current issuance generation. Newly owner-approved recovery supersedes older grants. Serialize redemption, recovery/revocation and final WSS admission per Receiver.

### Additional Sender and recovery

An already credentialed Receiver authenticates add_sender session creation with existing R. Activate only the new/replacing Pairing for the confirming Sender; preserve R, its delivery connection and all other Pairings. Its B token cannot enter the receiver-token issuance path: return mode_forbids_credential_issue without changing credentials.

If R is lost, a recover_receiver QR requires fresh WeChat or QQ Account-owner verification and explicit replacement approval. Clients confirm possession of an existing Pairing key where available through their authenticated reports; do not create a new Pairing or reserve a sixth binding for pure credential recovery. Revoke old R and close/cancel its active/pending connection, then return the replacement through the restricted session. Explicitly tell the approving owner that replacement interrupts this shared Receiver's connection for all of its Senders. Preserve Pairings only where Windows still has the local keys; missing keys require a separate pairing operation. New installations use new registrations. A public deviceId cannot independently recover a credential.

## 5. Cryptographic profile v1

This is the adopted interoperability profile for the selected primitives. Two-client interoperability and security validation remain required before contract release.

### QR and HPKE setup

- cryptoVersion is 1.
- Windows creates a fresh P-256 recipient key pair and cryptographically random 32-byte QR PSK per session.
- QR includes sessionId, receiverId, recipient public key and PSK; the private key stays on Windows.
- Public key and HPKE enc are 65-byte SEC1 uncompressed P-256 points. Validate public points and abort on invalid input.
- HPKE mode is PSK (0x01); KEM/KDF/AEAD are 0x0010 / 0x0001 / 0x0002.
- info is ASCII pin-glide/pairing/v1. psk_id is UTF-8 sessionId. HPKE AAD is the zero-length byte string.
- Android sends enc plus one HPKE-sealed JSON init containing type=pairing_init and a fresh 32-byte challengeA.
- Both derive the 32-byte persistent message key using Export with ASCII context pin-glide/message-key/v1 and output length 32.

### Key confirmation

Windows sends AES-256-GCM under the exported message key with a fresh nonce and authenticated JSON containing type=key_confirmation, role=receiver, challengeA and a new 32-byte challengeW.

Android authenticates and strictly checks type, role, challenge and handshake state, then returns AES-GCM confirmation with role=sender and challengeW. Windows verifies it. Only then may each client report completion.

Typed confirmation payloads cannot be displayed as verification messages. Do not substitute unchecked bytes, omit challenge validation, or treat a service status report as cryptographic proof.

### Message encryption

Use AES-256-GCM with the per-Pairing message key, a fresh cryptographically random 96-bit nonce, full 128-bit tag, and zero-length AAD.

Binary ciphertext encodes ciphertext followed by its 16-byte tag. Wire byte strings use standard padded Base64. This profile uses no sequence field, replay bitmap or message AAD.

The encrypted UTF-8 JSON is:

~~~json
{
  "type": "verification_message",
  "smsText": "Example verification message: 123456",
  "otp": "123456",
  "receivedAt": "2026-10-09T00:00:00Z"
}
~~~

The first three fields are required; receivedAt is optional client-origin display metadata. Android verifies the OTP against the SMS before encryption. Windows checks authenticated type and payload format before displaying/copying.

Honor the random-IV GCM key-use bound: both clients together perform at most 2^32 encryptions under a message key, including confirmations; rekey/re-pair before that bound. This is a cryptographic use constraint, not a service usage quota or replay sequence.

No promise of forward secrecy or content-level/end-to-end replay rejection. Pairing keys remain client-only. No client encryption keys, plaintext SMS/OTP or message ciphertext are persisted by the service; protected server-side provider/TLS secrets retain their existing roles.

## 6. Live delivery and status

One Message ID is generated once by the Sender for one SMS operation; all Receiver ciphertexts share it. An upload contains Sender-selected Receiver IDs and their independent ciphertexts. Sender identity comes from U.

For each processable entry:

1. Validate the current request's credential, ownership and Receiver/Pairing eligibility.
2. Atomically claim the retained (Sender ID, messageId, Receiver ID) identity. Only the winning claim may start a Delivery; a separate lookup followed by an unguarded insert is insufficient.
3. Register processing metadata before WSS side effects and publish the single live work result for duplicate callers.
4. Route once through the current delivery connection, or produce offline.
5. Wait for arrival receipt or terminal send/ACK failure.
6. Persist the terminal result and release that entry's ciphertext. Release request copies when processing ends.

An existing terminal identity returns its retained result. An existing processing identity joins the original work and waits for that same terminal result within the caller's remaining budget; it never creates another send. Release the duplicate request's unnecessary ciphertext copy. If a processing record has no live owner after interruption, finalize interrupted when storage permits rather than resume it. If no terminal result can be obtained within the request budget, return HTTP 503 delivery_status_unavailable as an uncertain outcome; do not return normal pending success, overwrite the original work or initiate another send.

Process entries in parallel. WSS write deadline is 2 seconds, ACK deadline 5 seconds after sending, total service batch budget 15 seconds. The budget is an upper bound, not a fixed delay or a promise that every end-to-end operation takes 15 seconds.

Freeze the original authorization decision for an admitted request. Revocation after validation does not add a pre-send check. A new request validates again before historical-result lookup. Preserve old Delivery history when recording later authorization failures.

Receiver ACK means arrival only. Send it after minimal correlation metadata is readable, before cryptographic/business validation. Invalid/decryption-failed business data does not suppress ACK. An unparseable frame with no usable correlation metadata is a protocol error, not a fabricated receipt.

HTTP authentication/whole-request parsing failures return ordinary 4xx errors. A processable batch returns 207 with one result per input entry. Invalid Receiver entries do not stop valid entries. Duplicate Receiver IDs within one batch are rejected per duplicated entry; unrelated unique entries continue, avoiding ambiguous selection of different ciphertexts for the same Delivery.

| Status | Meaning |
| --- | --- |
| received | Authenticated Receiver reported arrival; not proof of decrypt/display/copy |
| offline | No usable current delivery connection |
| rejected | Entry not authorized or structurally valid for this operation |
| send_failed | WSS write failed or timed out; arrival is not guaranteed absent |
| ack_timeout | No receipt observed within the ACK deadline; arrival may have occurred |
| interrupted | Registered work has no confirmable terminal outcome after interruption |

Terminal results do not change on a late ACK. Service performs no proactive delivery retries or offline/history replay.

HTTP disconnect does not cancel registered work. Service restart marks old unfinished work interrupted, without recovering/resending ciphertext. If final-status storage fails, return a server failure/uncertain outcome; never retransmit to compensate. When storage is available, an orphaned registered operation whose processing deadline has passed becomes interrupted instead of restarting its delivery.

### Business outcomes and client responsibility

Return explicit, stable business-error codes and distinguish rejection before delivery, a retained terminal result, and an uncertain delivery outcome. Document their HTTP/207/WSS meanings in the published YAML contracts. An error such as send_failed, ack_timeout or interrupted does not prove that the Receiver received nothing.

Sender message-retry behavior and client HTTP timeout selection belong to Android; the service specification does not prescribe an automatic retry count, retry schedule, polling policy or retry queue. Normal upload remains synchronous. The separately approved credential-refresh and Receiver-token issuance operation/attempt protocols remain part of the authentication contract.

## 7. Receiver delivery connection and presence

One active long-lived R per Receiver; one reserved/active delivery WSS slot. Authenticate and atomically reserve before HTTP 101. A second same-token upgrade returns HTTP 409 receiver_already_connected and preserves the first.

Serialize final 101 commit with explicit revocation/recovery. Revocation winning first cancels the pending upgrade; an earlier completed upgrade is closed by revocation. Connection-ID ownership governs cleanup; an old callback cannot remove a newer slot.

Ping every 15 seconds, require Pong within 5 seconds, then close/release on failure. Online starts after successful 101; current-connection close/revocation, Pong timeout or service restart means offline. Rejecting another/invalid handshake never changes a healthy current connection's presence. ACK is not a presence signal.

Expose online/offline and stateChangedAt to the owning Sender. Reconnected Receivers receive only new messages.

## 8. Bailian configuration

The selected default is Beijing qwen3.7-flash, OpenAI-compatible Chat Completions, thinking disabled and strict JSON Schema output. The [official model page](https://help.aliyun.com/zh/model-studio/qwen3-7-flash), [thinking guide](https://help.aliyun.com/zh/model-studio/deep-thinking) and [structured-output guide](https://help.aliyun.com/zh/model-studio/qwen-structured-output) document support for this combination.

The service returns the configured Workspace-specific Beijing base URL, https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1, with the model and inference settings. Android posts text-only messages to /chat/completions. Use enable_thinking=false explicitly; the provider enables thinking by default. Do not rely on a prompt instruction to disable it. The [regional endpoint guide](https://help.aliyun.com/zh/model-studio/regions/) recommends this host; do not invent a Workspace ID or switch region implicitly.

Extraction settings, excluding the Android-owned SMS prompt:

~~~json
{
  "model": "qwen3.7-flash",
  "enable_thinking": false,
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "otp_extraction",
      "strict": true,
      "schema": {
        "type": "object",
        "properties": {
          "otp": {"type": ["string", "null"]}
        },
        "required": ["otp"],
        "additionalProperties": false
      }
    }
  }
}
~~~

An absent OTP is represented by null. Android still validates output fields, rejects empty/invalid OTPs, and checks original-SMS membership before encryption/upload. Schema-conforming output alone does not establish that an OTP is correct. Keep inference text-only: the provider documents automatic downgrade to JSON Object for image/video/audio input, which would remove Schema constraints. Do not silently fall back to JSON Object in this profile.

Persist non-secret enabled model configuration in PostgreSQL. Parent API key stays in protected server configuration, restricted to the selected model.

U authorizes temporary-key requests. Return one consistent model/configuration snapshot with the temporary key and provider expiration; Android caches and invokes Bailian directly. Request TTL 600 seconds. Provider keys cannot be promised to revoke immediately with service credentials; stop new issuance after invalidation. Do not add another provider or inference in Go.

No Account allowlist, invitation or project usage/billing quota. Provider-enforced constraints still apply; cost and resource-abuse exposure remain with the operator.

## 9. Wire conventions and operation inventory

HTTP prefix is /v1; WSS subprotocol is pin-glide.v1. Required operations follow.

IDs are opaque strings; callers do not infer identity or authority from them. Message IDs must be randomly generated and never reused for another SMS. Timestamps use RFC 3339 UTC. Binary values use padded Base64. Credentials use Authorization: Bearer for HTTP/WSS where applicable, never query strings.

| Operation | Credential | Purpose |
| --- | --- | --- |
| POST /v1/auth/wechat | fresh WeChat code | Create/restore Account and Sender registration; issue M/U/F |
| POST /v1/auth/qq | fresh QQ code | Create/restore Account and Sender registration; issue M/U/F |
| POST /v1/auth/refresh | F in body | Rotate Sender bundle using operationId and attempt |
| POST /v1/auth/logout | valid Sender self access credential or F; M for switch_account | Revoke current Sender credentials; Account switching also removes own Pairings |
| GET /v1/self/receivers | M | Own Receiver list, aliases and presence |
| PATCH /v1/self/pairings/{id} | M | Rename own association alias |
| DELETE /v1/self/pairings/{id} | M | Remove own association only |
| POST /v1/devices/{id}/revoke | M plus fresh WeChat or QQ proof | Account-owner security revocation |
| POST /v1/pairing-sessions | none for new/recovery; R for add_sender | Create a mode-fixed restricted 5-minute context |
| POST /v1/pairing-sessions/{id}/authorize | M; fresh WeChat or QQ proof for recovery | Confirm owner; reserve capacity only for a new distinct Pairing |
| POST /v1/pairing-sessions/{id}/complete | M or B for its side | Report mode-specific key confirmation; activate Pairing only for new/add |
| DELETE /v1/pairing-sessions/{id} | owning M or B | Cancel and release reservation |
| POST /v1/pairing-sessions/{id}/receiver-token | B, new_receiver or recover_receiver only | Redeem current issuance grant, operation/attempt protected; add_sender is forbidden |
| POST /v1/model/credentials | U | Temporary provider key plus configuration |
| POST /v1/messages | U | Per-Receiver synchronous 207 results |
| GET /v1/messages/{messageId} | U | Own metadata only, if retained; no ciphertext/history |
| GET /health | none | Sanitized service/database health |

### HTTP request and success contracts: v1

The following definitions form the shared v1 contract. Sender ID and Receiver ID are the deviceId of their respective registration roles. AppID and Account ownership are configured/derived by the service, not supplied as caller-controlled authority.

Common response objects:

- CredentialBundle: accountId, senderId, userSession {token, expiresAt}, uploader {token, expiresAt}, refresh {token, expiresAt}. All fields are required. Expirations are RFC 3339 UTC and reflect the actual issued credentials.
- ReceiverAssociation: receiverId, pairingId, alias, presence (online/offline), stateChangedAt. alias is the current Sender's own string, initially empty if unset. Only active own associations appear in the list; pending reservations are represented by their Pairing Sessions.
- PairingSessionView: sessionId, mode, receiverId, state, expiresAt, senderConfirmed, receiverConfirmed, senderId once claimed, and pairingId when a new/replacement Pairing has been allocated. It contains no raw credential, QR PSK, client private key or message key.
- DeliveryView: receiverId, pairingId, status, createdAt, completedAt when terminal, and error for terminal failures. status may be processing only in a metadata query; synchronous upload 207 results remain terminal. Input index belongs to an upload response, not a Delivery's persistent identity.
- ModelCredential: credential {apiKey, expiresAt} and configuration {provider, region, baseUrl, inference}. provider is bailian, region is cn-beijing, and inference contains the model, enable_thinking and response_format fields defined in section 8. Return one consistent configuration snapshot and the provider's actual expiry, converted to RFC 3339 UTC.

| Operation | Request | Success |
| --- | --- | --- |
| POST /v1/auth/wechat | wechatCode; optional saved senderId | 200 CredentialBundle; omit senderId to request a new registration |
| POST /v1/auth/qq | qqCode; optional saved senderId | 200 CredentialBundle; omit senderId to request a new registration |
| POST /v1/auth/refresh | refreshToken, operationId, attempt (0 or 1) | 200 CredentialBundle plus the accepted operationId and attempt |
| POST /v1/auth/logout | Optional mode: logout or switch_account, default logout; refreshToken when using F | 204; logout revokes the current Sender's credentials and preserves Pairings; switch_account also removes its Pairings and requires M management authority |
| GET /v1/self/receivers | No body | 200 {items: ReceiverAssociation[]} |
| PATCH /v1/self/pairings/{id} | alias string | 200 ReceiverAssociation with the updated own alias |
| DELETE /v1/self/pairings/{id} | No body | 204; remove only the current Sender's association |
| POST /v1/devices/{id}/revoke | exactly one of wechatCode or qqCode for fresh same-Account owner verification | 204; apply the device-specific security-revocation lifecycle |
| POST /v1/pairing-sessions | mode; existing receiverId for add_sender/recover_receiver | 201 {session: PairingSessionView, pairingToken, pairingTokenExpiresAt}; new_receiver allocates receiverId, add_sender also authenticates existing R |
| POST /v1/pairing-sessions/{id}/authorize | Optional own alias; recovery also requires exactly one of wechatCode or qqCode | 200 PairingSessionView after owner/capacity checks; repeated confirmation by the same Sender returns the same authorized session |
| POST /v1/pairing-sessions/{id}/complete | keyConfirmation: confirmed; recovery may instead report not_available; Windows recovery also reports retainedPairingIds | 200 PairingSessionView; derive the reporting side from M/B and apply mode-specific completion |
| DELETE /v1/pairing-sessions/{id} | No body | 204 before activation; cancel unfinished work and release its reservation |
| POST /v1/pairing-sessions/{id}/receiver-token | operationId, attempt (0 or 1) | 200 {receiverId, receiverToken}; long-lived R has no expiry field |
| POST /v1/model/credentials | No body | 200 ModelCredential |
| POST /v1/messages | messageId and items as defined below | 207 with requestId and one terminal result per input index |
| GET /v1/messages/{messageId} | No body | 200 {messageId, deliveries: DeliveryView[]} for the authenticated Sender's retained admitted Deliveries |
| GET /health | No body | 200 {status: ok}; unavailable service/database returns sanitized 503 |

Send credential-bearing success responses with Cache-Control: no-store. Tokens are returned only to the authorized holder. Ordinary logout accepts a valid Sender self access credential in Authorization or its F in the refreshToken body field, including when M/U have expired; it does not require issuing new access tokens first. Supply exactly one credential source. switch_account requires M because it removes associations. If switching cleanup cannot be authorized or confirmed, the service returns the standard error rather than asserting remote cleanup. Refresh/logout response-loss and local generation handling remain as approved in section 3.

Unknown or purged message metadata returns 404 resource_not_found. The metadata query contains admitted Delivery records rather than an upload-request history; rejected-only input without an admitted Delivery does not create a queryable Delivery. A request's HTTP 207 response still reports every rejected input. Live processing is observable as processing in this GET only; unavailable storage or an unconfirmable result returns the standard 503. Resolve orphaned work as interrupted under the existing lifecycle.

### Pairing session state: v1

State is awaiting_authorization, awaiting_confirmation, credential_ready, credential_issued, completed, canceled, expired or superseded. The absolute session/B lifetime remains five minutes.

Creation enters awaiting_authorization. The first authorized Sender claim fixes ownership and enters awaiting_confirmation; another Sender receives pairing_session_claimed. Allocate a pending Pairing ID for new/add operations so both clients can store a provisional key under the same identity. The service accepts opaque relay traffic only for the session's authenticated participants and valid state.

Both new/add clients must report confirmed after saving the derived key and checking peer key confirmation. Repeating a successful side's completion while its credential remains valid is idempotent. Both reports permit activation; add_sender then enters completed without R issuance, while new_receiver enters credential_ready. For a replacement, commit the new Pairing ID only after both reports and while its expected old association is still current; otherwise return operation_superseded and preserve the current association. Clients select the new key/association only after activation is reported.

For recover_receiver, fresh owner approval is mandatory. Confirm an existing key where available; not_available is allowed only in this mode. Windows reports the existing Pairing IDs for which it still holds local keys; the service checks that the set belongs to that Receiver. Preserve those associations and remove associations whose keys were lost, without creating a Pairing or consuming new capacity. The approving Sender reports confirmation only for a key it actually holds. Recovery reaches credential_ready after the required authenticated reports.

Redemption enters credential_issued. A valid R presentation proves possession, consumes the grant and enters completed; the restricted B channel closes. Higher-attempt replacement of an unclaimed R, superseding recovery grants and atomic upgrade/revocation retain section 4's rules. An add_sender session cannot enter either credential state.

Cancel/expiry before activation releases the reservation without activating a Pairing. Once a Pairing has activated, session cancellation does not undo it: return pairing_session_closed and use ordinary Pairing removal when needed. B expiry stops credential redemption and closes the restricted channel; an already-active Pairing remains active, with owner-authorized credential recovery available if first R was not saved. Explicit Receiver recovery/revocation supersedes obsolete grants.

WSS disconnect alone does not extend the session or undo committed state. An authorized reconnect receives the current PairingSessionView. Opaque handshake ciphertext is live relay traffic, with no replay/history API. Clients use their local handshake state or start a new session if they cannot continue; the service does not recover client cryptographic state.

A result-query GET reads only the authenticated Sender's retained metadata. It does not require the Pairing to remain active merely to read that Sender's own history. It does not authorize new forwarding or override upload validation.

### Upload and response shape

~~~json
{
  "messageId": "random-message-id",
  "items": [
    {
      "receiverId": "receiver-id",
      "pairingId": "pairing-id",
      "cryptoVersion": 1,
      "nonce": "AAAAAAAAAAAAAAAA",
      "ciphertext": "AQIDBAUGBwgJCgsMDQ4PEBESExQ="
    }
  ]
}
~~~

~~~json
{
  "messageId": "random-message-id",
  "requestId": "req-example",
  "results": [
    {
      "index": 0,
      "receiverId": "receiver-id",
      "status": "received"
    }
  ]
}
~~~

These are structural examples with synthetic binary bytes, not working encrypted test vectors. Never copy example nonces into production encryption.

Failures carry a stable machine error code, not raw provider/credential data. Do not equate HTTP 207 or received with complete popup/clipboard acceptance. Do not return another Account's existence/details in target errors.

### Business-error descriptions

- openapi.yaml defines the error response schema, stable code enums, applicable HTTP status and business meaning for every operation, including authentication/expiry, ownership, Pairing/session state, capacity, credential issuance and provider/storage failures.
- Its upload contract distinguishes whole-request failure from per-entry 207 outcomes. Define each status/rejection code, input-index correlation, duplicate-result semantics and whether delivery may already have occurred. Include mixed valid/invalid/offline examples and delivery_status_unavailable; HTTP 207 alone is not success for every item.
- asyncapi.yaml defines handshake HTTP rejections, including HTTP 409 receiver_already_connected before 101, plus WSS business-error/control schemas, directions, correlation, malformed/unknown-message handling and close-code meanings. Distinguish arrival Receipt from decryption or display success.
- Both descriptions use consistent names and meanings for shared outcomes and describe the effect on existing work/connections. They expose service facts; client message-retry decisions remain client-owned.

### Standard business-error responses

The user approved this standard in [Standard business-error responses](https://github.com/BuddyLabsCo/pin-glide-service/issues/2#issuecomment-6080063942).

Use [RFC 9457 Problem Details](https://www.rfc-editor.org/rfc/rfc9457.html) for whole-request HTTP failures, consistent with [Huma's error model](https://huma.rocks/features/response-errors/). Return application/problem+json and the actual 4xx/5xx status. Required fields are type, title, status, detail, code and requestId. type is the primary problem-type identifier, using the URI reference /problems/{code}; code is its stable snake_case short identifier, with a one-to-one mapping. Declare type as format: uri-reference in the custom schema because Huma's default format: uri requires an absolute URI. title is a stable English summary; detail is safe explanatory text that clients do not parse for control flow. status matches the HTTP status. requestId is generated by the service and identifies the operation in service logs. Apply the same error model to service-owned authentication middleware, routing fallbacks and handler errors.

~~~json
{
  "type": "/problems/receiver_limit_reached",
  "title": "Receiver limit reached",
  "status": 409,
  "detail": "A Sender can have at most 5 associated Receivers.",
  "code": "receiver_limit_reached",
  "requestId": "req-example"
}
~~~

Validation errors may add errors, an array of location/message objects. Never echo rejected raw values, credentials, ciphertext, SQL details, stack traces or provider responses. Code meanings are stable; clients tolerate additional response fields and unfamiliar codes while retaining the HTTP/outcome semantics. Unknown codes are not success.

HTTP categories and initial code mapping:

| HTTP status | Code | Meaning |
| --- | --- | --- |
| 400 | invalid_request | Malformed JSON or missing required request body |
| 422 | validation_failed | Whole-request shape/field validation, including invalid path/query/header values |
| 401 | credential_missing, credential_invalid, credential_expired, credential_revoked | Required service credential is absent or unusable |
| 401 | wechat_authorization_invalid, qq_authorization_invalid, refresh_reuse_detected | Invalid provider authorization, or confirmed refresh reuse with the approved family-revocation effect |
| 403 | insufficient_scope, fresh_identity_required | Valid credential lacks operation scope or required fresh owner verification |
| 404 | resource_not_found | Resource does not exist or is not visible to this caller; do not distinguish another Account's resource |
| 409 | receiver_limit_reached, receiver_already_connected | Binding cap or exclusive delivery-connection conflict |
| 409 | device_registration_revoked | Fresh login requested restoration of an owned permanently revoked Sender registration |
| 409 | operation_superseded, pairing_session_claimed, pairing_not_ready, mode_forbids_credential_issue | Operation ordering or authenticated session-state conflict |
| 410 | pairing_session_expired, pairing_session_closed | An authorized caller's pairing context has expired or closed |
| 502 | identity_provider_error, model_provider_error | The service's identity verification or provider-key issuance dependency failed |
| 503 | service_unavailable, model_configuration_unavailable | Service/storage or required model configuration is unavailable |
| 503 | delivery_status_unavailable | A reliable terminal Delivery result cannot be obtained; delivery may already have occurred |
| 500 | internal_error | Unexpected server failure, with safe public detail |

Evaluate credential validity before protected resource state. An expired B therefore yields credential_expired; a valid owning M can receive pairing_session_expired for the same expired session. Normal same-message deduplication returns the retained/live Delivery result, rather than a duplicate-message conflict.

For POST /v1/messages, whole-request validation covers the envelope (for example, missing messageId or an invalid items array). Once entries can be enumerated, item validation belongs to the per-entry 207 result, including malformed items. An invalid entry must not trigger whole-request 422 and suppress valid entries. Every result carries its original input index; include receiverId only if it can be read as a valid identifier.

Use the shared BusinessError object {code, detail, errors?} inside failed 207 items and WSS error messages. Keep HTTP-only Problem Details fields at the HTTP-error level. A 207 response uses application/json, carries requestId once at the top level, and has one result per input entry. received omits error; other terminal statuses require it. Item codes are receiver_not_authorized, duplicate_receiver, invalid_delivery_item, unsupported_crypto_version, offline, send_failed, ack_timeout and interrupted. Unknown, foreign, unpaired and revoked targets all use receiver_not_authorized externally; retain the specific internal rejection reason without exposing resource existence.

~~~json
{
  "messageId": "message-example",
  "requestId": "req-example",
  "results": [
    {"index": 0, "receiverId": "receiver-a", "status": "received"},
    {
      "index": 1,
      "receiverId": "receiver-b",
      "status": "rejected",
      "error": {
        "code": "receiver_not_authorized",
        "detail": "Receiver is not eligible for this Sender."
      }
    }
  ]
}
~~~

rejected and offline describe this invocation's lack of forwarding to that target; they do not deny an earlier Delivery of the same Message. send_failed, ack_timeout, interrupted and delivery_status_unavailable may follow arrival. A transport failure or internal_error is not evidence of non-delivery. No retry instruction/flag is part of this error schema.

Before HTTP 101, WSS handshake rejection uses the same HTTP Problem Details response. After 101, recoverable service business errors use the application error message below, with requestId and available session/message correlation. Keep the connection open for recoverable business errors. Local Receiver decryption/validation failure remains Receiver-owned and does not suppress arrival Receipt or create a service decryption-error result.

~~~json
{
  "version": 1,
  "type": "error",
  "requestId": "req-example",
  "sessionId": "session-example",
  "error": {
    "code": "pairing_not_ready",
    "detail": "Both clients have not completed key confirmation."
  }
}
~~~

Unrecoverable WSS errors close with standard meanings: 1002 for protocol/version/type violations, 1007 for invalid text encoding, 1008 for credential/policy revocation, 1011 for unexpected server errors, and 1001 for server shutdown. A best-effort application error may precede Close; Close reason contains only a short machine code, never secret data, and fits 123 UTF-8 bytes. Do not require that an application error arrives when a connection is already unusable. Document each operation's applicable codes and effects in both YAML descriptions, including the same schema extensions emitted at runtime.

### WSS inventory

| Endpoint | Credential | Exchanges |
| --- | --- | --- |
| /v1/receiver/events | R | Service delivery/control; Receiver receipt; native Ping/Pong |
| /v1/pairing-sessions/{id}/events | owning M or receiver-side B | Opaque handshake, state changes, credential-ready notification |

R does not open an additional pairing WSS: use B or multiplex session traffic without a second R-authenticated delivery connection.

Delivery contains version, senderId, receiverId, pairingId, messageId, cryptoVersion, nonce and ciphertext. Receipt contains version, senderId and messageId; Receiver identity comes from the authenticated connection. Accept receipts only for that Receiver's registered in-flight Delivery; ignore harmless duplicates/late receipts without changing terminal state.

Pairing messages contain sessionId, side and opaque payload; derive allowed side from authentication. Control types include pairing_state, credential_ready, pairing_removed and device_revoked. No control response leaks R to Android or any client encryption key to the service.

### Message definitions and directions: v1

Every application WSS message contains version: 1 and type. Native Ping/Pong and Close retain their WebSocket control-frame formats.

| Channel and direction | Type | Required payload fields beyond version/type |
| --- | --- | --- |
| Delivery, service to Receiver | delivery | senderId, receiverId, pairingId, messageId, cryptoVersion, nonce, ciphertext |
| Delivery, Receiver to service | receipt | senderId, messageId; derive Receiver from the authenticated connection |
| Delivery, service to Receiver | pairing_removed | senderId, pairingId for the removed association |
| Delivery, service to Receiver | device_revoked | receiverId; followed by policy Close |
| Pairing, either client to service | pairing_relay | sessionId, payload as a padded-Base64 opaque byte string |
| Pairing, service to peer client | pairing_relay | sessionId, side (sender/receiver), payload unchanged |
| Pairing, service to either participant | pairing_state | session: PairingSessionView; send an initial snapshot after authorized subscription and later state changes |
| Pairing, service to Windows B only | credential_ready | sessionId, receiverId, expiresAt |
| Either channel, service to client | error | requestId, error: BusinessError; add sessionId or senderId/messageId when readable and applicable |

The caller does not select relay side. M must belong to the session's confirming Sender; B must belong to its Receiver side. Use B for the separate pairing channel, preserving the single R delivery connection. Receipt correlation is (authenticated Receiver, senderId, messageId); ignore duplicate/late Receipts without changing terminal results.

Client-only QR and handshake bytes are defined for interoperability:

- QR is UTF-8 JSON containing version, cryptoVersion, sessionId, receiverId, recipientPublicKey and psk. Both versions are 1; public key and PSK use the sizes/encoding from section 5. Credentials and service URLs are absent. Clients use their configured service origin and check that service session metadata matches the QR references.
- A pairing_relay payload is padded Base64 of UTF-8 JSON. HPKE setup uses kind: hpke_init, cryptoVersion: 1, enc and ciphertext; sealed plaintext contains type: pairing_init and challengeA.
- Confirmation relay uses kind: key_confirmation, cryptoVersion: 1, nonce and ciphertext. Decrypted confirmation fields are the type/role/challenge fields defined in section 5. Challenges are padded Base64 of exactly 32 bytes. The service forwards these bytes without verifying client key material or decrypted challenges.
- Delivery plaintext uses type: verification_message, smsText, otp and optional receivedAt as defined in section 5. A null model OTP never becomes a delivery payload.

### v1 compatibility

Published release descriptions remain immutable. v1 permits compatible optional-field additions; recipients tolerate unknown optional fields. Existing required fields, field meanings, credentials and Receipt milestone remain stable. Because unknown application message types are protocol errors, a new server-initiated type requires a new negotiated protocol version rather than being sent to an old v1 client. Incompatible fields/semantics likewise require a new HTTP/WSS protocol version and separately versioned release contracts. Advertise supported versions and changes in release notes; client generation remains client-owned.

Document before release: successful/failed handshake HTTP responses, message directions, correlation, unknown version/type handling, malformed-frame behavior and close codes. Standard closes: 1000 normal, 1001 server going away, 1002 protocol error, 1007 invalid text encoding, 1008 credential/policy revocation and 1011 unexpected server error. Rejected second connections use HTTP 409 before upgrade.

## 10. Persistence, cleanup and privacy

Persist Accounts, device/credential-digest records, Pairing authorizations/aliases, model configuration, pairing/control metadata and processing/status/rejection metadata as needed. No SMS/OTP plaintext, client encryption key or message ciphertext in PostgreSQL or application logs.

The architecture discussion clarified PostgreSQL retention into exactly three table-level policies: no scheduled deletion for configuration/durable authorization and release ledgers/version history; seven days for message-delivery transactions and temporary authentication/pairing control data; thirty days for service runtime logs, schema apply/check audits and pg_cron run history. Business operations may physically delete configuration where the functional lifecycle permits it; preserve minimal permanently revoked Device Registration identity for the agreed restoration behavior. A table must not mix no-cleanup/seven-day/thirty-day records. See [the retention decision](https://github.com/BuddyLabsCo/pin-glide-service/issues/3#issuecomment-6095218706) and [the architecture ticket](https://github.com/BuddyLabsCo/pin-glide-service/issues/3) for classification and discussion.

Use pg_cron in PostgreSQL, daily 03:00 Asia/Shanghai, to delete Delivery metadata older than 7 days. Deduplication depends on row existence, with no independent age filter. Normal row lifetime is about 7-8 days; missed/failed cleanup can extend it. No Go cleanup fallback.

Cleanup has its own configuration table defining target schema/table, retention days and date-column name, with one policy per table. The pg_cron task reads the entries and uses dynamic SQL to directly delete each target's rows by its configured time filter. Date columns are selected/validated during implementation/configuration rather than fixed for every table in architecture planning. Their semantics must preserve valid credentials, sliding families and ordering guards; long-lived R and activated Pairings remain separate from temporary session data. Historical identifiers must not let configuration deletion cascade into retained transaction/log data.

Interrupted cleanup reruns the filters on the next scheduled day without a progress checkpoint or new recovery mechanism. Date-column coverage is checked when implementing/configuring each target.

These retention periods apply only to PostgreSQL. Docker/container logs are operator-managed. Backups follow the Helm manual retention/recovery workflow; unfinished recovery dependencies must not be removed. Logical deletion is not immediate physical erasure from backups or storage.

Memory ciphertext release follows Delivery/request terminal processing, never the daily metadata schedule. Keep raw credentials and WeChat, QQ or Bailian secret-bearing requests/responses out of logs and published examples.

## 11. Acceptance and validation

The test-message flow uses a simulated SMS but real Bailian extraction, local validation, per-Receiver encryption and normal routing. Verify Windows popup, copy button and shortcut by pasting the result.

### Performance-testing boundary

Repository performance tests, load/benchmark plans, latency sampling procedures and P95/per-sample release gates are out of scope. The source's conditional end-to-end 5-second target remains product context, not a repository performance-test requirement. The agreed service deadlines/heartbeat and functional timeout behavior remain part of the contract. Arrival ACK does not replace functional popup/copy/shortcut verification.

### Required scenarios

- Fresh WeChat and QQ login, same-Account restore, provider namespace separation, Account switch and cross-Account rejection.
- Fresh re-login atomically replacing the old family without losing Pairings; security-revoked Sender ID refusing restoration; delayed old refresh unable to restore/revoke the new family.
- M/U/F scope separation, refresh with expired access, family replay/overlap/order, response loss and logout races.
- First Receiver credential, add-Sender non-interference, owner recovery, superseded grants and pending-upgrade revocation.
- Concurrent sixth binding blocked; same Receiver/re-pair not double-counted; expired reservations free capacity.
- Failed/canceled/expired replacement preserving the old Pairing; successful dual confirmation replacing it atomically; stale concurrent replacement rejected without changing the current association.
- Crypto two-client test vectors, public-point rejection, challenge/role/type checks, message decryption failure still arrival-ACKed.
- Valid/invalid/offline mixed 207; concurrent duplicate uploads atomically sharing one work/result; changed legality before a new upload; revocation after validation still attempts admitted work.
- Write timeout, missing/late ACK, HTTP disconnect, restart interruption and storage-failure uncertainty without resending.
- Duplicate WSS 409, stale-close callback, heartbeat timeout, reconnect without backlog and accurate presence.
- Provider expiry/failure, non-thinking JSON Schema settings, malformed/null/invalid OTP output and local original-SMS validation; no upload on failed extraction.
- Privacy checks across database/logs/examples; metadata pg_cron retention and no payload recovery after restart.

No current test results are claimed.

## 12. Engineering and release baseline

Preserve S13-S16: Go/Huma HTTP, authenticated Go WSS, PostgreSQL/Jet, pinned Jet/psqldef generation/tooling, domain-owned business logic, transactions and Jet stores with separate HTTP/WSS handling, versioned Docker images and own Linux Docker Compose deployment. Keep development/production projects, databases/volumes, endpoints and secrets independent.

Generate target-schema Jet artifacts against disposable local PostgreSQL. Declarative psqldef schemas and ordered structural/data upgrade scripts remain distinct. Never run tests/generation against production data.

First installation prepares configuration/images/database/schema/initial data and starts healthy service. Updates stop application writes, back up PostgreSQL, apply/check ordered schema/data evolution, restart and verify health. Reject unsupported downgrade. pg_cron extension/job permissions, scheduling during upgrades and restore cleanup must be covered in downstream persistence planning.

Export complete self-contained generated/protocol/openapi.yaml and generated/protocol/asyncapi.yaml with matching embedded schemas and the explicit business-error descriptions above. Validate syntax/references, operation/route/message coverage, authentication/directions, errors/receipts, behavior and supported-client compatibility. Attach both validated assets to the GitHub Release matching service source commit/tag and images; publish only when both are complete. Published assets are stable; corrections use a new release. Clients retrieve pinned releases and own their generation/conversion.

Keep the future static homepage owned/hosted here, but its content, UI and implementation timing are outside initial forwarding functionality.

## 13. Traceability and adoption

| Baseline | Specification |
| --- | --- |
| S01 | Account mapping/open registration, WeChat and QQ credential issuance |
| S02 | One-time QR, client-only key establishment, delayed Pairing activation |
| S03 | Opaque scopes, joint M/U/F renewal, long-lived Receiver credential |
| S04 | Own aliases/Pairing removal, presence, Account security revocation |
| S05 | Sender-selected legal targets and max-five Pairing authorization |
| S06 | Bailian model/configuration, temporary credentials and direct Android inference |
| S07 | v1 HTTP/WSS/crypto framing; explicit replay/AAD scope reduction |
| S08 | Parallel 207, arrival ACK, bounded once-only routing and interruption |
| S09 | Memory-only ciphertext and terminal/request release |
| S10 | Persistent metadata/privacy and daily seven-day purge |
| S11 | Normal simulated-message/copy/shortcut verification |
| S12 | Conditional 5-second product target retained as context; repository performance tests and gates excluded by the user |
| S13-S16 | Preserved engineering, schema/deployment and release-contract baseline |
| S17 | Future homepage ownership only |

Shared-boundary checkpoint: the current client maps contain no approved conflicting decision. Service-authority context pointers in [Android functional requirements](https://github.com/BuddyLabsCo/pin-glide-android/issues/2#issuecomment-6080124694) and [Windows functional requirements](https://github.com/BuddyLabsCo/pin-glide-windows/issues/2#issuecomment-6080125168) record the approved boundaries, supplemented by [Sender lifecycle and re-pairing](https://github.com/BuddyLabsCo/pin-glide-service/issues/2#issuecomment-6080579614). Their local HITL tickets remain open; the old source diagrams do not override the linked decisions.

Adoption: the user approved the complete v1 route/schema/crypto framing in [the functional resolution](https://github.com/BuddyLabsCo/pin-glide-service/issues/2#issuecomment-6080960446). Client context pointers carry that shared contract into their local HITL discussions. Provider support for the selected settings is documented; actual account permissions, temporary-key/Workspace-endpoint use and inference remain integration checks. Local client behavior remains with its owners; completed client specifications and runtime interoperability are not claimed.

S18-S20 code structure, static enforcement and persistence-evolution policy belong to the separate architecture child and its required root-level code-architecture.md. Do not implement or silently decide those here.
