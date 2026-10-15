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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	wechatfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/wechat"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
	"github.com/coder/websocket"
)

// canaries are synthetic secrets a test hands to the service. None of them is a
// working credential, and none may reach a PostgreSQL row or a service log.
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

// diagnostics reads every retained record of one correlation identifier.
func (e *environment) diagnostics(t *testing.T, requestID string) []diagnostic {
	t.Helper()

	rows, err := e.pool.QueryContext(context.Background(), `
SELECT request_id, kind, subject, method, code, duration_ms
FROM pin_glide.service_diagnostics
WHERE request_id = $1
ORDER BY id`, requestID)
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
// values, which is what shows they reached neither PostgreSQL nor the log.
func refuseSecrets(t *testing.T, where, stored string) {
	t.Helper()

	for name, secret := range map[string]string{
		"authorization code": wechatCodeCanary,
		"provider secret":    providerCanary,
		"OTP":                otpCanary,
		"SMS text":           smsCanary,
		"Pairing key":        keyCanary,
		"message ciphertext": ciphertextCanary,
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
	// record is the status alone.
	status, payload := environment.request(t, http.MethodPost, "/v1/auth/wechat",
		`{"wechatCode":`, "")
	if status != http.StatusBadRequest {
		t.Fatalf("malformed login status = %d: %s", status, payload)
	}
	var malformed problem
	if err := json.Unmarshal(payload, &malformed); err != nil {
		t.Fatalf("decode malformed-login problem: %v: %s", err, payload)
	}

	// An authorization the provider cannot verify, carrying the synthetic code
	// and OTP this test must never see again.
	status, payload = environment.request(t, http.MethodPost, "/v1/auth/wechat",
		`{"wechatCode":"`+wechatCodeCanary+`"}`, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("rejected login status = %d: %s", status, payload)
	}
	var rejected problem
	if err := json.Unmarshal(payload, &rejected); err != nil {
		t.Fatalf("decode rejected-login problem: %v: %s", err, payload)
	}

	// A provider that fails rather than rejects the identity. The failure cause
	// carries no credential of its own, so the record of it can only be safe.
	environment.provider.Fail(canaryCode, fmt.Errorf("%w: provider outage",
		identity.ErrIdentityProvider))
	status, payload = environment.request(t, http.MethodPost, "/v1/auth/wechat",
		`{"wechatCode":"`+canaryCode+`"}`, "")
	if status != http.StatusBadGateway {
		t.Fatalf("provider-failure status = %d: %s", status, payload)
	}
	var failed problem
	if err := json.Unmarshal(payload, &failed); err != nil {
		t.Fatalf("decode provider-failure problem: %v: %s", err, payload)
	}

	// A credential the service rejects exercises the same surface with the
	// bearer value the caller supplied.
	status, payload = environment.request(t, http.MethodPost, "/v1/auth/logout",
		`{}`, refreshCanary)
	if status != http.StatusUnauthorized {
		t.Fatalf("rejected credential status = %d: %s", status, payload)
	}

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

// dial opens one authenticated pairing subscription and returns its session ID
// once the snapshot the service sends on commit has arrived.
func (e *environment) dial(t *testing.T, sessionID, credential string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.service.BaseURL, "http") +
		"/v1/pairing-sessions/" + sessionID + "/events"
	socket, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + credential}},
		Subprotocols: []string{"pin-glide.v1"},
	})
	if err != nil {
		t.Fatalf("dial pairing channel %s: %v: %+v", url, err, response)
	}
	if _, _, err := socket.Read(ctx); err != nil {
		_ = socket.CloseNow()
		t.Fatalf("read the committed snapshot: %v", err)
	}
	// The retained record of a connection whose peer left first carries the
	// normal meaning the published close codes define.
	if err := socket.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close the pairing channel: %v", err)
	}
}

// TestWSSDiagnosticsCorrelateOneConnection shows the retained outcome of a real
// pairing subscription: the session it belonged to, the status that committed
// it and the Close status it ended with, under the operation that opened it.
func TestWSSDiagnosticsCorrelateOneConnection(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-wss", "openid-wss")

	status, payload := environment.request(t, http.MethodPost, "/v1/pairing-sessions",
		`{"mode":"new_receiver"}`, "")
	if status != http.StatusCreated {
		t.Fatalf("create Pairing Session status = %d: %s", status, payload)
	}
	var created struct {
		Session struct {
			SessionID string `json:"sessionId"`
		} `json:"session"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode created session: %v: %s", err, payload)
	}

	// The Sender's own subscription belongs to an authenticated session, so it
	// claims the session first, exactly as its client does.
	status, payload = environment.request(t, http.MethodPost,
		"/v1/pairing-sessions/"+created.Session.SessionID+"/authorize",
		`{"alias":"Diagnosed"}`, sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}

	environment.dial(t, created.Session.SessionID, sender.UserSession.Token)
	environment.stopped(t)

	// The subscription's own operation is the request that opened it, which the
	// service logged with the same identifiers it retained.
	retained := environment.diagnosticsFor(t, "wss", created.Session.SessionID)
	if len(retained) != 1 {
		t.Fatalf("the pairing connection kept %d records, want 1: %+v", len(retained), retained)
	}
	record := retained[0]
	if !record.code.Valid || record.code.Int64 != int64(websocket.StatusNormalClosure) {
		t.Errorf("pairing connection record = %+v, want close status %d",
			record, websocket.StatusNormalClosure)
	}
	if record.method.Valid || record.durationMS.Valid {
		t.Errorf("pairing connection record = %+v, want no request fields", record)
	}
	refuseSecrets(t, "service rows", environment.storedText(t))
	refuseSecrets(t, "service logs", environment.logs.String())
}

// diagnosticsFor reads every retained record of one surface and subject.
func (e *environment) diagnosticsFor(t *testing.T, kind, subject string) []diagnostic {
	t.Helper()

	rows, err := e.pool.QueryContext(context.Background(), `
SELECT request_id, kind, subject, method, code, duration_ms
FROM pin_glide.service_diagnostics
WHERE kind = $1 AND subject = $2
ORDER BY id`, kind, subject)
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
