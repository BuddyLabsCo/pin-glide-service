//go:build integration

// Concurrency, atomicity and secrecy of credential renewal: renewal races
// login and other renewals on one Sender, rolls back completely on storage
// failure, and never exposes a raw credential.
package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
)

// concurrentRefresh posts one renewal attempt without using testing.T, so a
// test can race it with another request.
func concurrentRefresh(environment *loginEnvironment, refreshToken, operationID string, attempt int) (int, []byte, error) {
	status, payload, _, err := concurrentPost(environment, refreshPath, refreshBody(refreshToken, operationID, attempt), "")
	return status, payload, err
}

// concurrentLogin posts one login without using testing.T, so a test can race
// it with another request.
func concurrentLogin(environment *loginEnvironment, body string) (int, []byte, error) {
	status, payload, _, err := concurrentPost(environment, loginPath, body, "")
	return status, payload, err
}

// concurrentPost sends one request off the test goroutine, carrying the
// presented credential when the caller supplies one, and never fails the test
// itself.
func concurrentPost(environment *loginEnvironment, path, body, bearer string) (int, []byte, http.Header, error) {
	request, err := http.NewRequest(http.MethodPost, environment.service.BaseURL+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := environment.service.Client.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, nil, err
	}
	return response.StatusCode, payload, response.Header, nil
}

// Concurrent attempts of one renewal operation serialize on the Sender:
// exactly one commits and the recorded attempt supersedes the other.
func TestRefreshConcurrentAttemptsCommitOnce(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-concurrent", identity.Identity{AppID: "wx-app", OpenID: "openid-concurrent"})
	issued := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-concurrent"}`)

	const attempted = 2
	statuses := make([]int, attempted)
	payloads := make([][]byte, attempted)
	failures := make([]error, attempted)
	var wait sync.WaitGroup
	for index := range statuses {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			statuses[index], payloads[index], failures[index] = concurrentRefresh(
				environment, issued.Refresh.Token, "operation-concurrent", 0)
		}(index)
	}
	wait.Wait()

	accepted := 0
	for index, status := range statuses {
		if failures[index] != nil {
			t.Fatalf("concurrent renewal %d: %v", index, failures[index])
		}
		switch status {
		case http.StatusOK:
			accepted++
		case http.StatusConflict:
			superseded := problem{}
			if err := json.Unmarshal(payloads[index], &superseded); err != nil {
				t.Fatalf("decode superseded problem: %v: %s", err, payloads[index])
			}
			if superseded.Code != "operation_superseded" {
				t.Errorf("concurrent rejection code = %q, want operation_superseded", superseded.Code)
			}
		default:
			t.Errorf("concurrent renewal %d status = %d, want 200 or 409: %s", index, status, payloads[index])
		}
	}
	if accepted != 1 {
		t.Fatalf("concurrent renewals accepted = %d, want exactly 1", accepted)
	}

	// The one accepted rotation stays the family's current token: the
	// superseded attempt neither issued nor revoked anything.
	for index, status := range statuses {
		if status != http.StatusOK {
			continue
		}
		winner := decodeRefresh(t, payloads[index])
		refreshOK(t, environment, winner.Refresh.Token, "operation-after-concurrent", 0)
	}
}

