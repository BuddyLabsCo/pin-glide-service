//go:build integration

package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
)

// Issued credentials authorize exactly their own scope, are persisted as
// digests only, and stop authorizing at their real expiry instead of relying on
// row deletion.
func TestSenderCredentialsEnforceScopeAndValidity(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-scope", identity.WeChatIdentity{AppID: "wx-app", OpenID: "openid-scope"})
	issued := environment.loginOK(t, `{"wechatCode":"code-scope"}`)

	credentials := identity.NewService(environment.pool, nil)
	ctx := context.Background()

	for _, accepted := range []struct {
		scope identity.Scope
		token string
	}{
		{scope: identity.ScopeUserSession, token: issued.UserSession.Token},
		{scope: identity.ScopeUploader, token: issued.Uploader.Token},
		{scope: identity.ScopeRefresh, token: issued.Refresh.Token},
	} {
		senderID, err := credentials.Authenticate(ctx, accepted.token, accepted.scope)
		if err != nil || senderID != issued.SenderID {
			t.Errorf("%s did not authorize its own scope: %q %v", accepted.scope, senderID, err)
		}
	}

	for _, rejected := range []struct {
		name  string
		scope identity.Scope
		token string
	}{
		{name: "management credential for upload", scope: identity.ScopeUploader, token: issued.UserSession.Token},
		{name: "upload credential for management", scope: identity.ScopeUserSession, token: issued.Uploader.Token},
		{name: "renewal credential for management", scope: identity.ScopeUserSession, token: issued.Refresh.Token},
		{name: "unknown credential", scope: identity.ScopeUserSession, token: "credential-that-was-never-issued"},
		{name: "missing credential", scope: identity.ScopeUserSession, token: ""},
	} {
		if _, err := credentials.Authenticate(ctx, rejected.token, rejected.scope); !errors.Is(err, identity.ErrCredentialInvalid) {
			t.Errorf("%s was accepted: %v", rejected.name, err)
		}
	}

	// Only digests are stored, so no row can hand a caller a raw credential.
	if stored := environment.count(t, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE token_digest IN ($1, $2, $3)`,
		issued.UserSession.Token, issued.Uploader.Token, issued.Refresh.Token); stored != 0 {
		t.Errorf("%d credential rows contain a raw credential", stored)
	}
	if stored := environment.count(t, `SELECT count(*) FROM pin_glide.identity_credentials
		WHERE sender_id = $1 AND revoked_at IS NULL AND expires_at > now()`, issued.SenderID); stored != 3 {
		t.Errorf("usable credential rows = %d, want 3", stored)
	}

	// Expiry is enforced metadata: an expired credential stops authorizing even
	// while its row exists.
	environment.exec(t, `UPDATE pin_glide.identity_credentials SET expires_at = now() - interval '1 second'
		WHERE sender_id = $1 AND scope = 'user_session'`, issued.SenderID)
	if _, err := credentials.Authenticate(ctx, issued.UserSession.Token, identity.ScopeUserSession); !errors.Is(err, identity.ErrCredentialInvalid) {
		t.Errorf("an expired credential still authorizes: %v", err)
	}
}
