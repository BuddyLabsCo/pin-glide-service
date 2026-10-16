//go:build integration

// Package privacy_test exercises what the composed service retains: the safe
// outcomes an operator correlates, the table-level retention policies, and the
// secrets that must reach neither PostgreSQL nor service logs. It runs HTTP and
// real WSS requests against the composed service over a disposable real
// PostgreSQL and a fake WeChat boundary.
package privacy_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	wechatfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/wechat"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

// canaries are synthetic secrets a test hands to the service. None of them is a
// working credential, and none may reach a PostgreSQL row, a service log or a
// public error.
const (
	otpCanary        = "otp-canary-421337"
	smsCanary        = "sms-canary-door-code-9900"
	keyCanary        = "key-canary-shared-secret"
	ciphertextCanary = "ciphertext-canary-Zm9vYmFyYmF6"
	refreshCanary    = "refresh-canary-token-MFRB"
	providerCanary   = "provider-canary-secret-token"
	wechatCodeCanary = "wechat-code-canary-9f2b"
	// canaryCode is the authorization the fake provider fails, so this test
	// exercises a provider outage rather than a rejected code.
	canaryCode = "wechat-code-provider-failure"
)

// relayedCanary is the padded-Base64 handshake bytes a pairing_relay frame
// carries. That direction accepts only Base64, so the ciphertext and client key
// canaries travel through the service encoded.
var relayedCanary = base64.StdEncoding.EncodeToString([]byte(ciphertextCanary + "|" + keyCanary))

// environment is one composed service with its disposable database and the
// service log output a test inspects.
type environment struct {
	service     *harness.Service
	provider    *wechatfake.Fake
	pool        *sql.DB
	databaseURL string
	logs        *safeLogBuffer
}

// start composes the service against the shipped schema and collects the
// diagnostics its logger writes.
func start(t *testing.T) *environment {
	t.Helper()

	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")
	// Scheduled cleanup covers release audit history and pg_cron's own run
	// history too, so the composed service runs against both desired schemas.
	postgres.ApplySchema(t, "release")

	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open disposable database: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	provider := wechatfake.New()
	logs := &safeLogBuffer{}
	t.Cleanup(logs.close)
	return &environment{
		service: harness.StartServiceLogging(t, postgres.DSN, provider, nil,
			observability.NewLogger(logs, "integration")),
		provider:    provider,
		pool:        pool,
		databaseURL: postgres.DSN,
		logs:        logs,
	}
}

// safeLogBuffer collects service log output for assertions and reports any
// background write after the test ended instead of panicking.
type safeLogBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
	closed bool
}

func (b *safeLogBuffer) Write(data []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if b.closed {
		return len(data), nil
	}
	return b.buffer.Write(data)
}

func (b *safeLogBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.String()
}

func (b *safeLogBuffer) close() {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.closed = true
}

// bundle is the decoded Sender credential bundle.
type bundle struct {
	SenderID    string `json:"senderId"`
	UserSession struct {
		Token string `json:"token"`
	} `json:"userSession"`
	Uploader struct {
		Token string `json:"token"`
	} `json:"uploader"`
	Refresh struct {
		Token string `json:"token"`
	} `json:"refresh"`
}

// problem is the decoded whole-request failure.
type problem struct {
	Status    int    `json:"status"`
	Code      string `json:"code"`
	RequestID string `json:"requestId"`
}

// sender registers one installation and returns the credential bundle its
// holder uses.
func (e *environment) sender(t *testing.T, code, openID string) bundle {
	t.Helper()
	e.provider.Accept(code, identity.WeChatIdentity{AppID: "wx-app", OpenID: openID})

	status, payload := e.request(t, http.MethodPost, "/v1/auth/wechat",
		`{"wechatCode":"`+code+`"}`, "")
	if status != http.StatusOK {
		t.Fatalf("login status = %d: %s", status, payload)
	}
	var issued bundle
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode credential bundle: %v: %s", err, payload)
	}
	return issued
}