// A fresh login replaces the whole family, and neither a delayed
// retransmission nor a concurrent renewal can restore the replaced family or
// revoke the family the login authenticated.
func TestRefreshCannotDisturbAFreshlyAuthenticatedFamily(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-family", identity.Identity{AppID: "wx-app", OpenID: "openid-family"})
	issued := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-family"}`)
	replaced := refreshOK(t, environment, issued.Refresh.Token, "operation-old", 0)

	// A fresh login restoring the same Sender replaces that family. The
	// delayed retransmission of the old operation then presents a revoked
	// token: it authorizes nothing and revokes nothing.
	authenticated := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-family","senderId":"`+issued.SenderID+`"}`)
	refreshFails(t, environment, issued.Refresh.Token, "operation-old", 1,
		http.StatusUnauthorized, "credential_revoked")
	refreshFails(t, environment, replaced.Refresh.Token, "operation-replaced", 0,
		http.StatusUnauthorized, "credential_revoked")
	refreshOK(t, environment, authenticated.Refresh.Token, "operation-fresh", 0)

	// A renewal racing a fresh login serializes on the Sender, so only the
	// authenticated family authorizes afterwards either way.
	racing := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-family"}`)
	var wait sync.WaitGroup
	var renewedStatus int
	var renewedPayload []byte
	var renewedFailure error
	var loginFailure error
	var loginPayload []byte
	wait.Add(2)
	go func() {
		defer wait.Done()
		renewedStatus, renewedPayload, renewedFailure = concurrentRefresh(
			environment, racing.Refresh.Token, "operation-race", 0)
	}()
	go func() {
		defer wait.Done()
		status, payload, err := concurrentLogin(environment,
			`{"deviceName":"Test Sender","wechatCode":"code-family","senderId":"`+racing.SenderID+`"}`)
		loginPayload = payload
		switch {
		case err != nil:
			loginFailure = err
		case status != http.StatusOK:
			loginFailure = fmt.Errorf("login status = %d: %s", status, payload)
		}
	}()
	wait.Wait()
	if renewedFailure != nil {
		t.Fatalf("racing renewal: %v", renewedFailure)
	}
	if loginFailure != nil {
		t.Fatalf("racing login: %v", loginFailure)
	}

	credentials := identity.NewService(environment.pool, nil)
	ctx := context.Background()
	fresh := decodeBundle(t, loginPayload)
	if _, err := credentials.Authenticate(ctx, fresh.UserSession.Token, identity.ScopeUserSession); err != nil {
		t.Errorf("the logged-in family does not authorize after the race: %v", err)
	}
	switch renewedStatus {
	case http.StatusOK:
		// The renewal committed first: the login replaced that family, so its
		// credentials are revoked with it.
		renewed := decodeRefresh(t, renewedPayload)
		if _, err := credentials.Authenticate(ctx, renewed.UserSession.Token, identity.ScopeUserSession); !errors.Is(err, identity.ErrCredentialInvalid) {
			t.Errorf("the replaced family still authorizes: %v", err)
		}
	case http.StatusUnauthorized:
		rejected := problem{}
		if err := json.Unmarshal(renewedPayload, &rejected); err != nil {
			t.Fatalf("decode racing rejection: %v: %s", err, renewedPayload)
		}
		if rejected.Code != "credential_revoked" {
			t.Errorf("racing renewal code = %q, want credential_revoked", rejected.Code)
		}
	default:
		t.Fatalf("racing renewal status = %d, want 200 or 401: %s", renewedStatus, renewedPayload)
	}
}

// A storage failure while renewal issues credentials rolls the whole operation
// back: the consumed token stays current and the same attempt can commit again.
func TestRefreshRollsBackIssuanceWhenStorageFails(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-storage", identity.Identity{AppID: "wx-app", OpenID: "openid-storage"})
	issued := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-storage"}`)

	environment.exec(t, `CREATE FUNCTION pin_glide.identity_credentials_test_reject() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected credential storage failure'; END $$`)
	environment.exec(t, `CREATE TRIGGER trg_identity_credentials_test_reject
		BEFORE INSERT ON pin_glide.identity_credentials
		FOR EACH ROW EXECUTE FUNCTION pin_glide.identity_credentials_test_reject()`)

	refreshFails(t, environment, issued.Refresh.Token, "operation-storage", 0,
		http.StatusServiceUnavailable, "service_unavailable")

	environment.exec(t, `DROP TRIGGER trg_identity_credentials_test_reject ON pin_glide.identity_credentials`)
	environment.exec(t, `DROP FUNCTION pin_glide.identity_credentials_test_reject()`)

	// Nothing of the failed attempt survived, so the same token and operation
	// commit on the retry instead of being superseded or revoked.
	refreshOK(t, environment, issued.Refresh.Token, "operation-storage", 0)
}

// Renewal hands raw credentials only to their holder: neither the database nor
// service diagnostics carry them.
func TestRefreshKeepsCredentialsOutOfDiagnostics(t *testing.T) {
	var diagnostics bytes.Buffer
	environment := startLoginWith(t, observability.NewLogger(&diagnostics, "integration"))
	environment.provider.Accept("code-diagnostics", identity.Identity{AppID: "wx-app", OpenID: "openid-diagnostics"})
	issued := environment.loginOK(t, `{"deviceName":"Test Sender","wechatCode":"code-diagnostics"}`)
	rotated := refreshOK(t, environment, issued.Refresh.Token, "operation-diagnostics", 0)

	logs := diagnostics.String()
	if logs == "" {
		t.Fatal("the service wrote no diagnostics to inspect")
	}
	for name, secret := range map[string]string{
		"issued userSession":  issued.UserSession.Token,
		"issued uploader":     issued.Uploader.Token,
		"issued refresh":      issued.Refresh.Token,
		"renewed userSession": rotated.UserSession.Token,
		"renewed uploader":    rotated.Uploader.Token,
		"renewed refresh":     rotated.Refresh.Token,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("service diagnostics contain the %s credential", name)
		}
	}
}
