# Upgrade to required Sender device names

The release containing issue #109 requires updated Sender clients and explicit names for existing registrations. Version v1.0.1 does not contain this feature. Fresh databases enforce the name constraint directly and need no backfill.

## Update clients before deployment

Both `POST /v1/auth/qq` and `POST /v1/auth/wechat` require `deviceName` on creation and restoration. The service trims Unicode whitespace, then accepts 1 to 64 Unicode characters. Missing, null, wrong-type, blank or overlong names return `422 validation_failed` without changing registrations or credentials. Old clients that omit the field are intentionally incompatible; deploy updated clients and pin a service release containing this contract.

Restoration updates the owned active Sender's name while preserving its ID, Account and Pairings, and replaces credentials as before. Names can repeat. They identify devices for display; only Sender IDs select revocation targets. Renewal and fresh owner-proof bodies do not gain this field.

## Prepare an existing deployment

Complete this preparation before running the new release's migration. Supply names for **all** registrations, including permanently revoked ones. Names are operator data: obtain explicit names from the owners or deployment records. Do not infer hardware metadata, assign dummy names, delete registrations or change ownership.

1. Back up the database using the deployment's established snapshot procedure. Keep the previous release available for recovery.
2. Stop the API and every other application writer. Keep PostgreSQL running throughout preparation and migration. The old service can create unnamed Senders, so leave it stopped until the new release passes health checks.
3. Connect to the deployment database with `psql -X -v ON_ERROR_STOP=1`. Add the nullable preparation column and export the complete registration inventory for assigning names:

```sql
ALTER TABLE pin_glide.identity_sender_registrations
ADD COLUMN IF NOT EXISTS device_name text;
SELECT id AS sender_id, account_id, state, created_at, device_name
FROM pin_glide.identity_sender_registrations
ORDER BY account_id, id;
```

Create `sender-names.csv` on the machine running `psql`, with headers `sender_id,device_name` and exactly one explicit, trimmed name per exported Sender ID. Import and apply that mapping in one transaction. The temporary table's primary key rejects duplicate Sender IDs:

```sql
BEGIN;
CREATE TEMP TABLE sender_name_inputs (
    sender_id text PRIMARY KEY,
    device_name text NOT NULL
) ON COMMIT DROP;
\copy sender_name_inputs FROM 'sender-names.csv' CSV HEADER
UPDATE pin_glide.identity_sender_registrations AS sender
SET device_name = input.device_name
FROM sender_name_inputs AS input
WHERE sender.id = input.sender_id;
SELECT id, device_name
FROM pin_glide.identity_sender_registrations
ORDER BY id;
```

Verify every registration has its intended name before issuing `COMMIT`; use `ROLLBACK` to correct an incomplete or incorrect mapping. The only persistent field this update changes is `device_name`. Do not update IDs, Accounts, states, timestamps, Pairings or credentials.

4. Run the new release's normal upgrade procedure while writers remain stopped. Its rerunnable Migration Script checks every name, including Unicode whitespace and the 64-character limit, before Schema Sync enables `NOT NULL` and `chk_sender_device_name`. A missing or invalid name stops the migration with a link to this guide. Correct the explicit mapping and retry; the migration never generates replacement names or discards rows. An empty existing database needs no mapping.
5. Verify the final invariant with this query; both flags must be true:

```sql
SELECT
    bool_and(device_name IS NOT NULL) AS all_names_present,
    bool_and(char_length(device_name) BETWEEN 1 AND 64) AS lengths_valid
FROM pin_glide.identity_sender_registrations;
SELECT convalidated, pg_get_constraintdef(oid)
FROM pg_constraint
WHERE conrelid = 'pin_glide.identity_sender_registrations'::regclass
    AND conname = 'chk_sender_device_name';
```

The named check must exist and be validated; it also rejects names containing only Unicode whitespace. For an empty table, `bool_and` returns null because there are no registrations to verify. Start the updated API, check health, and reload the inventory with a current management credential. Reconcile the returned names against the prepared mapping. Retain the mapping and backup under the deployment's data-handling policy.

The schema addition preserves existing identity, Pairing and credential data. Rolling the API back to v1.0.1 without also planning schema compatibility fails for new Sender creation because the old service supplies no name. Keep writers stopped while choosing a recovery procedure; do not drop data to make an old client or service appear compatible.

## Consume the complete Account inventory

`GET /v1/self/senders` requires current Sender management authority M and derives the Account from that credential. It has no Account selector, paging, search or filters. U, R, F and restricted Pairing credentials cannot authorize it. Listing calls no identity provider and changes no credentials, presence or revocation state.

The `200` response has only `items`. Each item has `senderId`, nonempty `deviceName`, `state: "active"`, `createdAt` in RFC 3339 UTC, and `revokedAt: null`. The complete list includes self and ordinary-logged-out Senders, excludes permanently revoked registrations, and sorts by creation time descending, then Sender ID ascending. Active means unrevoked; it does not describe login or online status. Match the current row using the client's saved Sender ID.

Use the existing `POST /v1/devices/{id}/revoke` with actor M, the target Sender ID, and fresh matching QQ owner proof. After a successful reload for the same Account, absence of the selected target confirms revocation. If it remains, retry revocation. A failed list request establishes neither outcome. Errors remain Problem Details: `401` for unusable M, `409 device_registration_revoked` for a permanently closed actor, and `503 service_unavailable` for temporary storage failure.

The automated fresh-schema and prepared-upgrade tests use disposable PostgreSQL and synthetic identities. They do not establish deployed acceptance or real QQ verification.