// request issues one JSON request with an optional bearer credential.
func (e *environment) request(t *testing.T, method, path, body, credential string) (int, []byte) {
	t.Helper()

	request, err := http.NewRequest(method, e.service.BaseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := e.service.Client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return response.StatusCode, payload
}

// stopped stops the composed service, which stores the records still queued, so
// every request this test issued is readable afterwards.
func (e *environment) stopped(t *testing.T) {
	t.Helper()

	if err := e.service.Stop(); err != nil {
		t.Fatalf("stop the composed service: %v", err)
	}
}

// diagnostic is one retained service outcome as this test reads it.
type diagnostic struct {
	requestID  string
	kind       string
	subject    sql.NullString
	method     sql.NullString
	code       sql.NullInt64
	durationMS sql.NullInt64
}

// retained reads every record the given query selects.
func (e *environment) retained(t *testing.T, query string, args ...any) []diagnostic {
	t.Helper()

	rows, err := e.pool.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("read retained diagnostics: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var retained []diagnostic
	for rows.Next() {
		var record diagnostic
		if err := rows.Scan(&record.requestID, &record.kind, &record.subject,
			&record.method, &record.code, &record.durationMS); err != nil {
			t.Fatalf("read one retained diagnostic: %v", err)
		}
		retained = append(retained, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read retained diagnostics: %v", err)
	}
	return retained
}

// diagnostics reads every retained record of one correlation identifier.
func (e *environment) diagnostics(t *testing.T, requestID string) []diagnostic {
	t.Helper()

	return e.retained(t, `
SELECT request_id, kind, subject, method, code, duration_ms
FROM pin_glide.service_diagnostics
WHERE request_id = $1
ORDER BY id`, requestID)
}

// storedText reads every retained diagnostic row as text, which is what shows
// whether a value the adopted privacy rules exclude ever reached the table.
func (e *environment) storedText(t *testing.T) string {
	t.Helper()

	rows, err := e.pool.QueryContext(context.Background(), `
SELECT id::text || coalesce(occurred_at::text, '') || request_id || kind
    || coalesce(subject, '') || coalesce(method, '')
    || coalesce(code::text, '') || coalesce(duration_ms::text, '')
FROM pin_glide.service_diagnostics
ORDER BY id`)
	if err != nil {
		t.Fatalf("read retained diagnostics: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var stored strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("read one retained diagnostic: %v", err)
		}
		stored.WriteString(line)
		stored.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read retained diagnostics: %v", err)
	}
	return stored.String()
}

// refuseSecrets requires none of the synthetic secrets to appear in stored
// values, which is what shows they reached neither PostgreSQL nor the log. No
// operation accepts SMS text, an OTP or a client key, so for those the sweep is
// the whole assertion; the rest are handed to the service by the test that
// sweeps.
func refuseSecrets(t *testing.T, where, stored string) {
	t.Helper()

	for name, secret := range map[string]string{
		"authorization code": wechatCodeCanary,
		"provider secret":    providerCanary,
		"OTP":                otpCanary,
		"SMS text":           smsCanary,
		"Pairing key":        keyCanary,
		"message ciphertext": ciphertextCanary,
		"relayed ciphertext": relayedCanary,
		"refresh credential": refreshCanary,
	} {
		if strings.Contains(stored, secret) {
			t.Errorf("%s contains the %s", where, name)
		}
	}
}

// TestHTTPDiagnosticsCarryOnlySafeOutcomes shows the retained outcome of
// requests an operator correlates: a failure before the transport could read a
// body, a rejected authorization and a provider failure, each recorded under
// the request ID the caller was told.
func TestHTTPDiagnosticsCarryOnlySafeOutcomes(t *testing.T) {
	environment := start(t)

	// A body the transport cannot read fails before any business work, so its
	// record is the status alone, and the answer to it must not echo the body
	// the caller sent.
	status, malformedPayload := environment.request(t, http.MethodPost, "/v1/auth/wechat",
		`{"wechatCode":"`+wechatCodeCanary+`"`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("malformed login status = %d, want %d", status, http.StatusBadRequest)
	}
	var malformed problem
	if err := json.Unmarshal(malformedPayload, &malformed); err != nil {
		t.Fatalf("decode malformed-login problem: %v", err)
	}

	// An authorization the provider cannot verify, carrying the synthetic code
	// and OTP this test must never see again.
	status, rejectedPayload := environment.request(t, http.MethodPost, "/v1/auth/wechat",
		`{"wechatCode":"`+wechatCodeCanary+`"}`, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("rejected login status = %d: %s", status, rejectedPayload)
	}
	var rejected problem
	if err := json.Unmarshal(rejectedPayload, &rejected); err != nil {
		t.Fatalf("decode rejected-login problem: %v: %s", err, rejectedPayload)
	}

	// A provider that fails rather than rejects the identity. The failure cause
	// carries no credential of its own, so the record of it can only be safe.
	environment.provider.Fail(canaryCode, fmt.Errorf("%w: provider outage",
		identity.ErrIdentityProvider))
	status, failedPayload := environment.request(t, http.MethodPost, "/v1/auth/wechat",
		`{"wechatCode":"`+canaryCode+`"}`, "")
	if status != http.StatusBadGateway {
		t.Fatalf("provider-failure status = %d: %s", status, failedPayload)
	}
	var failed problem
	if err := json.Unmarshal(failedPayload, &failed); err != nil {
		t.Fatalf("decode provider-failure problem: %v: %s", err, failedPayload)
	}

	// A credential the service rejects exercises the same surface with the
	// bearer value the caller supplied.
	status, credentialPayload := environment.request(t, http.MethodPost, "/v1/auth/logout",
		`{}`, refreshCanary)
	if status != http.StatusUnauthorized {
		t.Fatalf("rejected credential status = %d: %s", status, credentialPayload)
	}

	// Every failure answer is a public surface, so none of them may carry what
	// its caller sent.
	refuseSecrets(t, "the malformed-request error", string(malformedPayload))
	refuseSecrets(t, "the rejected-login error", string(rejectedPayload))
	refuseSecrets(t, "the provider-failure error", string(failedPayload))
	refuseSecrets(t, "the rejected-credential error", string(credentialPayload))

	environment.stopped(t)

	retained := environment.diagnostics(t, failed.RequestID)
	if len(retained) != 1 {
		t.Fatalf("the failed request kept %d records, want 1: %+v", len(retained), retained)
	}
	record := retained[0]
	if record.kind != "http" || !record.method.Valid || record.method.String != http.MethodPost {
		t.Errorf("failed request record = %+v, want one HTTP POST", record)
	}
	if !record.code.Valid || record.code.Int64 != http.StatusBadGateway {
		t.Errorf("failed request record = %+v, want status %d", record, http.StatusBadGateway)
	}
	if !record.durationMS.Valid || record.durationMS.Int64 < 0 {
		t.Errorf("failed request record = %+v, want a whole millisecond duration", record)
	}
	if record.subject.Valid {
		t.Errorf("HTTP record subjects %q, want no connection subject", record.subject.String)
	}

	// A request the service answered before any business work reaches the same
	// correlation surface with its own status.
	if malformedRecord := environment.diagnostics(t, malformed.RequestID); len(malformedRecord) != 1 {
		t.Errorf("the malformed request kept %d records, want 1", len(malformedRecord))
	} else if !malformedRecord[0].code.Valid || malformedRecord[0].code.Int64 != http.StatusBadRequest {
		t.Errorf("malformed request record = %+v, want status %d", malformedRecord[0], http.StatusBadRequest)
	}
	if len(environment.diagnostics(t, rejected.RequestID)) != 1 {
		t.Errorf("the rejected authorization kept no record of its own")
	}

	stored := environment.storedText(t)
	if stored == "" {
		t.Fatal("the service retained no diagnostics to inspect")
	}
	refuseSecrets(t, "service rows", stored)
	refuseSecrets(t, "service logs", environment.logs.String())
}
