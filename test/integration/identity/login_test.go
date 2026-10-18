//go:build integration

// Package identity_test exercises Sender login the way a client experiences it:
// HTTP requests against the composed service, a disposable real PostgreSQL
// holding the shipped schema, and a fake WeChat boundary.
package identity_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	qqfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/qq"
	wechatfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/wechat"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

// loginPath is the implemented WeChat login operation.
const loginPath = "/v1/auth/wechat"

// fixturePath is the accepted login contract, relative to this package.
const fixturePath = "../../fixtures/contract/http/wechat_login_v1.json"

// loginFixture is the accepted contract both this test and the published
// description check.
type loginFixture struct {
	Success struct {
		Required []string `json:"required"`
	} `json:"success"`
	Credential struct {
		Required []string `json:"required"`
	} `json:"credential"`
	Errors map[string]string `json:"errors"`
}

// bundle is the decoded login success payload.
type bundle struct {
	AccountID   string `json:"accountId"`
	SenderID    string `json:"senderId"`
	UserSession struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	} `json:"userSession"`
	Uploader struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	} `json:"uploader"`
	Refresh struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	} `json:"refresh"`
}

// problem is the decoded whole-request failure payload.
type problem struct {
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
	RequestID string `json:"requestId"`
}

// loginEnvironment is one composed service with its disposable database and
// faked WeChat and QQ boundaries.
type loginEnvironment struct {
	service  *harness.Service
	provider *wechatfake.Fake
	qq       *qqfake.Fake
	pool     *sql.DB
}

// startLogin composes the service against the shipped identity schema.
func startLogin(t *testing.T) *loginEnvironment {
	t.Helper()
	return startLoginWith(t, observability.NewLogger(os.Stderr, "integration"))
}

// startLoginWith composes the same service with a caller-supplied logger, so a
// test can assert which values reach service diagnostics.
func startLoginWith(t *testing.T, logger *slog.Logger) *loginEnvironment {
	t.Helper()

	provider := wechatfake.New()
	qqProvider := qqfake.New()
	environment := startLoginWithProviders(t, logger, provider, qqProvider)
	environment.provider = provider
	environment.qq = qqProvider
	return environment
}

// startLoginWithProviders composes only the identity boundaries a test needs,
// so provider absence remains observable.
func startLoginWithProviders(t *testing.T, logger *slog.Logger, providers ...identity.Provider) *loginEnvironment {
	t.Helper()

	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")

	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open disposable database: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	return &loginEnvironment{
		service: harness.StartProvidersLogging(t, postgres.DSN, providers, nil, logger),
		pool:    pool,
	}
}

// login posts one raw body and returns the status, payload and headers.
func (e *loginEnvironment) login(t *testing.T, body string) (int, []byte, http.Header) {
	t.Helper()
	return post(t, e, loginPath, body)
}

// loginOK requires a successful login and checks it against the accepted
// success and credential fields of the compatibility fixture.
func (e *loginEnvironment) loginOK(t *testing.T, body string) bundle {
	t.Helper()

	status, payload, header := e.login(t, body)
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want %d: %s", status, http.StatusOK, payload)
	}
	if cacheControl := header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("login Cache-Control = %q, want no-store", cacheControl)
	}

	fixture := readLoginFixture(t)
	decoded := map[string]any{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode login payload: %v: %s", err, payload)
	}
	if fields := sortedKeys(t, decoded); !equalFields(fields, fixture.Success.Required) {
		t.Fatalf("login fields = %v, want %v", fields, fixture.Success.Required)
	}
	for _, name := range []string{"userSession", "uploader", "refresh"} {
		credential, ok := decoded[name].(map[string]any)
		if !ok {
			t.Fatalf("login %s is not a credential: %v", name, decoded[name])
		}
		if fields := sortedKeys(t, credential); !equalFields(fields, fixture.Credential.Required) {
			t.Fatalf("login %s fields = %v, want %v", name, fields, fixture.Credential.Required)
		}
	}

	issued := bundle{}
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode credential bundle: %v: %s", err, payload)
	}
	return issued
}

// loginFails requires one failed login and checks the failure against the
// status and code the compatibility fixture accepts for it.
func (e *loginEnvironment) loginFails(t *testing.T, body string, wantStatus int) problem {
	t.Helper()

	status, payload, _ := e.login(t, body)
	if status != wantStatus {
		t.Fatalf("login status = %d, want %d: %s", status, wantStatus, payload)
	}
	decoded := problem{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, payload)
	}
	if want := readLoginFixture(t).Errors[strconv.Itoa(wantStatus)]; decoded.Code != want {
		t.Fatalf("login code = %q, want %q: %s", decoded.Code, want, payload)
	}
	if decoded.Status != wantStatus {
		t.Fatalf("problem status field = %d, want %d", decoded.Status, wantStatus)
	}
	return decoded
}

