//go:build integration

package pairing_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

func (e *environment) qqSender(t *testing.T, code, openID string) bundle {
	t.Helper()

	e.qq.Accept(code, identity.Identity{AppID: "qq-app", OpenID: openID})
	status, payload := e.post(t, "/v1/auth/qq", `{"qqCode":"`+code+`"}`, "")
	if status != http.StatusOK {
		t.Fatalf("QQ login status = %d: %s", status, payload)
	}
	var issued bundle
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode QQ credential bundle: %v: %s", err, payload)
	}
	return issued
}

func (e *environment) qqRevokeDevice(t *testing.T, deviceID, credential, code, openID string) (int, []byte) {
	t.Helper()

	e.qq.Accept(code, identity.Identity{AppID: "qq-app", OpenID: openID})
	return e.post(t, "/v1/devices/"+deviceID+"/revoke",
		`{"qqCode":"`+code+`"}`, credential)
}

// QQ owner proof drives recovery and both permanent device-revocation paths
// through the same ownership and credential lifecycle as WeChat.
func TestQQProofSupportsRecoveryAndPermanentDeviceRevocation(t *testing.T) {
	environment := start(t)
	owner := environment.qqSender(t, "qq-owner", "openid-qq-owner")
	second := environment.qqSender(t, "qq-second", "openid-qq-owner")
	if owner.AccountID != second.AccountID || owner.SenderID == second.SenderID {
		t.Fatalf("QQ installations = %+v and %+v, want one Account and two Senders", owner, second)
	}

	association, _ := environment.paired(t, owner, "Phone")
	recovery, restricted := environment.createRecovery(t, association.ReceiverID)
	environment.qq.Accept("qq-recovery", identity.Identity{AppID: "qq-app", OpenID: "openid-qq-owner"})
	status, claimed, payload := environment.claim(t, recovery.SessionID,
		`{"qqCode":"qq-recovery"}`, second.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("QQ recovery claim status = %d: %s", status, payload)
	}
	if claimed.SenderID != second.SenderID {
		t.Errorf("QQ recovery claimed Sender %q, want %q", claimed.SenderID, second.SenderID)
	}
	if status, _, payload := environment.reportRecovery(t, recovery.SessionID,
		second.UserSession.Token, `{"keyConfirmation":"confirmed"}`); status != http.StatusOK {
		t.Fatalf("QQ Sender recovery report status = %d: %s", status, payload)
	}
	if status, ready, payload := environment.reportRecovery(t, recovery.SessionID,
		restricted, retainedReport(association.PairingID)); status != http.StatusOK {
		t.Fatalf("QQ Receiver recovery report status = %d: %s", status, payload)
	} else if ready.State != "credential_ready" {
		t.Errorf("QQ recovery state = %q, want credential_ready", ready.State)
	}
	status, replacement, payload := environment.redeem(t, recovery.SessionID,
		restricted, "op-qq-recovery", 0)
	if status != http.StatusOK || replacement.ReceiverID != association.ReceiverID {
		t.Fatalf("QQ recovery redemption = %d %+v: %s", status, replacement, payload)
	}

	environment.qq.Accept("qq-revoke-owner-sender", identity.Identity{AppID: "qq-app", OpenID: "openid-qq-owner"})
	status, payload = environment.post(t, "/v1/devices/"+owner.SenderID+"/revoke",
		`{"qqCode":"qq-revoke-owner-sender"}`, second.UserSession.Token)
	if status != http.StatusNoContent {
		t.Fatalf("QQ Sender revocation status = %d, want 204: %s", status, payload)
	}
	request, err := http.NewRequest(http.MethodGet, environment.service.BaseURL+"/v1/self/receivers", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+owner.UserSession.Token)
	response, err := environment.service.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if status := response.StatusCode; status != http.StatusUnauthorized {
		t.Errorf("QQ-revoked Sender credential status = %d, want 401", status)
	}

	status, payload = environment.qqRevokeDevice(t, association.ReceiverID,
		second.UserSession.Token, "qq-revoke-receiver", "openid-qq-owner")
	if status != http.StatusNoContent {
		t.Fatalf("QQ Receiver revocation status = %d, want 204: %s", status, payload)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_registrations
		WHERE id = $1 AND state = 'revoked'`, association.ReceiverID); count != 1 {
		t.Errorf("QQ revocation left %d revoked Receiver registrations", count)
	}
}

// QQ proof names the same Account the management credential belongs to; a
// foreign Account, rejected code or conflicting provider fields never reaches
// a protected device.
func TestQQProofsRejectForeignAndConflictingIdentities(t *testing.T) {
	environment := start(t)
	owner := environment.qqSender(t, "qq-proof-owner", "openid-qq-proof-owner")
	foreign := environment.qqSender(t, "qq-proof-foreign", "openid-qq-proof-foreign")
	association, _ := environment.paired(t, owner, "Phone")

	recovery, _ := environment.createRecovery(t, association.ReceiverID)
	environment.qq.Accept("qq-foreign-proof", identity.Identity{AppID: "qq-app", OpenID: "openid-qq-proof-foreign"})
	status, _, payload := environment.claim(t, recovery.SessionID,
		`{"qqCode":"qq-foreign-proof"}`, foreign.UserSession.Token)
	if status != http.StatusNotFound {
		t.Errorf("foreign QQ owner claim = %d, want 404: %s", status, payload)
	}

	environment.qq.Accept("qq-owner-proof", identity.Identity{AppID: "qq-app", OpenID: "openid-qq-proof-owner"})
	status, _, payload = environment.claim(t, recovery.SessionID,
		`{"qqCode":"qq-owner-proof"}`, foreign.UserSession.Token)
	if status != http.StatusForbidden {
		t.Errorf("QQ proof of another Account = %d, want 403: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "fresh_identity_required" {
		t.Errorf("foreign QQ proof code = %q, want fresh_identity_required", code)
	}

	status, _, payload = environment.claim(t, recovery.SessionID,
		`{"wechatCode":"wechat-proof","qqCode":"qq-owner-proof"}`, owner.UserSession.Token)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("conflicting recovery proofs = %d, want 422: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "validation_failed" {
		t.Errorf("conflicting proof code = %q, want validation_failed", code)
	}

	environment.qq.Fail("qq-invalid-proof", identity.ErrQQAuthorizationInvalid)
	status, _, payload = environment.claim(t, recovery.SessionID,
		`{"qqCode":"qq-invalid-proof"}`, owner.UserSession.Token)
	if status != http.StatusUnauthorized {
		t.Errorf("invalid QQ recovery proof = %d, want 401: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "qq_authorization_invalid" {
		t.Errorf("invalid QQ proof code = %q, want qq_authorization_invalid", code)
	}

	environment.qq.Accept("qq-foreign-revoke", identity.Identity{AppID: "qq-app", OpenID: "openid-qq-proof-foreign"})
	status, payload = environment.post(t, "/v1/devices/"+association.ReceiverID+"/revoke",
		`{"qqCode":"qq-foreign-revoke"}`, owner.UserSession.Token)
	if status != http.StatusForbidden {
		t.Errorf("revocation with another Account's QQ proof = %d, want 403: %s", status, payload)
	}
	status, payload = environment.post(t, "/v1/devices/"+association.ReceiverID+"/revoke",
		`{"wechatCode":"wechat-proof","qqCode":"qq-foreign-revoke"}`, owner.UserSession.Token)
	if status != http.StatusUnprocessableEntity {
		t.Errorf("conflicting revocation proofs = %d, want 422: %s", status, payload)
	}

	if state := environment.associationOf(t, association.PairingID).state; state != "active" {
		t.Errorf("a rejected QQ proof changed the Pairing state to %q", state)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_registrations
		WHERE id = $1 AND state = 'active'`, association.ReceiverID); count != 1 {
		t.Errorf("a rejected QQ proof changed the Receiver registration")
	}
}
