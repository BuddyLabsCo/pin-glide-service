//go:build integration

package release_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fresh installs enforce names; upgrades must stop for explicit operator data,
// then preserve every existing registration, Pairing and credential unchanged.
func TestSenderDeviceNameUpgradeRequiresPreparedData(t *testing.T) {
	root, work, bin, pool, dsn := releaseDatabase(t)
	process := releaseProcess{work: work, bin: bin, dsn: dsn, version: "0.1.0", initial: "true", retry: "false"}
	writeWorkFile(t, work, "db/migrations/order.txt", "")
	process.migrate(t, true, "complete")
	execSQL := func(query string) { t.Helper(); _, err := pool.Exec(query); must(t, err) }
	execSQL(`INSERT INTO pin_glide.identity_accounts (id,provider,app_id,open_id) VALUES ('owner','qq','app','owner')`)
	for _, value := range []string{"NULL", "''", "E' \\t'", "U&'\\3000'", "repeat('界',65)"} {
		if _, err := pool.Exec(`INSERT INTO pin_glide.identity_sender_registrations (id,account_id,device_name) VALUES ('invalid','owner',` + value + `)`); err == nil {
			t.Fatalf("fresh schema accepted %s", value)
		}
	}
	for _, whitespace := range "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000" {
		if _, err := pool.Exec(`INSERT INTO pin_glide.identity_sender_registrations (id,account_id,device_name) VALUES ('whitespace','owner',$1)`, string(whitespace)); err == nil {
			t.Fatalf("fresh schema accepted U+%04X as a name", whitespace)
		}
	}
	execSQL(`INSERT INTO pin_glide.identity_sender_registrations (id,account_id,device_name) VALUES ('boundary','owner',repeat('📱',64))`)
	execSQL(`DELETE FROM pin_glide.identity_sender_registrations WHERE id='boundary'`)
	// Reconstruct the released schema, before device names existed.
	execSQL(`ALTER TABLE pin_glide.identity_sender_registrations DROP COLUMN device_name CASCADE`)
	execSQL(`INSERT INTO pin_glide.identity_sender_registrations (id,account_id,state,revoked_at) VALUES ('active','owner','active',NULL),('revoked','owner','revoked',now())`)
	execSQL(`INSERT INTO pin_glide.identity_receiver_registrations (id,account_id,state) VALUES ('receiver','owner','active')`)
	execSQL(`INSERT INTO pin_glide.identity_credentials (sender_id,family_id,scope,token_digest,expires_at) VALUES ('active','family','user_session','digest',now()+interval '1 day')`)
	execSQL(`INSERT INTO pin_glide.identity_receiver_credentials (receiver_id,generation,token_digest) VALUES ('receiver',1,'receiver-digest')`)
	execSQL(`INSERT INTO pin_glide.pairing_pairings (id,sender_id,receiver_id,state,activated_at) VALUES ('pairing','active','receiver','active',now())`)
	snapshot := func() string {
		t.Helper()
		var value string
		must(t, pool.QueryRow(`SELECT jsonb_build_object(
   'accounts',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM pin_glide.identity_accounts r),
   'senders',(SELECT jsonb_agg(to_jsonb(r)-'device_name' ORDER BY id) FROM pin_glide.identity_sender_registrations r),
   'receivers',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM pin_glide.identity_receiver_registrations r),
   'credentials',(SELECT jsonb_agg(to_jsonb(r) ORDER BY token_digest) FROM pin_glide.identity_credentials r),
   'receiverCredentials',(SELECT jsonb_agg(to_jsonb(r) ORDER BY receiver_id) FROM pin_glide.identity_receiver_credentials r),
   'pairings',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM pin_glide.pairing_pairings r)
  )::text`).Scan(&value))
		return value
	}
	before := snapshot()
	migrationPath := "db/migrations/pin_glide/issue_109_1_sender_device_names.sql"
	migration, err := os.ReadFile(filepath.Join(root, migrationPath))
	must(t, err)
	writeWorkFile(t, work, migrationPath, string(migration))
	writeWorkFile(t, work, "db/migrations/order.txt", migrationPath+"\n")
	process.initial = "false"
	process.migrate(t, false, "Populate valid Sender device names before upgrading")
	if snapshot() != before {
		t.Fatal("unprepared upgrade mutated existing data")
	}
	execSQL(`ALTER TABLE pin_glide.identity_sender_registrations ADD COLUMN IF NOT EXISTS device_name text`)
	execSQL(`UPDATE pin_glide.identity_sender_registrations SET device_name='Owner supplied active name' WHERE id='active'`)
	process.migrate(t, false, "Populate valid Sender device names before upgrading")
	for _, invalid := range []string{"", "\t\u3000", " untrimmed ", strings.Repeat("界", 65)} {
		_, err := pool.Exec(`UPDATE pin_glide.identity_sender_registrations SET device_name=$1 WHERE id='revoked'`, invalid)
		must(t, err)
		process.migrate(t, false, "Populate valid Sender device names before upgrading")
	}
	execSQL(`UPDATE pin_glide.identity_sender_registrations SET device_name='Owner supplied retired name' WHERE id='revoked'`)
	process.migrate(t, true, "complete")
	process.migrate(t, true, "already processed")
	// A direct rerun is also safe, independently of the migration ledger.
	_, err = pool.Exec(string(migration))
	must(t, err)
	if snapshot() != before {
		t.Fatal("prepared upgrade changed IDs, ownership, Pairings or credentials")
	}
	var name string
	must(t, pool.QueryRow(`SELECT device_name FROM pin_glide.identity_sender_registrations WHERE id='active'`).Scan(&name))
	if name != "Owner supplied active name" {
		t.Fatalf("operator name changed to %q", name)
	}
	if _, err := pool.Exec(`UPDATE pin_glide.identity_sender_registrations SET device_name=NULL WHERE id='active'`); err == nil {
		t.Fatal("upgrade left names nullable")
	}
	if _, err := pool.Exec(`UPDATE pin_glide.identity_sender_registrations SET device_name=U&'\3000' WHERE id='revoked'`); err == nil {
		t.Fatal("upgrade accepted a whitespace-only name")
	}
}