// exec runs one statement against the database the composed service uses.
func (e *loginEnvironment) exec(t *testing.T, statement string, args ...any) {
	t.Helper()

	if _, err := e.pool.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

// count returns one row count from the database the composed service uses.
func (e *loginEnvironment) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()

	var value int64
	if err := e.pool.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// The composed service registers, distinguishes and restores Sender
// installations, replaces the credential family of a restored one and reports
// every documented failure without confirming another Account's registrations.
func TestWeChatLoginRegistersAndRestoresSenderInstallations(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-first", identity.Identity{AppID: "wx-app", OpenID: "openid-a"})
	environment.provider.Accept("code-second", identity.Identity{AppID: "wx-app", OpenID: "openid-a"})
	environment.provider.Accept("code-other", identity.Identity{AppID: "wx-app", OpenID: "openid-b"})
	environment.provider.Accept("code-revoked", identity.Identity{AppID: "wx-app", OpenID: "openid-revoked"})
	environment.provider.Fail("code-broken", identity.ErrIdentityProvider)

	// A login without a saved Sender ID registers this installation.
	first := environment.loginOK(t, `{"wechatCode":"code-first"}`)
	assertIssuedFamily(t, first, time.Now().UTC())

	// The same Account logging in from another installation gets its own
	// Sender ID, not the first one.
	second := environment.loginOK(t, `{"wechatCode":"code-second"}`)
	if second.AccountID != first.AccountID {
		t.Errorf("Account mapping: second login account = %q, want %q", second.AccountID, first.AccountID)
	}
	if second.SenderID == first.SenderID || second.SenderID == "" {
		t.Errorf("installation distinction: second login sender = %q, first = %q", second.SenderID, first.SenderID)
	}

	// A fresh same-Account login restoring the saved ID replaces that Sender's
	// credential family: the delayed old family cannot authorize afterwards.
	restored := environment.loginOK(t, `{"wechatCode":"code-first","senderId":"`+first.SenderID+`"}`)
	if restored.AccountID != first.AccountID || restored.SenderID != first.SenderID {
		t.Errorf("restore = %+v, want account %q and sender %q", restored, first.AccountID, first.SenderID)
	}
	if restored.UserSession.Token == first.UserSession.Token || restored.Refresh.Token == first.Refresh.Token {
		t.Error("fresh login reused a previous family credential")
	}
	credentials := identity.NewService(environment.pool, nil)
	if _, err := credentials.Authenticate(context.Background(), first.UserSession.Token, identity.ScopeUserSession); err == nil {
		t.Error("the replaced family still authorizes management")
	}
	if senderID, err := credentials.Authenticate(context.Background(), restored.UserSession.Token, identity.ScopeUserSession); err != nil || senderID != first.SenderID {
		t.Errorf("the fresh family does not authorize management: %q %v", senderID, err)
	}

	// Another Account cannot see or take over the registration, and an unknown
	// ID stays resource_not_found.
	environment.loginFails(t, `{"wechatCode":"code-other","senderId":"`+first.SenderID+`"}`, http.StatusNotFound)
	environment.loginFails(t, `{"wechatCode":"code-first","senderId":"sender-that-never-existed"}`, http.StatusNotFound)

	// An owned, permanently security-revoked registration is refused and
	// never recreated.
	environment.exec(t, `INSERT INTO pin_glide.identity_accounts (id, provider, app_id, open_id)
		VALUES ('account-revoked', 'wechat', 'wx-app', 'openid-revoked')`)
	environment.exec(t, `INSERT INTO pin_glide.identity_sender_registrations (id, account_id, state, revoked_at)
		VALUES ('sender-revoked', 'account-revoked', 'revoked', now())`)
	environment.loginFails(t, `{"wechatCode":"code-revoked","senderId":"sender-revoked"}`, http.StatusConflict)
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.identity_sender_registrations WHERE account_id = 'account-revoked'`); count != 1 {
		t.Errorf("revoked registration was recreated: %d rows", count)
	}

	// A shared UnionID never merges Accounts.
	environment.provider.Accept("code-union-a", identity.Identity{AppID: "wx-app", OpenID: "openid-union-a", UnionID: "union-shared"})
	environment.provider.Accept("code-union-b", identity.Identity{AppID: "wx-app", OpenID: "openid-union-b", UnionID: "union-shared"})
	unionA := environment.loginOK(t, `{"wechatCode":"code-union-a"}`)
	unionB := environment.loginOK(t, `{"wechatCode":"code-union-b"}`)
	if unionA.AccountID == unionB.AccountID {
		t.Errorf("a shared UnionID merged two Accounts into %q", unionA.AccountID)
	}

	// Whole-request failures and provider outcomes keep their documented
	// status and code.
	environment.loginFails(t, `{"wechatCode":""}`, http.StatusUnprocessableEntity)
	environment.loginFails(t, `{"senderId":"sender-x"}`, http.StatusUnprocessableEntity)
	environment.loginFails(t, `not json`, http.StatusBadRequest)
	environment.loginFails(t, `{"wechatCode":"code-that-was-never-issued"}`, http.StatusUnauthorized)
	environment.loginFails(t, `{"wechatCode":"code-broken"}`, http.StatusBadGateway)
}

// A storage failure inside the login transaction must roll the whole
// registration back: no Account or Sender row may survive a login whose
// credentials were never stored, and the same login must succeed once storage
// answers again.
func TestWeChatLoginRollsBackWhenCredentialStorageFails(t *testing.T) {
	environment := startLogin(t)
	environment.provider.Accept("code-rollback", identity.Identity{AppID: "wx-app", OpenID: "openid-rollback"})

	rejectCredentials := `CREATE FUNCTION pin_glide.identity_credentials_test_reject() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected credential storage failure'; END $$`
	environment.exec(t, rejectCredentials)
	environment.exec(t, `CREATE TRIGGER trg_identity_credentials_test_reject
		BEFORE INSERT ON pin_glide.identity_credentials
		FOR EACH ROW EXECUTE FUNCTION pin_glide.identity_credentials_test_reject()`)

	environment.loginFails(t, `{"wechatCode":"code-rollback"}`, http.StatusServiceUnavailable)
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.identity_accounts
		WHERE provider = 'wechat' AND app_id = 'wx-app' AND open_id = 'openid-rollback'`); count != 0 {
		t.Errorf("failed login left %d Account rows", count)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.identity_sender_registrations`); count != 0 {
		t.Errorf("failed login left %d Sender registrations", count)
	}

	environment.exec(t, `DROP TRIGGER trg_identity_credentials_test_reject ON pin_glide.identity_credentials`)
	environment.exec(t, `DROP FUNCTION pin_glide.identity_credentials_test_reject()`)
	if issued := environment.loginOK(t, `{"wechatCode":"code-rollback"}`); issued.SenderID == "" {
		t.Error("the retried login issued no Sender registration")
	}
}

