//go:build integration

package model_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
)

// A configuration change that lands while one issuance is in flight does not
// mix snapshots: both the provider call and the response carry the snapshot
// read before the change.
func TestModelCredentialsKeepOneSnapshotAcrossAConcurrentConfigurationChange(t *testing.T) {
	environment := start(t)
	environment.configure(t, "llm-first", "qwen3.7-flash")
	sender := environment.sender(t, "code-racing", "openid-racing", "")
	environment.mint.Answer(model.Credential{
		APIKey:    "st-first",
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second),
	})

	arrived := environment.mint.Hold()
	type outcome struct {
		status  int
		payload []byte
	}
	issued := make(chan outcome, 1)
	go func() {
		response, payload, err := environment.credentials(sender.Uploader.Token)
		if err != nil {
			issued <- outcome{status: -1, payload: []byte(err.Error())}
			return
		}
		issued <- outcome{status: response.StatusCode, payload: payload}
	}()

	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the provider was not called in time")
	}
	// The operator replaces the configuration while the issuance is in flight.
	environment.configure(t, "llm-second", "qwen3.7-flash-second")
	environment.mint.Release()

	response := <-issued
	if response.status != http.StatusOK {
		t.Fatalf("model credentials status = %d, want %d: %s", response.status, http.StatusOK, response.payload)
	}
	decoded := modelCredential{}
	if err := json.Unmarshal(response.payload, &decoded); err != nil {
		t.Fatalf("decode model credential: %v: %s", err, response.payload)
	}
	if want := "https://llm-first.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"; decoded.Configuration.BaseURL != want {
		t.Errorf("base URL = %q, want the snapshot read before the change %q", decoded.Configuration.BaseURL, want)
	}
	if decoded.Configuration.Inference.Model != "qwen3.7-flash" {
		t.Errorf("model = %q, want the snapshot read before the change", decoded.Configuration.Inference.Model)
	}
	if requests := environment.mint.Requests(); len(requests) != 1 || requests[0].WorkspaceID != "llm-first" {
		t.Errorf("provider requests = %+v, want the snapshot's own workspace", requests)
	}
}

// An absent or disabled configuration reports the configuration outcome
// instead of calling the provider or reporting a provider failure.
func TestModelCredentialsReportAnUnavailableConfiguration(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-unconfigured", "openid-unconfigured", "")
	environment.mint.Answer(model.Credential{
		APIKey:    "st-unused",
		ExpiresAt: time.Now().UTC().Add(time.Minute).Truncate(time.Second),
	})

	environment.fails(t, sender.Uploader.Token, http.StatusServiceUnavailable, "model_configuration_unavailable")

	environment.configure(t, "llm-workspace", "qwen3.7-flash")
	environment.disable(t)
	environment.fails(t, sender.Uploader.Token, http.StatusServiceUnavailable, "model_configuration_unavailable")

	if requests := environment.mint.Requests(); len(requests) != 0 {
		t.Errorf("the provider was called %d times without an enabled configuration", len(requests))
	}
}

// A provider that fails, times out or answers unusably reports the provider
// outcome instead of an internal error, and the failure detail stays public.
func TestModelCredentialsReportProviderFailuresAsProviderErrors(t *testing.T) {
	environment := start(t)
	environment.configure(t, "llm-workspace", "qwen3.7-flash")
	sender := environment.sender(t, "code-broken", "openid-broken", "")

	for _, failure := range []struct {
		name string
		err  error
	}{
		{name: "provider failure", err: fmt.Errorf("%w: provider returned HTTP 500", model.ErrProvider)},
		{name: "provider timeout", err: fmt.Errorf("%w: %w", model.ErrProvider, context.DeadlineExceeded)},
		{name: "unusable provider answer", err: fmt.Errorf("%w: provider answer has no temporary key", model.ErrProvider)},
	} {
		t.Run(failure.name, func(t *testing.T) {
			environment.mint.Fail(failure.err)

			reported := environment.fails(t, sender.Uploader.Token, http.StatusBadGateway, "model_provider_error")
			if strings.Contains(reported.Detail, "HTTP 500") || strings.Contains(reported.Detail, "temporary key") {
				t.Errorf("provider failure detail = %q, want the safe public detail", reported.Detail)
			}
		})
	}
}

