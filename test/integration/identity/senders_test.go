//go:build integration

package identity_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// Invalid names must fail before either provider can create or restore a Sender.
// The existing login tests only validate authorization codes and Sender IDs.
func TestSenderLoginRejectsInvalidDeviceNames(t *testing.T) {
	environment := startLogin(t)
	environment.qq.Accept("name-code", identity.Identity{AppID: "app", OpenID: "owner"})
	environment.provider.Accept("name-code", identity.Identity{AppID: "app", OpenID: "owner"})
	for _, provider := range []string{"qq", "wechat"} {
		for _, name := range []string{"", `,"deviceName":null`, `,"deviceName":12`, `,"deviceName":false`, `,"deviceName":{}`, `,"deviceName":[]`, `,"deviceName":""`, `,"deviceName":" \t\n\u3000"`, fmt.Sprintf(`,"deviceName":%q`, strings.Repeat("界", 65))} {
			body := fmt.Sprintf(`{%q:"name-code"%s}`, provider+"Code", name)
			status, payload, _ := post(t, environment, "/v1/auth/"+provider, body)
			var failure problem
			if err := json.Unmarshal(payload, &failure); err != nil {
				t.Fatal(err)
			}
			if status != http.StatusUnprocessableEntity || failure.Code != "validation_failed" {
				t.Errorf("%s %s: status=%d code=%s, want 422 validation_failed", provider, name, status, failure.Code)
			}
		}
	}
	if count := environment.count(t, `SELECT (SELECT count(*) FROM pin_glide.identity_sender_registrations) + (SELECT count(*) FROM pin_glide.identity_accounts) + (SELECT count(*) FROM pin_glide.identity_credentials)`); count != 0 {
		t.Fatalf("invalid names created %d identity rows", count)
	}
}

func (e *loginEnvironment) senderInventory(t *testing.T, token string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, e.service.BaseURL+"/v1/self/senders?accountId=ignored", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := e.service.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, payload
}

// The complete Account inventory includes logged-out registrations but excludes
// revoked and foreign ones; restoration changes the intended name, not identity.
func TestSenderInventoryAndNameRestoration(t *testing.T) {
	environment := startLogin(t)
	environment.qq.Accept("owner", identity.Identity{AppID: "app", OpenID: "owner"})
	environment.qq.Accept("foreign", identity.Identity{AppID: "app", OpenID: "foreign"})
	environment.qq.Accept("other-app", identity.Identity{AppID: "other-app", OpenID: "owner"})
	environment.provider.Accept("owner", identity.Identity{AppID: "app", OpenID: "owner"})
	name := strings.Repeat("📱", 64)
	actor := environment.qqLoginOK(t, fmt.Sprintf(`{"qqCode":"owner","deviceName":%q}`, " \u3000"+name+"\t"))
	loggedOut := environment.qqLoginOK(t, `{"qqCode":"owner","deviceName":"duplicate"}`)
	target := environment.qqLoginOK(t, `{"qqCode":"owner","deviceName":"duplicate"}`)
	foreign := environment.qqLoginOK(t, `{"qqCode":"foreign","deviceName":"Foreign"}`)
	environment.qqLoginOK(t, `{"qqCode":"other-app","deviceName":"Other App"}`)
	environment.loginOK(t, `{"wechatCode":"owner","deviceName":"WeChat"}`)
	logoutOK(t, environment, loggedOut.UserSession.Token, "")
	environment.seedPairing(t, "named-receiver", "named-pairing", actor.SenderID)
	// The two older rows tie, forcing the Sender ID tie-breaker independently of insertion order.
	environment.exec(t, `UPDATE pin_glide.identity_sender_registrations SET created_at='2026-01-01T00:00:00Z' WHERE account_id=$1`, actor.AccountID)
	environment.exec(t, `UPDATE pin_glide.identity_sender_registrations SET created_at='2026-01-02T00:00:00Z' WHERE id=$1`, actor.SenderID)
	status, payload := environment.senderInventory(t, actor.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("inventory status=%d: %v", status, payload)
	}
	assertSenderItems(t, payload, actor.SenderID, name, []string{loggedOut.SenderID, target.SenderID})
	// A foreign proof, an unknown ID and a revoked ID cannot rename the original.
	environment.qqLoginFails(t, fmt.Sprintf(`{"qqCode":"foreign","senderId":%q,"deviceName":"stolen"}`, actor.SenderID), http.StatusNotFound, "resource_not_found")
	environment.qqLoginFails(t, `{"qqCode":"owner","senderId":"unknown","deviceName":"unknown"}`, http.StatusNotFound, "resource_not_found")
	environment.qqLoginFails(t, fmt.Sprintf(`{"qqCode":"owner","senderId":%q,"deviceName":" \u3000"}`, actor.SenderID), http.StatusUnprocessableEntity, "validation_failed")
	status, payload = environment.senderInventory(t, actor.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("invalid restoration changed M: %d %v", status, payload)
	}
	assertSenderItems(t, payload, actor.SenderID, name, []string{loggedOut.SenderID, target.SenderID})
	restored := environment.qqLoginOK(t, fmt.Sprintf(`{"qqCode":"owner","senderId":%q,"deviceName":"  Renamed 界  "}`, actor.SenderID))
	if restored.SenderID != actor.SenderID || restored.AccountID != actor.AccountID {
		t.Fatal("restoration changed ownership or ID")
	}
	if environment.associationState(t, "named-pairing") != "pending" {
		t.Fatal("restoration changed Pairing")
	}
	status, payload = environment.senderInventory(t, actor.UserSession.Token)
	if status != http.StatusUnauthorized {
		t.Fatalf("replaced M status=%d: %v", status, payload)
	}
	status, payload = environment.senderInventory(t, restored.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("restored inventory status=%d: %v", status, payload)
	}
	assertSenderItems(t, payload, actor.SenderID, "Renamed 界", []string{loggedOut.SenderID, target.SenderID})
	revokeOK(t, environment, target.SenderID, restored.UserSession.Token, `{"qqCode":"owner"}`)
	environment.qqLoginFails(t, fmt.Sprintf(`{"qqCode":"owner","senderId":%q,"deviceName":"resurrected"}`, target.SenderID), http.StatusConflict, "device_registration_revoked")
	status, payload = environment.senderInventory(t, restored.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("post-revocation status=%d: %v", status, payload)
	}
	assertSenderItems(t, payload, actor.SenderID, "Renamed 界", []string{loggedOut.SenderID})
	status, payload = environment.senderInventory(t, foreign.UserSession.Token)
	items, _ := payload["items"].([]any)
	if status != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["senderId"] != foreign.SenderID {
		t.Fatalf("foreign Account inventory: %d %v", status, payload)
	}
}