// assertIssuedFamily checks the three independent credentials and their actual
// lifetimes against the adopted 3h/3h/7d metadata.
func assertIssuedFamily(t *testing.T, issued bundle, loginTime time.Time) {
	t.Helper()

	if issued.AccountID == "" || issued.SenderID == "" {
		t.Fatalf("login issued %+v, want an Account and Sender identity", issued)
	}
	tokens := []string{issued.UserSession.Token, issued.Uploader.Token, issued.Refresh.Token}
	for _, token := range tokens {
		if token == "" {
			t.Fatal("login issued an empty credential")
		}
	}
	if tokens[0] == tokens[1] || tokens[0] == tokens[2] || tokens[1] == tokens[2] {
		t.Errorf("issued credentials are not independent: %v", tokens)
	}

	for _, expected := range []struct {
		name     string
		expires  time.Time
		lifetime time.Duration
	}{
		{name: "userSession", expires: issued.UserSession.ExpiresAt, lifetime: identity.AccessLifetime},
		{name: "uploader", expires: issued.Uploader.ExpiresAt, lifetime: identity.AccessLifetime},
		{name: "refresh", expires: issued.Refresh.ExpiresAt, lifetime: identity.RefreshLifetime},
	} {
		want := loginTime.Add(expected.lifetime)
		if expected.expires.Before(want.Add(-time.Minute)) || expected.expires.After(want.Add(time.Minute)) {
			t.Errorf("%s expires at %s, want about %s", expected.name, expected.expires, want)
		}
	}
}

func readLoginFixture(t *testing.T) loginFixture {
	t.Helper()

	contents, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read login fixture: %v", err)
	}
	fixture := loginFixture{}
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatalf("decode login fixture: %v", err)
	}
	return fixture
}

// sortedKeys returns the sorted field names of one decoded JSON object.
func sortedKeys(t *testing.T, object map[string]any) []string {
	t.Helper()

	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

// equalFields compares two field lists that do not depend on order.
func equalFields(fields, expected []string) bool {
	want := append([]string(nil), expected...)
	sort.Strings(want)
	return slices.Equal(fields, want)
}
