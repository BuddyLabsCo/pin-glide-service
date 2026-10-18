//go:build integration

// Package model_test exercises temporary model credentials the way a Sender
// experiences them: HTTP requests against the composed service, a disposable
// real PostgreSQL holding the shipped schema, and fake WeChat, QQ and Bailian
// boundaries.
package model_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/model"
	"github.com/BuddyLabsCo/pin-glide-service/internal/observability"
	bailianfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/bailian"
	qqfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/qq"
	wechatfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/wechat"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
)

const (
	// credentialsPath is the implemented model credential operation.
	credentialsPath = "/v1/model/credentials"
	// loginPath issues the upload credential that operation requires.
	loginPath = "/v1/auth/wechat"
	// logoutPath ends one Sender's access without closing its registration.
	logoutPath = "/v1/auth/logout"
	// fixturePath is the accepted model credential contract, relative to this
	// package.
	fixturePath = "../../fixtures/contract/http/model_credentials_v1.json"
)

// modelFixture is the accepted contract both this test and the published
// description check.
type modelFixture struct {
	Success struct {
		Required []string `json:"required"`
	} `json:"success"`
	Credential struct {
		Required []string `json:"required"`
	} `json:"credential"`
	Configuration struct {
		Required []string `json:"required"`
	} `json:"configuration"`
	Inference struct {
		Required []string `json:"required"`
	} `json:"inference"`
	ResponseFormat struct {
		Required []string `json:"required"`
	} `json:"responseFormat"`
	Errors map[string][]string `json:"errors"`
}

// problem is the decoded whole-request failure payload.
type problem struct {
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
	RequestID string `json:"requestId"`
}

// senderBundle is the login payload this suite reads: the registration it may
// restore and the credentials its holder uses.
type senderBundle struct {
	SenderID    string `json:"senderId"`
	UserSession struct {
		Token string `json:"token"`
	} `json:"userSession"`
	Uploader struct {
		Token string `json:"token"`
	} `json:"uploader"`
}

// extractionFormat is the strict structured-output request the Sender sends to
// the configured model.
type extractionFormat struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name   string `json:"name"`
		Strict bool   `json:"strict"`
		Schema struct {
			Type                 string         `json:"type"`
			Properties           map[string]any `json:"properties"`
			Required             []string       `json:"required"`
			AdditionalProperties bool           `json:"additionalProperties"`
		} `json:"schema"`
	} `json:"json_schema"`
}

// modelCredential is the decoded success payload.
type modelCredential struct {
	Credential struct {
		APIKey    string    `json:"apiKey"`
		ExpiresAt time.Time `json:"expiresAt"`
	} `json:"credential"`
	Configuration struct {
		Provider  string `json:"provider"`
		Region    string `json:"region"`
		BaseURL   string `json:"baseUrl"`
		Inference struct {
			Model          string           `json:"model"`
			EnableThinking bool             `json:"enable_thinking"`
			ResponseFormat extractionFormat `json:"response_format"`
		} `json:"inference"`
	} `json:"configuration"`
}

// environment is one composed service with its disposable database and the
// faked WeChat, QQ and Bailian boundaries.
type environment struct {
	service  *harness.Service
	provider *wechatfake.Fake
	qq       *qqfake.Fake
	mint     *bailianfake.Fake
	pool     *sql.DB
}

// start composes the service against the shipped schema and the harness's own
// stderr diagnostics.
func start(t *testing.T) *environment {
	t.Helper()
	return startLogging(t, nil)
}

// startLogging composes the service like start with the supplied diagnostics
// logger, so a test can inspect what the service reports.
func startLogging(t *testing.T, logger *slog.Logger) *environment {
	t.Helper()
	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")

	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open disposable database: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	provider, qqProvider, mint := wechatfake.New(), qqfake.New(), bailianfake.New()
	if logger == nil {
		logger = observability.NewLogger(os.Stderr, "integration")
	}
	return &environment{
		service:  harness.StartProvidersLogging(t, postgres.DSN, []identity.Provider{provider, qqProvider}, mint, logger),
		provider: provider,
		qq:       qqProvider,
		mint:     mint,
		pool:     pool,
	}
}

// configure enables one model configuration the way an operator does, whether
// the row exists already or not.
func (e *environment) configure(t *testing.T, workspaceID, modelID string) {
	t.Helper()

	e.exec(t, `INSERT INTO pin_glide.model_configurations (id, workspace_id, model)
		VALUES ('default', $1, $2)
		ON CONFLICT (id) DO UPDATE
		SET workspace_id = EXCLUDED.workspace_id, model = EXCLUDED.model,
			enabled = true, updated_at = now()`, workspaceID, modelID)
}

// disable keeps the configuration row and stops the service issuing from it.
func (e *environment) disable(t *testing.T) {
	t.Helper()

	e.exec(t, `UPDATE pin_glide.model_configurations
		SET enabled = false, updated_at = now() WHERE id = 'default'`)
}

