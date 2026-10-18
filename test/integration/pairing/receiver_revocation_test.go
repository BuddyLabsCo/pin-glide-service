//go:build integration

// Owner security revocation of a Receiver is exercised the way an Account
// owner experiences it: HTTP requests and real WSS delivery connections against
// the composed service, a disposable real PostgreSQL holding the shipped
// schema, and the faked WeChat boundary.
package pairing_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/coder/websocket"
)

// revokeDevice posts one owner security revocation with the Sender management
// credential that authorizes it and one fresh WeChat proof for the Account that
// owns the named device.
func (e *environment) revokeDevice(t *testing.T, deviceID, credential, code, openID string) (int, []byte) {
	t.Helper()

	e.provider.Accept(code, identity.Identity{AppID: "wx-app", OpenID: openID})
	return e.post(t, "/v1/devices/"+deviceID+"/revoke", `{"wechatCode":"`+code+`"}`, credential)
}

// revokeDeviceOK requires one accepted owner revocation: an empty 204 that
// hands no credential to the caller.
func (e *environment) revokeDeviceOK(t *testing.T, deviceID, credential, code, openID string) {
	t.Helper()

	status, payload := e.revokeDevice(t, deviceID, credential, code, openID)
	if status != http.StatusNoContent {
		t.Fatalf("revocation of %s = %d, want 204: %s", deviceID, status, payload)
	}
	if len(payload) != 0 {
		t.Fatalf("revocation of %s returned a body: %s", deviceID, payload)
	}
}

// acceptedRevocationErrors reads the accepted owner revocation contract, so a
// refusal this test expects is the one that contract defines for the operation
// rather than a second copy of it.
func acceptedRevocationErrors(t *testing.T) map[string][]string {
	t.Helper()

	contents, err := os.ReadFile("../../fixtures/contract/http/device_revocation_v1.json")
	if err != nil {
		t.Fatalf("read accepted revocation contract: %v", err)
	}
	var fixture struct {
		Errors map[string][]string `json:"errors"`
	}
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatalf("decode accepted revocation contract: %v", err)
	}
	return fixture.Errors
}

