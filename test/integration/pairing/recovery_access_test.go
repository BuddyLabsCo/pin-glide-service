//go:build integration

package pairing_test

import (
	"net/http"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/coder/websocket"
)

// A public Receiver ID authorizes nothing: recovery requires the management
// credential of a Sender that owns the Receiver together with fresh proof of
// that Sender's Account, and every other caller stays unable to confirm which
// Receivers exist.
func TestRecoveryRequiresTheOwningSendersFreshProof(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-owner-proof", "openid-owner-proof")
	foreign := environment.sender(t, "code-foreign-proof", "openid-foreign-proof")

	association, _ := environment.paired(t, owner, "Phone")
	foreignAssociation, _ := environment.paired(t, foreign, "Foreign")

	// A Sender of another Account cannot approve a replacement for a Receiver
	// it does not own: the Receiver stays invisible to it, whichever proof it
	// presents.
	anonymous, restricted := environment.createRecovery(t, association.ReceiverID)
	if restricted == "" {
		t.Fatal("the anonymous recovery session returned no restricted credential")
	}
	environment.provider.Accept("code-foreign-fresh", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-foreign-proof"})
	status, _, payload := environment.claim(t, anonymous.SessionID, `{"wechatCode":"code-foreign-fresh"}`, foreign.UserSession.Token)
	if status != http.StatusNotFound {
		t.Errorf("foreign owner proof = %d, want 404: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "resource_not_found" {
		t.Errorf("foreign owner proof code = %q, want resource_not_found", code)
	}
	environment.provider.Accept("code-owner-proxy", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-owner-proof"})
	status, _, payload = environment.claim(t, anonymous.SessionID, `{"wechatCode":"code-owner-proxy"}`, foreign.UserSession.Token)
	if status != http.StatusForbidden {
		t.Errorf("proof of another Account's owner = %d, want 403: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "fresh_identity_required" {
		t.Errorf("proof of another Account's owner code = %q, want fresh_identity_required", code)
	}

	// The owning Sender proves another Account instead of its own, which is not
	// the fresh same-Account approval a replacement requires.
	environment.provider.Accept("code-other-account", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-foreign-proof"})
	status, _, payload = environment.claim(t, anonymous.SessionID, `{"wechatCode":"code-other-account"}`, owner.UserSession.Token)
	if status != http.StatusForbidden {
		t.Errorf("foreign proof of the owning Sender = %d, want 403: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "fresh_identity_required" {
		t.Errorf("foreign proof code = %q, want fresh_identity_required", code)
	}

	// The owning Sender proves its own Account but still learns nothing about a
	// Receiver that Account does not own.
	notOwned, _ := environment.createRecovery(t, foreignAssociation.ReceiverID)
	environment.provider.Accept("code-owner-fresh", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-owner-proof"})
	status, _, payload = environment.claim(t, notOwned.SessionID, `{"wechatCode":"code-owner-fresh"}`, owner.UserSession.Token)
	if status != http.StatusNotFound {
		t.Errorf("recovery of another Account's Receiver = %d, want 404: %s", status, payload)
	} else if code := decodeProblem(t, payload).Code; code != "resource_not_found" {
		t.Errorf("invisible Receiver code = %q, want resource_not_found", code)
	}

	// An unknown Receiver is invisible at creation as well.
	status, payload = environment.post(t, "/v1/pairing-sessions", `{"mode":"recover_receiver","receiverId":"unknown"}`, "")
	if status != http.StatusNotFound {
		t.Errorf("recovery of an unknown Receiver = %d, want 404: %s", status, payload)
	}
}

// The missing-key report and the retained-key set belong to recovery alone:
// recovery accepts them from the authenticated side that owns that fact, and
// every other combination is a validation failure.
func TestRecoveryReportsAreModeAndSideSpecific(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-reports", "openid-reports")
	other := environment.sender(t, "code-reports-other", "openid-reports-other")

	association, _ := environment.paired(t, owner, "Phone")
	unrelated, _ := environment.paired(t, other, "Other")

	// A new_receiver session reports only a confirmation, from either side.
	claiming, _ := environment.claimNewReceiver(t, owner, "Two")
	if status, _, payload := environment.reportRecovery(t, claiming.SessionID, owner.UserSession.Token,
		`{"keyConfirmation":"not_available"}`); status != http.StatusUnprocessableEntity {
		t.Errorf("missing-key report outside recovery = %d, want 422: %s", status, payload)
	}
	if status, _, payload := environment.reportRecovery(t, claiming.SessionID, owner.UserSession.Token,
		retainedReport(claiming.PairingID)); status != http.StatusUnprocessableEntity {
		t.Errorf("retained keys outside recovery = %d, want 422: %s", status, payload)
	}

	recovery, restricted := environment.createRecovery(t, association.ReceiverID)
	environment.claimRecovery(t, recovery.SessionID, "code-reports-approval", "openid-reports", owner)

	// Only the Receiver side holds the local key store, and it must state a set
	// that agrees with its own report.
	if status, _, payload := environment.reportRecovery(t, recovery.SessionID, owner.UserSession.Token,
		retainedReport(association.PairingID)); status != http.StatusUnprocessableEntity {
		t.Errorf("Sender-side retained keys = %d, want 422: %s", status, payload)
	}
	if status, _, payload := environment.reportRecovery(t, recovery.SessionID, restricted,
		`{"keyConfirmation":"not_available","retainedPairingIds":["`+association.PairingID+`"]}`); status != http.StatusUnprocessableEntity {
		t.Errorf("a missing key next to retained Pairing IDs = %d, want 422: %s", status, payload)
	}
	// A Pairing that belongs to another Receiver is not this Receiver's key.
	if status, _, payload := environment.reportRecovery(t, recovery.SessionID, restricted,
		retainedReport(unrelated.PairingID)); status != http.StatusUnprocessableEntity {
		t.Errorf("another Receiver's Pairing ID = %d, want 422: %s", status, payload)
	}
	// A rejected report leaves the session where it was, so the approved
	// recovery still completes with its own key inventory.
	if status, view, payload := environment.reportRecovery(t, recovery.SessionID, restricted,
		retainedReport(association.PairingID)); status != http.StatusOK {
		t.Fatalf("valid Receiver report = %d, want 200: %s", status, payload)
	} else if view.State != "awaiting_confirmation" {
		t.Errorf("one-sided recovery = %+v, want awaiting_confirmation", view)
	}
	if status, view, payload := environment.reportRecovery(t, recovery.SessionID, owner.UserSession.Token,
		`{"keyConfirmation":"confirmed"}`); status != http.StatusOK {
		t.Fatalf("Sender report = %d, want 200: %s", status, payload)
	} else if view.State != "credential_ready" {
		t.Errorf("both-sided recovery = %+v, want credential_ready", view)
	}
}

// Recovery supersedes an older grant that was never redeemed, and a lost
// redemption response costs at most the one permitted retransmission.
func TestRecoverySupersedesAnOlderGrantAndRetransmitsOnce(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-supersede", "openid-supersede")

	stale, staleToken := environment.readySession(t, owner)
	recovery, restricted := environment.approvedRecovery(t, owner, stale.ReceiverID,
		"code-supersede-approval", "openid-supersede", stale.PairingID)
	status, first, payload := environment.redeem(t, recovery.SessionID, restricted, "op-supersede", 0)
	if status != http.StatusOK {
		t.Fatalf("recovery redemption status = %d: %s", status, payload)
	}
	// The issuance replaced the older grant, so the session that holds it
	// reports that its grant is gone before anyone tries to redeem it.
	if state := environment.sessionState(t, stale.SessionID); state != "superseded" {
		t.Errorf("the grant the recovery replaced = %q, want superseded", state)
	}

	// The delayed first issuance of the older session cannot replace the
	// credential the newer recovery issued; its own session is superseded.
	status, _, payload = environment.redeem(t, stale.SessionID, staleToken, "op-stale-grant", 0)
	if status != http.StatusGone {
		t.Fatalf("delayed older issuance = %d, want 410: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "pairing_session_closed" {
		t.Errorf("delayed older issuance code = %q, want pairing_session_closed", code)
	}
	if state := environment.sessionState(t, stale.SessionID); state != "superseded" {
		t.Errorf("older grant state = %q, want superseded", state)
	}

	// The lost response is recovered by the one permitted retransmission, which
	// replaces the credential the client could not have saved.
	status, second, payload := environment.redeem(t, recovery.SessionID, restricted, "op-supersede", 1)
	if status != http.StatusOK {
		t.Fatalf("recovery retransmission = %d, want 200: %s", status, payload)
	}
	receiverRejected(t, environment, first.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	receiverConnected(t, environment, second.ReceiverToken)
	if state := environment.sessionState(t, recovery.SessionID); state != "completed" {
		t.Errorf("recovery session after possession = %q, want completed", state)
	}
}

// Recovery issuance is ordered: the newer approved recovery decides the
// credential that stays, whichever of the two redeems first, and an older
// generation cannot replace it afterwards.
func TestRecoveryIssuanceKeepsTheNewerGeneration(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-order", "openid-order")

	association, original := environment.paired(t, owner, "Phone")
	older, olderToken := environment.approvedRecovery(t, owner, association.ReceiverID,
		"code-order-first", "openid-order", association.PairingID)
	newer, newerToken := environment.approvedRecovery(t, owner, association.ReceiverID,
		"code-order-second", "openid-order", association.PairingID)

	status, first, payload := environment.redeem(t, older.SessionID, olderToken, "op-order-older", 0)
	if status != http.StatusOK {
		t.Fatalf("older redemption status = %d: %s", status, payload)
	}
	status, second, payload := environment.redeem(t, newer.SessionID, newerToken, "op-order-newer", 0)
	if status != http.StatusOK {
		t.Fatalf("newer redemption status = %d: %s", status, payload)
	}

	// Only the newer generation authorizes the shared delivery connection, and
	// the older session cannot come back with a retransmission.
	receiverRejected(t, environment, original.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	receiverRejected(t, environment, first.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	receiverConnected(t, environment, second.ReceiverToken)
	status, _, payload = environment.redeem(t, older.SessionID, olderToken, "op-order-older", 1)
	if status != http.StatusGone {
		t.Fatalf("older retransmission after the newer generation = %d, want 410: %s", status, payload)
	}
	if state := environment.sessionState(t, older.SessionID); state != "superseded" {
		t.Errorf("older recovery session state = %q, want superseded", state)
	}

	// The Pairing both clients kept is still the association that forwards.
	if state := environment.pairingState(t, association.PairingID); state != "active" {
		t.Errorf("preserved association state = %q, want active", state)
	}
}

// Two recoveries of one Receiver whose sessions were created in the same
// instant are ordered by the issuance that committed first: the one that only
// finishes afterwards cannot replace the credential that already works,
// whichever identifier its session happens to carry.
func TestRecoveryOfTheSameInstantKeepsTheIssuanceThatCommitted(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-instant", "openid-instant")

	association, _ := environment.paired(t, owner, "Phone")
	first, firstToken := environment.createRecovery(t, association.ReceiverID)
	second, secondToken := environment.createRecovery(t, association.ReceiverID)
	// The service stamps each session when it creates it, so the equal instant
	// this ordering cares about is forced below. The lower identifier reports
	// and redeems first, which leaves the other one as the delayed session.
	issuing, issuingToken, delayed, delayedToken := first, firstToken, second, secondToken
	if delayed.SessionID < issuing.SessionID {
		issuing, issuingToken, delayed, delayedToken = delayed, delayedToken, issuing, issuingToken
	}
	environment.exec(t, `UPDATE pin_glide.pairing_sessions
SET created_at = (SELECT created_at FROM pin_glide.pairing_sessions WHERE id = $1)
WHERE id = $2`, issuing.SessionID, delayed.SessionID)

	// The delayed session has not even been claimed when the other one issues,
	// so nothing but the ordering of that issuance can refuse it afterwards.
	environment.claimRecovery(t, issuing.SessionID, "code-instant-issuing", "openid-instant", owner)
	if status, _, payload := environment.reportRecovery(t, issuing.SessionID, owner.UserSession.Token,
		`{"keyConfirmation":"confirmed"}`); status != http.StatusOK {
		t.Fatalf("Sender recovery report = %d, want 200: %s", status, payload)
	}
	if status, _, payload := environment.reportRecovery(t, issuing.SessionID, issuingToken,
		retainedReport(association.PairingID)); status != http.StatusOK {
		t.Fatalf("Receiver recovery report = %d, want 200: %s", status, payload)
	}
	status, working, payload := environment.redeem(t, issuing.SessionID, issuingToken, "op-instant-issuing", 0)
	if status != http.StatusOK {
		t.Fatalf("redemption of the same-instant grant = %d, want 200: %s", status, payload)
	}

	environment.claimRecovery(t, delayed.SessionID, "code-instant-delayed", "openid-instant", owner)
	if status, _, payload := environment.reportRecovery(t, delayed.SessionID, owner.UserSession.Token,
		`{"keyConfirmation":"confirmed"}`); status != http.StatusOK {
		t.Fatalf("delayed Sender recovery report = %d, want 200: %s", status, payload)
	}
	status, view, payload := environment.reportRecovery(t, delayed.SessionID, delayedToken,
		retainedReport(association.PairingID))
	if status != http.StatusOK {
		t.Fatalf("delayed Receiver recovery report = %d, want 200: %s", status, payload)
	}
	if view.State != "credential_ready" {
		t.Fatalf("delayed recovery = %+v, want credential_ready", view)
	}
	status, _, payload = environment.redeem(t, delayed.SessionID, delayedToken, "op-instant-delayed", 0)
	if status != http.StatusGone {
		t.Fatalf("delayed same-instant redemption = %d, want 410: %s", status, payload)
	}
	if state := environment.sessionState(t, delayed.SessionID); state != "superseded" {
		t.Errorf("the delayed same-instant session = %q, want superseded", state)
	}

	// The credential the first issuance handed out still authorizes the shared
	// delivery channel, and the preserved association still forwards.
	receiverConnected(t, environment, working.ReceiverToken)
	if state := environment.pairingState(t, association.PairingID); state != "active" {
		t.Errorf("preserved association state = %q, want active", state)
	}
}

// The restricted credential's five-minute lifetime bounds redemption only: a
// lapsed recovery session leaves the Pairings it preserved forwarding, and
// owner-approved recovery remains available afterwards.
func TestRecoveryExpiryKeepsPreservedAssociationsAndStaysAvailable(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-expiry", "openid-expiry")

	association, issued := environment.paired(t, owner, "Phone")
	expired, expiredToken := environment.approvedRecovery(t, owner, association.ReceiverID,
		"code-expiry-approval", "openid-expiry", association.PairingID)
	environment.exec(t, `UPDATE pin_glide.pairing_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`,
		expired.SessionID)

	status, _, payload := environment.redeem(t, expired.SessionID, expiredToken, "op-expired-recovery", 0)
	if status != http.StatusUnauthorized {
		t.Fatalf("redemption after B expiry = %d, want 401: %s", status, payload)
	}
	if code := decodeProblem(t, payload).Code; code != "credential_expired" {
		t.Errorf("expired recovery redemption code = %q, want credential_expired", code)
	}
	if state := environment.pairingState(t, association.PairingID); state != "active" {
		t.Errorf("association after a lapsed recovery = %q, want active", state)
	}
	receiverConnected(t, environment, issued.ReceiverToken)

	// The owner approves recovery again and replaces the credential it never
	// received.
	later, laterToken := environment.approvedRecovery(t, owner, association.ReceiverID,
		"code-expiry-later", "openid-expiry", association.PairingID)
	status, replacement, payload := environment.redeem(t, later.SessionID, laterToken, "op-later-recovery", 0)
	if status != http.StatusOK {
		t.Fatalf("later recovery redemption = %d, want 200: %s", status, payload)
	}
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	receiverConnected(t, environment, replacement.ReceiverToken)
}

// A Sender that already holds the five admitted Receivers recovers one of them
// without a sixth slot: the recovery reserves no association, leaves the
// capacity unchanged, and preserves the Pairing.
func TestRecoveryAtTheReceiverLimitNeedsNoSixthSlot(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-limit", "openid-limit")

	association, issued := environment.paired(t, owner, "Phone")
	receiverConnected(t, environment, issued.ReceiverToken)
	environment.reserveSlots(t, owner, 4)

	recovery, restricted := environment.approvedRecovery(t, owner, association.ReceiverID,
		"code-limit-approval", "openid-limit", association.PairingID)
	status, replacement, payload := environment.redeem(t, recovery.SessionID, restricted, "op-limit", 0)
	if status != http.StatusOK {
		t.Fatalf("recovery at the Receiver limit = %d, want 200: %s", status, payload)
	}
	if held := environment.heldReceivers(t, owner.SenderID); held != 5 {
		t.Errorf("held Receivers after recovery = %d, want the unchanged 5", held)
	}
	if state := environment.pairingState(t, association.PairingID); state != "active" {
		t.Errorf("preserved association state = %q, want active", state)
	}
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	receiverConnected(t, environment, replacement.ReceiverToken)
}

// A replacement is handed out only after the superseded generation lost its
// shared delivery access, so a delivery upgrade that is still committing with
// the credential that generation held either never commits or ends with the
// policy close. Either way it holds no connection once the recovery answered,
// and only the replacement reaches the channel.
func TestRecoveryRedemptionAndOldUpgradeCommitOneGeneration(t *testing.T) {
	environment := start(t)
	owner := environment.sender(t, "code-upgrade-race", "openid-upgrade-race")

	association, issued := environment.paired(t, owner, "Phone")
	recovery, restricted := environment.approvedRecovery(t, owner, association.ReceiverID,
		"code-upgrade-race-approval", "openid-upgrade-race", association.PairingID)

	// The superseded credential's handshake races the redemption replacing it.
	type upgrade struct {
		socket *websocket.Conn
		status int
	}
	upgraded := make(chan upgrade, 1)
	go func() {
		socket, response, err := dialReceiver(t, environment, issued.ReceiverToken)
		result := upgrade{socket: socket}
		if err != nil && response != nil {
			result.status = response.StatusCode
		}
		upgraded <- result
	}()

	status, replacement, payload := environment.redeem(t, recovery.SessionID, restricted, "op-upgrade-race", 0)
	if status != http.StatusOK {
		t.Fatalf("recovery redemption status = %d: %s", status, payload)
	}

	if race := <-upgraded; race.socket != nil {
		// The upgrade committed before the generation moved on, so the service
		// ends it instead of leaving it authoritative.
		requireClose(t, race.socket, websocket.StatusPolicyViolation)
	} else if race.status != http.StatusUnauthorized {
		t.Errorf("an upgrade with the superseded credential = %d, want 401", race.status)
	}
	receiverRejected(t, environment, issued.ReceiverToken, http.StatusUnauthorized, "credential_invalid")
	receiverConnected(t, environment, replacement.ReceiverToken)
}