// The issued key reaches only the Sender that asked for it: neither service
// diagnostics nor the database carry the temporary credential.
func TestModelCredentialsKeepTheIssuedKeyOutOfDiagnostics(t *testing.T) {
	var diagnostics bytes.Buffer
	environment := startLogging(t, observability.NewLogger(&diagnostics, "integration"))
	environment.configure(t, "llm-workspace", "qwen3.7-flash")
	sender := environment.sender(t, "code-diagnostics", "openid-diagnostics", "")
	environment.mint.Answer(model.Credential{
		APIKey:    "st-diagnostics-key",
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second),
	})

	response, payload, err := environment.credentials(sender.Uploader.Token)
	if err != nil {
		t.Fatalf("model credentials: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("model credentials status = %d, want %d: %s", response.StatusCode, http.StatusOK, payload)
	}

	logs := diagnostics.String()
	if logs == "" {
		t.Fatal("the service wrote no diagnostics to inspect")
	}
	for name, secret := range map[string]string{
		"issued provider key": "st-diagnostics-key",
		"upload credential":   sender.Uploader.Token,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("service diagnostics contain the %s", name)
		}
	}
}

// Only current upload authority mints a key: a missing, wrongly scoped,
// unknown, expired or invalidated credential reports its own outcome, and the
// provider is never called for one.
func TestModelCredentialsRequireCurrentUploadAuthority(t *testing.T) {
	environment := start(t)
	environment.configure(t, "llm-workspace", "qwen3.7-flash")
	environment.mint.Answer(model.Credential{
		APIKey:    "st-unused",
		ExpiresAt: time.Now().UTC().Add(time.Minute).Truncate(time.Second),
	})
	sender := environment.sender(t, "code-authority", "openid-authority", "")

	environment.fails(t, "", http.StatusUnauthorized, "credential_missing")
	environment.fails(t, sender.UserSession.Token, http.StatusUnauthorized, "credential_invalid")
	environment.fails(t, "credential-that-was-never-issued", http.StatusUnauthorized, "credential_invalid")

	// Access expiry is enforced metadata, not row deletion.
	environment.exec(t, `UPDATE pin_glide.identity_credentials
		SET expires_at = now() - interval '1 second'
		WHERE sender_id = $1 AND scope = 'uploader'`, sender.SenderID)
	environment.fails(t, sender.Uploader.Token, http.StatusUnauthorized, "credential_expired")

	// A fresh login replaces that Sender's family, so the replaced upload
	// credential mints nothing. Its row survives the replacement revoked, which
	// the service reports the way it reports any other unusable credential.
	replacement := environment.sender(t, "code-authority", "openid-authority", sender.SenderID)
	if replacement.Uploader.Token == sender.Uploader.Token {
		t.Fatal("fresh login reused the previous upload credential")
	}
	environment.fails(t, sender.Uploader.Token, http.StatusUnauthorized, "credential_invalid")

	// Ordinary logout revokes the replacement's whole family too.
	response, payload, err := environment.post(logoutPath, "", replacement.Uploader.Token)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d: %s", response.StatusCode, http.StatusNoContent, payload)
	}
	environment.fails(t, replacement.Uploader.Token, http.StatusUnauthorized, "credential_invalid")

	// Only an owner's permanent security revocation reports the revoked
	// outcome: a closed registration authorizes nothing, whatever its rows
	// still record.
	latest := environment.sender(t, "code-authority", "openid-authority", sender.SenderID)
	environment.provider.Accept("code-owner-revocation",
		identity.Identity{AppID: "wx-app", OpenID: "openid-authority"})
	revocation, revoked, err := environment.post(
		"/v1/devices/"+latest.SenderID+"/revoke", `{"wechatCode":"code-owner-revocation"}`, latest.UserSession.Token)
	if err != nil {
		t.Fatalf("owner revocation: %v", err)
	}
	if revocation.StatusCode != http.StatusNoContent {
		t.Fatalf("owner revocation status = %d, want %d: %s", revocation.StatusCode, http.StatusNoContent, revoked)
	}
	environment.fails(t, latest.Uploader.Token, http.StatusUnauthorized, "credential_revoked")

	if requests := environment.mint.Requests(); len(requests) != 0 {
		t.Errorf("the provider was called %d times without current upload authority", len(requests))
	}
}
