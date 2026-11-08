//go:build integration

package pairing_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// Account reads accept current R without proving connection possession. A
// redemption retry must still work, and the replaced R must lose read access.
func TestAccountLookupPreservesReceiverGrantAndPairings(t *testing.T) {
	e := start(t)
	owner := e.qqSender(t, "owner", "owner")
	foreign := e.qqSender(t, "foreign", "foreign")
	ready, restricted := e.readySession(t, owner)
	status, issued, payload := e.redeem(t, ready.SessionID, restricted, "account-grant", 0)
	if status != http.StatusOK {
		t.Fatalf("redeem = %d: %s", status, payload)
	}
	assertAccount(t, e, issued.ReceiverToken, owner.AccountID, nil, nil)
	assertAccount(t, e, owner.Uploader.Token, owner.AccountID, nil, nil)
	assertAccount(t, e, foreign.Uploader.Token, foreign.AccountID, nil, nil)
	assertAccountRejected(t, e, restricted)

	// Even the oldest stored display data remains readable while QQ is
	// unavailable; no timestamp can make a read refresh or erase the cache.
	e.exec(t, `UPDATE pin_glide.identity_accounts SET created_at = now() - interval '8 days',
		nickname = 'Old name', avatar_url = 'https://qq.example/old' WHERE id = $1`, owner.AccountID)
	e.qq.Fail("owner", identity.ErrIdentityProvider)
	assertAccount(t, e, issued.ReceiverToken, owner.AccountID, "Old name", "https://qq.example/old")
	assertAccount(t, e, owner.Uploader.Token, owner.AccountID, "Old name", "https://qq.example/old")

	status, replacement, payload := e.redeem(t, ready.SessionID, restricted, "account-grant", 1)
	if status != http.StatusOK {
		t.Fatalf("lookup consumed issuance grant: %d: %s", status, payload)
	}
	assertAccountRejected(t, e, issued.ReceiverToken)
	assertAccount(t, e, replacement.ReceiverToken, owner.AccountID, "Old name", "https://qq.example/old")
	status, payload = e.post(t, "/v1/messages", `{"messageId":"receiver-cannot-upload","items":[]}`, replacement.ReceiverToken)
	if status != http.StatusUnauthorized || decodeProblem(t, payload).Code != "credential_invalid" {
		t.Fatalf("R upload = %d: %s, want 401 credential_invalid", status, payload)
	}

	e.qq.Accept("update", identity.Identity{AppID: "qq-app", OpenID: "owner",
		Profile: &identity.DisplayProfile{Nickname: "New name", AvatarURL: "https://qq.example/new"}})
	status, payload = e.post(t, "/v1/auth/qq", `{"deviceName":"Test Sender","qqCode":"update","senderId":"`+owner.SenderID+`"}`, "")
	if status != http.StatusOK {
		t.Fatalf("restore = %d: %s", status, payload)
	}
	var restored bundle
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.AccountID != owner.AccountID || restored.SenderID != owner.SenderID {
		t.Fatal("restore replaced device identity")
	}
	assertAccountRejected(t, e, owner.Uploader.Token)
	assertAccount(t, e, replacement.ReceiverToken, owner.AccountID, "New name", "https://qq.example/new")
	assertAccount(t, e, restored.Uploader.Token, owner.AccountID, "New name", "https://qq.example/new")
	status, associations, payload := e.listOwnReceivers(t, restored.UserSession.Token)
	if status != http.StatusOK || len(associations) != 1 || associations[0].PairingID != ready.PairingID || associations[0].Presence != "offline" {
		t.Fatalf("lookup/login changed Pairing or presence: %d %+v: %s", status, associations, payload)
	}
	status, payload = e.qqRevokeDevice(t, ready.ReceiverID, restored.UserSession.Token, "revoke", "owner")
	if status != http.StatusNoContent {
		t.Fatalf("revoke = %d: %s", status, payload)
	}
	assertAccountRejected(t, e, replacement.ReceiverToken)
}

func assertAccount(t *testing.T, e *environment, credential, accountID string, nickname, avatar any) {
	t.Helper()
	// Extra query parameters cannot select a foreign Account.
	status, payload := e.request(t, http.MethodGet, "/v1/self/account?accountId=foreign", "", credential)
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || len(body) != 3 || body["accountId"] != accountID || body["nickname"] != nickname || body["avatarUrl"] != avatar {
		t.Fatalf("own Account = %d: %s, want %s/%v/%v", status, payload, accountID, nickname, avatar)
	}
}

func assertAccountRejected(t *testing.T, e *environment, credential string) {
	t.Helper()
	status, payload := e.request(t, http.MethodGet, "/v1/self/account", "", credential)
	if status != http.StatusUnauthorized || decodeProblem(t, payload).Code != "credential_invalid" {
		t.Fatalf("rejected Account credential = %d: %s", status, payload)
	}
}