// An Account owner revokes one Receiver for every Sender that shares it: the
// registration, its credential, its grants and all of its Pairings go with one
// commit, its committed connection reads device_revoked and then closes with
// the policy meaning, and an unrelated Receiver of the same Account keeps its
// credential, its Pairings and its live connection.
func TestOwnerRevocationClosesOneReceiverForEverySender(t *testing.T) {
	environment := start(t)
	// Two installations of one Account, so one Receiver serves both of them.
	first := environment.sender(t, "code-revoke-first", "openid-revoke-owner")
	second := environment.sender(t, "code-revoke-second", "openid-revoke-owner")

	revoked, issued := environment.paired(t, first, "Phone")
	shared := environment.attach(t, second, revoked.ReceiverID, issued.ReceiverToken)
	kept, keptIssued := environment.paired(t, first, "Tablet")
	environment.attach(t, second, kept.ReceiverID, keptIssued.ReceiverToken)

	// Both Receivers hold their committed delivery connection.
	held := receiverConnected(t, environment, issued.ReceiverToken)
	keptConnection := receiverConnected(t, environment, keptIssued.ReceiverToken)
	receiverPresence(t, environment.service, revoked.ReceiverID, true, 5*time.Second)
	receiverPresence(t, environment.service, kept.ReceiverID, true, 5*time.Second)

	// The second installation's management credential together with fresh proof
	// of the Account that owns both is what makes this an owner action.
	environment.revokeDeviceOK(t, revoked.ReceiverID, second.UserSession.Token,
		"code-revoke-owner", "openid-revoke-owner")

	// The committed connection reads the adopted control, and the policy Close
	// the revocation committed follows it.
	var control protocol.DeviceRevoked
	readFrame(t, held, &control)
	if control.Version != protocol.WSSVersion || control.Type != "device_revoked" || control.ReceiverID != revoked.ReceiverID {
		t.Errorf("revocation control = %+v, want version 1 device_revoked of %s", control, revoked.ReceiverID)
	}
	closing, cancelClosing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClosing()
	if _, _, err := held.Read(closing); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("the revoked connection closed with %v, want %v",
			websocket.CloseStatus(err), websocket.StatusPolicyViolation)
	}
	receiverPresence(t, environment.service, revoked.ReceiverID, false, 5*time.Second)

	// Every Pairing that forwarded to the revoked Receiver is gone, whichever
	// Sender owned it, and its registration and credential authorize nothing.
	if state := environment.associationOf(t, revoked.PairingID).state; state != "removed" {
		t.Errorf("the revoked Receiver's association state = %q, want removed", state)
	}
	if state := environment.associationOf(t, shared.PairingID).state; state != "removed" {
		t.Errorf("the sharing Sender's association state = %q, want removed", state)
	}
	if closed := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_registrations
		WHERE id = $1 AND state = 'revoked' AND revoked_at IS NOT NULL`, revoked.ReceiverID); closed != 1 {
		t.Errorf("the revoked registration is not stored as permanently closed: %d rows", closed)
	}
	if authorized := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_credentials
		WHERE receiver_id = $1 AND revoked_at IS NULL`, revoked.ReceiverID); authorized != 0 {
		t.Errorf("the revoked Receiver still holds %d authorizing credentials", authorized)
	}
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")

	// Repeating the revocation reports the permanent closure instead of closing
	// the registration a second time.
	status, payload := environment.revokeDevice(t, revoked.ReceiverID, first.UserSession.Token,
		"code-revoke-closed", "openid-revoke-owner")
	if status != http.StatusConflict {
		t.Fatalf("repeated revocation = %d, want 409: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "device_registration_revoked" {
		t.Errorf("repeated revocation code = %q, want device_registration_revoked", code)
	}

	// The Account's Senders keep their own authority and see only the Receiver
	// that survived.
	status, payload = environment.request(t, http.MethodGet, "/v1/self/receivers", "", first.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("own Receiver list = %d, want 200: %s", status, payload)
	}
	var listed struct {
		Items []struct {
			PairingID string `json:"pairingId"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &listed); err != nil {
		t.Fatalf("decode own Receiver list: %v: %s", err, payload)
	}
	if len(listed.Items) != 1 || listed.Items[0].PairingID != kept.PairingID {
		t.Errorf("own Receiver list = %+v, want only the surviving association %s", listed.Items, kept.PairingID)
	}

	// The unrelated Receiver keeps its Pairing, its credential and its live
	// connection, and the closed connection's own cleanup leaves that alone.
	if state := environment.associationOf(t, kept.PairingID).state; state != "active" {
		t.Errorf("the unrelated Receiver's association state = %q, want active", state)
	}
	if authorized, _ := environment.currentCredential(t, kept.ReceiverID); authorized != 1 {
		t.Errorf("the unrelated Receiver holds %d current credentials, want 1", authorized)
	}
	if state := receiverPresence(t, environment.service, revoked.ReceiverID, false, time.Second); state.Online {
		t.Errorf("the closed connection published %+v, want the Receiver offline", state)
	}
	receiverPresence(t, environment.service, kept.ReceiverID, true, time.Second)
	readTimeout, cancelRead := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelRead()
	if _, _, err := keptConnection.Read(readTimeout); err == nil {
		t.Error("the revocation sent the unrelated Receiver's connection traffic")
	} else if websocket.CloseStatus(err) != -1 {
		t.Errorf("the revocation closed the unrelated Receiver's connection: %v", err)
	}
}

// Owner revocation of a Receiver needs both halves of its authority: a
// management credential of the Account that owns the Receiver, and fresh WeChat
// proof of that same Account. Every unusable half is refused without exposing
// another Account's Receiver and without changing what the Receiver can still
// do.
func TestReceiverRevocationNeedsAnOwnedReceiverAndFreshSameAccountProof(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-revoke-owned", "openid-revoke-owned")
	foreign := environment.sender(t, "code-revoke-foreign", "openid-revoke-foreign")
	held, issued := environment.paired(t, owner, "Phone")

	environment.provider.Fail("code-revoke-invalid", identity.ErrWeChatAuthorizationInvalid)
	environment.provider.Fail("code-revoke-broken", identity.ErrIdentityProvider)
	accepted := acceptedRevocationErrors(t)

	for _, refused := range []struct {
		name       string
		deviceID   string
		credential string
		code       string
		openID     string
		wantStatus int
		wantCode   string
	}{
		{
			name: "no management credential", deviceID: held.ReceiverID,
			code: "code-revoke-owned", openID: "openid-revoke-owned",
			wantStatus: http.StatusUnauthorized, wantCode: "credential_missing",
		},
		{
			name: "unknown management credential", deviceID: held.ReceiverID,
			credential: "credential-that-was-never-issued", code: "code-revoke-owned", openID: "openid-revoke-owned",
			wantStatus: http.StatusUnauthorized, wantCode: "credential_invalid",
		},
		{
			name: "upload credential instead of management", deviceID: held.ReceiverID,
			credential: owner.Uploader.Token, code: "code-revoke-owned", openID: "openid-revoke-owned",
			wantStatus: http.StatusUnauthorized, wantCode: "credential_invalid",
		},
		{
			name: "no proof", deviceID: held.ReceiverID, credential: owner.UserSession.Token,
			wantStatus: http.StatusUnprocessableEntity, wantCode: "validation_failed",
		},
		{
			name: "proof of another Account", deviceID: held.ReceiverID, credential: owner.UserSession.Token,
			code: "code-revoke-foreign", openID: "openid-revoke-foreign",
			wantStatus: http.StatusForbidden, wantCode: "fresh_identity_required",
		},
		{
			name: "proof the provider rejects", deviceID: held.ReceiverID, credential: owner.UserSession.Token,
			code: "code-revoke-invalid", openID: "openid-revoke-owned",
			wantStatus: http.StatusUnauthorized, wantCode: "wechat_authorization_invalid",
		},
		{
			name: "provider failure", deviceID: held.ReceiverID, credential: owner.UserSession.Token,
			code: "code-revoke-broken", openID: "openid-revoke-owned",
			wantStatus: http.StatusBadGateway, wantCode: "identity_provider_error",
		},
		{
			name: "unknown Receiver", deviceID: "receiver-that-never-existed",
			credential: owner.UserSession.Token, code: "code-revoke-owned", openID: "openid-revoke-owned",
			wantStatus: http.StatusNotFound, wantCode: "resource_not_found",
		},
		{
			name: "another Account's Receiver", deviceID: held.ReceiverID,
			credential: foreign.UserSession.Token, code: "code-revoke-foreign", openID: "openid-revoke-foreign",
			wantStatus: http.StatusNotFound, wantCode: "resource_not_found",
		},
	} {
		t.Run(refused.name, func(t *testing.T) {
			if !slices.Contains(accepted[strconv.Itoa(refused.wantStatus)], refused.wantCode) {
				t.Fatalf("the accepted contract defines no %s for status %d",
					refused.wantCode, refused.wantStatus)
			}
			body := `{}`
			if refused.code != "" {
				body = `{"wechatCode":"` + refused.code + `"}`
			}
			environment.provider.Accept(refused.code, identity.Identity{AppID: "wx-app", OpenID: refused.openID})
			status, payload := environment.post(t, "/v1/devices/"+refused.deviceID+"/revoke", body, refused.credential)
			if status != refused.wantStatus {
				t.Fatalf("revocation = %d, want %d: %s", status, refused.wantStatus, payload)
			}
			if code := decodeProblem(t, payload).Code; code != refused.wantCode {
				t.Errorf("revocation code = %q, want %q", code, refused.wantCode)
			}
		})
	}

	// Nothing a refused attempt reached changed: the Receiver keeps its Pairing
	// and its current credential, and that credential still opens its delivery
	// connection.
	if state := environment.associationOf(t, held.PairingID).state; state != "active" {
		t.Errorf("the Receiver's association state = %q, want active", state)
	}
	if authorized, _ := environment.currentCredential(t, held.ReceiverID); authorized != 1 {
		t.Errorf("the Receiver holds %d current credentials, want 1", authorized)
	}
	if closed := environment.count(t, `SELECT count(*) FROM pin_glide.identity_receiver_registrations
		WHERE id = $1 AND state = 'active' AND revoked_at IS NULL`, held.ReceiverID); closed != 1 {
		t.Errorf("a refused attempt closed the Receiver registration: %d active rows", closed)
	}
	receiverConnected(t, environment, issued.ReceiverToken)
}

// Revocation invalidates every grant one Receiver could still redeem: a grant
// whose credential was already handed out, one still waiting for its first
// redemption, and an owner-approved recovery. Each closes with the registration
// in one commit, so no late redemption, retransmission or key report can hand
// out access to a Receiver that is gone.
func TestReceiverRevocationInvalidatesEveryOutstandingGrant(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-revoke-grants", "openid-revoke-grants")

	// A grant that handed out its credential without it ever proving possession.
	issued, issuedToken := environment.claimNewReceiver(t, owner, "Issued")
	environment.activate(t, issued.SessionID, owner.UserSession.Token, issuedToken)
	status, redeemed, payload := environment.redeem(t, issued.SessionID, issuedToken, "op-revoke-issued", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}
	if state := environment.sessionState(t, issued.SessionID); state != "credential_issued" {
		t.Fatalf("session state after redemption = %q, want credential_issued", state)
	}

	// A grant that was never redeemed, and the recovery one owner approved.
	pending, pendingToken := environment.readySession(t, owner)
	recovered, recoveredToken := environment.paired(t, owner, "Recovered")
	recovery, restricted := environment.createRecovery(t, recovered.ReceiverID)
	environment.claimRecovery(t, recovery.SessionID, "code-revoke-grants-approval", "openid-revoke-grants", owner)

	environment.revokeDeviceOK(t, issued.ReceiverID, owner.UserSession.Token,
		"code-revoke-grants-issued", "openid-revoke-grants")
	environment.revokeDeviceOK(t, pending.ReceiverID, owner.UserSession.Token,
		"code-revoke-grants-pending", "openid-revoke-grants")
	environment.revokeDeviceOK(t, recovered.ReceiverID, owner.UserSession.Token,
		"code-revoke-grants-recovery", "openid-revoke-grants")

	// The credential the closed Receiver had already handed out is revoked with
	// the generation it belonged to, and the retransmission of its issuance
	// cannot mint another one.
	if status, _, payload := environment.redeem(t, issued.SessionID, issuedToken, "op-revoke-issued", 1); status != http.StatusGone {
		t.Errorf("retransmission after revocation = %d, want 410: %s", status, payload)
	}
	receiverRejected(t, environment, redeemed.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	if state := environment.sessionState(t, issued.SessionID); state != "canceled" {
		t.Errorf("the issued grant's session state = %q, want canceled", state)
	}
	if authorized, _ := environment.currentCredential(t, issued.ReceiverID); authorized != 0 {
		t.Errorf("the revoked Receiver holds %d current credentials, want none", authorized)
	}

	// The grant that was never redeemed cannot be redeemed after the closure.
	if status, _, payload := environment.redeem(t, pending.SessionID, pendingToken, "op-revoke-pending", 0); status != http.StatusGone {
		t.Errorf("redemption of a revoked Receiver's grant = %d, want 410: %s", status, payload)
	}
	if state := environment.sessionState(t, pending.SessionID); state != "canceled" {
		t.Errorf("the pending grant's session state = %q, want canceled", state)
	}

	// The approved recovery of the revoked Receiver cannot report, so the
	// credential it would have replaced is never replaced.
	if status, _, payload := environment.reportRecovery(t, recovery.SessionID, restricted, retainedReport()); status != http.StatusGone {
		t.Errorf("Receiver report of a revoked recovery = %d, want 410: %s", status, payload)
	}
	if status, _, payload := environment.reportRecovery(t, recovery.SessionID, owner.UserSession.Token,
		`{"keyConfirmation":"confirmed"}`); status != http.StatusGone {
		t.Errorf("Sender report of a revoked recovery = %d, want 410: %s", status, payload)
	}
	if state := environment.sessionState(t, recovery.SessionID); state != "canceled" {
		t.Errorf("the recovery grant's session state = %q, want canceled", state)
	}
	if state := environment.associationOf(t, recovered.PairingID).state; state != "removed" {
		t.Errorf("the recovered Receiver's association state = %q, want removed", state)
	}
	// The credential the Receiver held when the owner revoked it is revoked with
	// the registration it belonged to.
	receiverRejected(t, environment, recoveredToken.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
}

// A closure and a generation change are decided by commit order, never by
// request order: a redemption that commits first hands out a credential the
// closure revokes with its generation, a redemption that loses is refused on
// the session the closure canceled before it can issue anything, and a delivery
// upgrade that commits after the closure is ended instead of left holding the
// connection the closed registration used to own.
func TestReceiverRevocationSerializesWithRedemptionAndUpgrade(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-revoke-race", "openid-revoke-race")

	// A grant that is one redemption away from issuing races the closure that
	// would cancel it.
	redeeming, redeemingToken := environment.readySession(t, owner)
	type redemption struct {
		status int
		issued issuedCredential
		body   []byte
	}
	redeemed := make(chan redemption, 1)
	go func() {
		status, issued, payload := environment.redeem(t, redeeming.SessionID, redeemingToken, "op-revoke-race", 0)
		redeemed <- redemption{status: status, issued: issued, body: payload}
	}()
	environment.revokeDeviceOK(t, redeeming.ReceiverID, owner.UserSession.Token,
		"code-revoke-race-pending", "openid-revoke-race")
	race := <-redeemed
	switch race.status {
	case http.StatusOK:
		// The redemption won, so the credential it handed out belongs to the
		// generation the closure revoked.
		receiverRejected(t, environment, race.issued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	case http.StatusGone:
		if code := decodeProblem(t, race.body).Code; code != "pairing_session_closed" {
			t.Errorf("losing redemption code = %q, want pairing_session_closed", code)
		}
	default:
		t.Errorf("racing redemption = %d, want 200 or 410: %s", race.status, race.body)
	}
	if state := environment.sessionState(t, redeeming.SessionID); state != "canceled" {
		t.Errorf("the raced grant's session state = %q, want canceled", state)
	}
	if authorized, _ := environment.currentCredential(t, redeeming.ReceiverID); authorized != 0 {
		t.Errorf("the raced Receiver holds %d current credentials, want none", authorized)
	}

	// The same closure races the recovery that would replace an existing
	// credential: whichever commits first, the replacement the closure did not
	// take with the registration is refused for the same reason.
	recovered, recoveredIssued := environment.paired(t, owner, "Recovered")
	recovery, restricted := environment.approvedRecovery(t, owner, recovered.ReceiverID,
		"code-revoke-race-approval", "openid-revoke-race", recovered.PairingID)
	replaced := make(chan redemption, 1)
	go func() {
		status, issued, payload := environment.redeem(t, recovery.SessionID, restricted, "op-revoke-race-recovery", 0)
		replaced <- redemption{status: status, issued: issued, body: payload}
	}()
	environment.revokeDeviceOK(t, recovered.ReceiverID, owner.UserSession.Token,
		"code-revoke-race-recovery", "openid-revoke-race")
	race = <-replaced
	switch race.status {
	case http.StatusOK:
		receiverRejected(t, environment, race.issued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	case http.StatusGone:
		if code := decodeProblem(t, race.body).Code; code != "pairing_session_closed" {
			t.Errorf("losing recovery redemption code = %q, want pairing_session_closed", code)
		}
	default:
		t.Errorf("racing recovery redemption = %d, want 200 or 410: %s", race.status, race.body)
	}
	if state := environment.sessionState(t, recovery.SessionID); state != "canceled" {
		t.Errorf("the raced recovery's session state = %q, want canceled", state)
	}
	if authorized, _ := environment.currentCredential(t, recovered.ReceiverID); authorized != 0 {
		t.Errorf("the raced Receiver holds %d current credentials, want none", authorized)
	}
	receiverRejected(t, environment, recoveredIssued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")

	// The closure also races the final 101 commit of a credential it is about
	// to revoke, which is the same boundary an upgrade takes.
	association, issued := environment.paired(t, owner, "Phone")
	type upgrade struct {
		socket *websocket.Conn
		status int
		body   []byte
	}
	upgraded := make(chan upgrade, 1)
	go func() {
		socket, response, err := dialReceiver(t, environment, issued.ReceiverToken)
		result := upgrade{socket: socket}
		if err != nil && response != nil {
			result.status = response.StatusCode
			if response.Body != nil {
				result.body, _ = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
		}
		upgraded <- result
	}()
	environment.revokeDeviceOK(t, association.ReceiverID, owner.UserSession.Token,
		"code-revoke-race-upgrade", "openid-revoke-race")
	if committed := <-upgraded; committed.socket != nil {
		// The upgrade committed before the closure, so the service ends the
		// connection the revoked registration was holding.
		requireClose(t, committed.socket, websocket.StatusPolicyViolation)
	} else {
		if committed.status != http.StatusUnauthorized {
			t.Errorf("an upgrade racing the closure = %d, want 401: %s", committed.status, committed.body)
		}
		if code := decodeProblem(t, committed.body).Code; code != "credential_invalid" {
			t.Errorf("an upgrade racing the closure = %q, want credential_invalid", code)
		}
	}
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	if state := environment.associationOf(t, association.PairingID).state; state != "removed" {
		t.Errorf("the raced Receiver's association state = %q, want removed", state)
	}
}
