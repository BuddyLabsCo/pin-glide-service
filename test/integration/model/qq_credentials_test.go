//go:build integration

package model_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/model"
)

// qqSender logs in one installation through a QQ authorization code.
func (e *environment) qqSender(t *testing.T, code, openID string) senderBundle {
	t.Helper()

	e.qq.Accept(code, identity.Identity{AppID: "qq-app", OpenID: openID})
	return e.login(t, "/v1/auth/qq", fmt.Sprintf(`{"deviceName":"Test Sender","qqCode":%q}`, code))
}

// A QQ uploader credential follows the same model-credential path as a WeChat
// uploader credential.
func TestQQUploaderReceivesModelCredential(t *testing.T) {
	environment := start(t)
	environment.configure(t, "llm-qq", "qwen3.7-flash")
	sender := environment.qqSender(t, "code-model-qq", "openid-model-qq")

	expiry := time.Now().UTC().Add(20 * time.Minute).Truncate(time.Second)
	environment.mint.Answer(model.Credential{APIKey: "st-qq-key", ExpiresAt: expiry})
	response, payload, err := environment.credentials(sender.Uploader.Token)
	if err != nil {
		t.Fatalf("model credentials: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("model credentials status = %d, want %d: %s", response.StatusCode, http.StatusOK, payload)
	}

	issued := modelCredential{}
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode model credential: %v: %s", err, payload)
	}
	if issued.Credential.APIKey != "st-qq-key" || !issued.Credential.ExpiresAt.Equal(expiry) {
		t.Errorf("issued credential = %+v, want the provider key and expiry", issued.Credential)
	}
	if issued.Configuration.Inference.Model != "qwen3.7-flash" {
		t.Errorf("model = %q, want the enabled QQ account's configuration", issued.Configuration.Inference.Model)
	}
}
