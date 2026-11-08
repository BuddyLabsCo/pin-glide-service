//go:build integration

package release_test

import (
	"database/sql"
	"testing"
)

// Nullable display columns need only Schema Sync: an upgrade must preserve
// Account IDs, registrations, Pairings and credentials without a Data Script.
func TestAccountProfileSchemaUpgradePreservesIdentity(t *testing.T) {
	_, work, bin, pool, dsn := releaseDatabase(t)
	process := releaseProcess{work: work, bin: bin, dsn: dsn, version: "1.0.1", initial: "true", retry: "false"}
	writeWorkFile(t, work, "db/migrations/order.txt", "")
	process.migrate(t, true, "complete")
	exec := func(query string) { t.Helper(); _, err := pool.Exec(query); must(t, err) }
	exec(`ALTER TABLE pin_glide.identity_accounts DROP COLUMN nickname, DROP COLUMN avatar_url`)
	exec(`INSERT INTO pin_glide.identity_accounts (id, provider, app_id, open_id)
		VALUES ('legacy-account', 'qq', 'app', 'openid')`)
	exec(`INSERT INTO pin_glide.identity_sender_registrations (id, account_id, device_name)
		VALUES ('legacy-sender', 'legacy-account', 'Legacy Sender')`)
	exec(`INSERT INTO pin_glide.identity_receiver_registrations (id, account_id, state)
		VALUES ('legacy-receiver', 'legacy-account', 'active')`)
	exec(`INSERT INTO pin_glide.identity_credentials (sender_id, family_id, scope, token_digest, expires_at)
		VALUES ('legacy-sender', 'legacy-family', 'uploader', 'legacy-U', now() + interval '1 hour')`)
	exec(`INSERT INTO pin_glide.identity_receiver_credentials (receiver_id, generation, token_digest)
		VALUES ('legacy-receiver', 1, 'legacy-R')`)
	exec(`INSERT INTO pin_glide.pairing_pairings (id, sender_id, receiver_id, state, activated_at)
		VALUES ('legacy-pairing', 'legacy-sender', 'legacy-receiver', 'active', now())`)
	process.initial, process.version = "false", "1.0.2"
	process.migrate(t, true, "complete")
	var nickname, avatar sql.NullString
	must(t, pool.QueryRow(`SELECT nickname, avatar_url FROM pin_glide.identity_accounts
		WHERE id = 'legacy-account' AND provider = 'qq' AND app_id = 'app' AND open_id = 'openid'`).Scan(&nickname, &avatar))
	if nickname.Valid || avatar.Valid {
		t.Fatal("upgrade invented display data")
	}
	var count int
	must(t, pool.QueryRow(`SELECT count(*) FROM pin_glide.pairing_pairings p
		JOIN pin_glide.identity_sender_registrations s ON s.id = p.sender_id
		JOIN pin_glide.identity_receiver_registrations r ON r.id = p.receiver_id
		JOIN pin_glide.identity_credentials u ON u.sender_id = s.id
		JOIN pin_glide.identity_receiver_credentials c ON c.receiver_id = r.id
		WHERE p.id = 'legacy-pairing' AND p.state = 'active' AND s.state = 'active' AND r.state = 'active'
		AND s.account_id = 'legacy-account' AND r.account_id = 'legacy-account'
		AND u.token_digest = 'legacy-U' AND u.revoked_at IS NULL AND u.expires_at > now()
		AND c.token_digest = 'legacy-R' AND c.revoked_at IS NULL AND c.generation = 1`).Scan(&count))
	if count != 1 {
		t.Fatal("upgrade changed existing identities, Pairing or credentials")
	}
	exec(`UPDATE pin_glide.identity_accounts SET nickname = 'Cached name', avatar_url = 'https://qq.example/avatar' WHERE id = 'legacy-account'`)
	process.version = "1.0.3"
	process.migrate(t, true, "complete")
	must(t, pool.QueryRow(`SELECT nickname, avatar_url FROM pin_glide.identity_accounts WHERE id = 'legacy-account'`).Scan(&nickname, &avatar))
	if nickname.String != "Cached name" || avatar.String != "https://qq.example/avatar" {
		t.Fatal("Schema Sync erased cached display data")
	}
}