// sender logs in one installation, restoring senderID when it is given, and
// returns the credentials its holder uses.
func (e *environment) sender(t *testing.T, code, openID, senderID string) senderBundle {
	t.Helper()

	e.provider.Accept(code, identity.Identity{AppID: "wx-app", OpenID: openID})
	body := fmt.Sprintf(`{"wechatCode":%q}`, code)
	if senderID != "" {
		body = fmt.Sprintf(`{"wechatCode":%q,"senderId":%q}`, code, senderID)
	}
	return e.login(t, loginPath, body)
}

func (e *environment) login(t *testing.T, path, body string) senderBundle {
	t.Helper()

	response, payload, err := e.post(path, body, "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d: %s", response.StatusCode, http.StatusOK, payload)
	}

	issued := senderBundle{}
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode login payload: %v: %s", err, payload)
	}
	return issued
}

// credentials issues one model credential request, reporting a transport
// failure instead of failing the test, so a caller can also run it concurrently.
func (e *environment) credentials(credential string) (*http.Response, []byte, error) {
	return e.post(credentialsPath, "", credential)
}

// post issues one request against the composed service.
func (e *environment) post(path, body, credential string) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, e.service.BaseURL+path, strings.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}

	response, err := e.service.Client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s response: %w", path, err)
	}
	return response, payload, nil
}

// fails requires one failed request and checks its status, its code and that
// the accepted fixture lists that code for that status.
func (e *environment) fails(t *testing.T, credential string, wantStatus int, wantCode string) problem {
	t.Helper()

	response, payload, err := e.credentials(credential)
	if err != nil {
		t.Fatalf("model credentials: %v", err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("model credentials status = %d, want %d: %s", response.StatusCode, wantStatus, payload)
	}

	decoded := problem{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, payload)
	}
	if decoded.Code != wantCode {
		t.Errorf("problem code = %q, want %q: %s", decoded.Code, wantCode, payload)
	}
	if accepted := readFixture(t).Errors[strconv.Itoa(wantStatus)]; !slices.Contains(accepted, decoded.Code) {
		t.Errorf("accepted fixture lists %v for status %d, service reported %q", accepted, wantStatus, decoded.Code)
	}
	if decoded.Status != wantStatus {
		t.Errorf("problem status field = %d, want %d", decoded.Status, wantStatus)
	}
	return decoded
}