func assertSenderItems(t *testing.T, payload map[string]any, actorID, name string, olderIDs []string) {
	t.Helper()
	items, ok := payload["items"].([]any)
	if !ok || len(payload) != 1 || len(items) != len(olderIDs)+1 {
		t.Fatalf("inventory=%v", payload)
	}
	slices.Sort(olderIDs)
	wantIDs := append([]string{actorID}, olderIDs...)
	for i, value := range items {
		item := value.(map[string]any)
		wantName := "duplicate"
		wantTime := "2026-01-01T00:00:00Z"
		if i == 0 {
			wantName = name
			wantTime = "2026-01-02T00:00:00Z"
		}
		_, hasRevokedAt := item["revokedAt"]
		if !hasRevokedAt || len(item) != 5 || item["senderId"] != wantIDs[i] || item["deviceName"] != wantName || item["state"] != "active" || item["revokedAt"] != nil || item["createdAt"] != wantTime {
			t.Errorf("item %d=%v", i, item)
		}
	}
}

// Inventory uses management authority only, including the existing permanent
// registration-revocation precedence over credential scope and expiry.
func TestSenderInventoryRequiresCurrentManagementCredential(t *testing.T) {
	environment := startLogin(t)
	environment.qq.Accept("owner", identity.Identity{AppID: "app", OpenID: "owner"})
	actor := environment.qqLoginOK(t, `{"qqCode":"owner","deviceName":"Actor"}`)
	status, payload, _ := post(t, environment, "/v1/pairing-sessions", `{"mode":"new_receiver"}`)
	if status != http.StatusCreated {
		t.Fatalf("create Pairing Session: %d %s", status, payload)
	}
	var session struct {
		PairingToken string `json:"pairingToken"`
	}
	if err := json.Unmarshal(payload, &session); err != nil {
		t.Fatal(err)
	}
	environment.exec(t, `INSERT INTO pin_glide.identity_receiver_registrations (id,account_id,state) VALUES ('inventory-receiver',$1,'active')`, actor.AccountID)
	environment.exec(t, `INSERT INTO pin_glide.identity_receiver_credentials (receiver_id,generation,token_digest) VALUES ('inventory-receiver',1,$1)`, fmt.Sprintf("%x", sha256.Sum256([]byte("inventory-receiver-token"))))
	for _, token := range []string{"", "invalid", actor.Uploader.Token, actor.Refresh.Token, session.PairingToken, "inventory-receiver-token"} {
		status, payload := environment.senderInventory(t, token)
		if status != http.StatusUnauthorized {
			t.Errorf("wrong authority status=%d: %v", status, payload)
		}
	}
	environment.exec(t, `UPDATE pin_glide.identity_credentials SET expires_at=now()-interval '1 second' WHERE token_digest=$1`, fmt.Sprintf("%x", sha256.Sum256([]byte(actor.UserSession.Token))))
	status, body := environment.senderInventory(t, actor.UserSession.Token)
	if status != http.StatusUnauthorized || body["code"] != "credential_expired" {
		t.Fatalf("expired M: %d %v", status, body)
	}
	environment.exec(t, `UPDATE pin_glide.identity_sender_registrations SET state='revoked',revoked_at=now() WHERE id=$1`, actor.SenderID)
	for _, token := range []string{actor.UserSession.Token, actor.Uploader.Token, actor.Refresh.Token} {
		status, body = environment.senderInventory(t, token)
		if status != http.StatusConflict || body["code"] != "device_registration_revoked" {
			t.Errorf("revoked actor: %d %v", status, body)
		}
	}
	if err := environment.postgres.Stop(); err != nil {
		t.Fatal(err)
	}
	status, body = environment.senderInventory(t, actor.UserSession.Token)
	if status != http.StatusServiceUnavailable || body["code"] != "service_unavailable" {
		t.Fatalf("storage failure: %d %v", status, body)
	}
}