// exec runs one statement against the disposable database.
func (e *environment) exec(t *testing.T, statement string, args ...any) {
	t.Helper()

	if _, err := e.pool.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

// count returns one row count from the disposable database.
func (e *environment) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()

	var value int64
	if err := e.pool.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// countStoredValues returns how many rows of the whole Pin Glide schema hold
// value in a textual column. The service must persist no raw provider
// credential, so the check also covers a table this suite does not name.
func (e *environment) countStoredValues(t *testing.T, value string) int64 {
	t.Helper()

	rows, err := e.pool.QueryContext(context.Background(), `
		SELECT table_name, column_name
		FROM information_schema.columns
		WHERE table_schema = 'pin_glide'
		  AND data_type IN ('text', 'character varying', 'character')
		ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("list textual columns: %v", err)
	}

	var columns []string
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			_ = rows.Close()
			t.Fatalf("scan textual column: %v", err)
		}
		columns = append(columns, table+"\x00"+column)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("list textual columns: %v", err)
	}
	_ = rows.Close()

	var total int64
	for _, name := range columns {
		table, column, _ := strings.Cut(name, "\x00")
		total += e.count(t, fmt.Sprintf(
			`SELECT count(*) FROM pin_glide.%q WHERE %q::text = $1`, table, column), value)
	}
	return total
}

// readFixture decodes the accepted model credential contract.
func readFixture(t *testing.T) modelFixture {
	t.Helper()

	contents, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read model credential fixture: %v", err)
	}
	fixture := modelFixture{}
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatalf("decode model credential fixture: %v", err)
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

// assertPayloadFields requires one nested payload to carry exactly the accepted
// fields of the compatibility fixture.
func assertPayloadFields(t *testing.T, parent map[string]any, name string, accepted []string) map[string]any {
	t.Helper()

	nested, ok := parent[name].(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want a payload object", name, parent[name])
	}
	if fields := sortedKeys(t, nested); !equalFields(fields, accepted) {
		t.Errorf("%s fields = %v, want %v", name, fields, accepted)
	}
	return nested
}

// assertStrictExtractionSchema requires the adopted extraction profile: one
// strict JSON Schema with a single required nullable-string otp and no other
// property, never the unconstrained JSON Object fallback.
func assertStrictExtractionSchema(t *testing.T, format extractionFormat) {
	t.Helper()

	if format.Type != "json_schema" || format.JSONSchema.Name != "otp_extraction" || !format.JSONSchema.Strict {
		t.Errorf("response format = %+v, want the named strict JSON Schema", format)
	}
	schema := format.JSONSchema.Schema
	if schema.Type != "object" || schema.AdditionalProperties || !slices.Equal(schema.Required, []string{"otp"}) {
		t.Errorf("extraction schema = %+v, want one required otp and no other property", schema)
	}
	if len(schema.Properties) != 1 {
		t.Fatalf("extraction properties = %v, want exactly otp", schema.Properties)
	}
	otp, ok := schema.Properties["otp"].(map[string]any)
	if !ok {
		t.Fatalf("otp property = %v, want an object", schema.Properties["otp"])
	}
	types, _ := otp["type"].([]any)
	if len(types) != 2 || types[0] != "string" || types[1] != "null" {
		t.Errorf("otp type = %v, want the nullable string [string, null]", otp["type"])
	}
}

// One upload credential receives one temporary provider key with the enabled
// configuration snapshot it was issued for: the provider's actual expiry, the
// workspace-specific base URL, the adopted inference profile and an
// uncacheable body, while the raw key reaches no database row.
func TestModelCredentialsReturnTheProviderKeyWithItsConfigurationSnapshot(t *testing.T) {
	environment := start(t)
	environment.configure(t, "llm-workspace", "qwen3.7-flash")
	sender := environment.sender(t, "code-model", "openid-model", "")

	actualExpiry := time.Now().UTC().Add(20 * time.Minute).Truncate(time.Second)
	environment.mint.Answer(model.Credential{APIKey: "st-issued-key", ExpiresAt: actualExpiry})

	response, payload, err := environment.credentials(sender.Uploader.Token)
	if err != nil {
		t.Fatalf("model credentials: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("model credentials status = %d, want %d: %s", response.StatusCode, http.StatusOK, payload)
	}
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("model credentials Cache-Control = %q, want no-store", cacheControl)
	}

	fixture := readFixture(t)
	decoded := map[string]any{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v: %s", err, payload)
	}
	if fields := sortedKeys(t, decoded); !equalFields(fields, fixture.Success.Required) {
		t.Fatalf("model credential fields = %v, want %v", fields, fixture.Success.Required)
	}
	assertPayloadFields(t, decoded, "credential", fixture.Credential.Required)
	configuration := assertPayloadFields(t, decoded, "configuration", fixture.Configuration.Required)
	inference := assertPayloadFields(t, configuration, "inference", fixture.Inference.Required)
	assertPayloadFields(t, inference, "response_format", fixture.ResponseFormat.Required)

	issued := modelCredential{}
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode model credential: %v: %s", err, payload)
	}
	if issued.Credential.APIKey != "st-issued-key" {
		t.Errorf("issued key = %q, want the provider's own key", issued.Credential.APIKey)
	}
	if !issued.Credential.ExpiresAt.Equal(actualExpiry) {
		t.Errorf("issued expiry = %s, want the provider's actual expiry %s",
			issued.Credential.ExpiresAt, actualExpiry)
	}
	if issued.Configuration.Provider != "bailian" || issued.Configuration.Region != "cn-beijing" {
		t.Errorf("configuration = %+v, want the adopted provider and region", issued.Configuration)
	}
	if want := "https://llm-workspace.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"; issued.Configuration.BaseURL != want {
		t.Errorf("base URL = %q, want %q", issued.Configuration.BaseURL, want)
	}
	if issued.Configuration.Inference.Model != "qwen3.7-flash" || issued.Configuration.Inference.EnableThinking {
		t.Errorf("inference = %+v, want the configured model with thinking disabled", issued.Configuration.Inference)
	}
	assertStrictExtractionSchema(t, issued.Configuration.Inference.ResponseFormat)

	// The snapshot the response states is the one the provider was called with.
	if requests := environment.mint.Requests(); len(requests) != 1 || requests[0].WorkspaceID != "llm-workspace" {
		t.Errorf("provider requests = %+v, want one issuance for the configured workspace", requests)
	}
	if stored := environment.countStoredValues(t, "st-issued-key"); stored != 0 {
		t.Errorf("%d database rows contain the raw provider credential", stored)
	}
}

// The enabled row is the only source of the Workspace ID that becomes both the
// published base URL and the parent-key request, so the shipped schema itself
// refuses a stored value that is not one Workspace label.
func TestModelConfigurationRefusesAWorkspaceValueThatIsNotOneLabel(t *testing.T) {
	environment := start(t)

	_, err := environment.pool.ExecContext(context.Background(),
		`INSERT INTO pin_glide.model_configurations (id, workspace_id, model)
		VALUES ('default', 'evil.example/', 'qwen3.7-flash')`)
	if err == nil || !strings.Contains(err.Error(), "chk_model_configurations_workspace_id_is_a_host_label") {
		t.Fatalf("inserting a URL-shaped workspace ID = %v, want the host-label constraint", err)
	}

	// The same statement with one label is accepted, so the refusal above is
	// the constraint rather than a schema the test cannot reach.
	environment.configure(t, "llm-workspace", "qwen3.7-flash")
}
