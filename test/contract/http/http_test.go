// Package http_test checks the published HTTP description against the contract
// rules every registered operation must satisfy.
package http_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api/httpapi"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/goccy/go-yaml"
)

// verbResource matches the published operation identifiers: one of the HTTP
// verbs followed by the resource it acts on, so a generated client method name
// is stable and readable.
var verbResource = regexp.MustCompile(`^(Get|Post|Put|Patch|Delete)[A-Z][A-Za-z0-9]*$`)

// fixtureDir holds the shared HTTP contract fixtures, relative to this package.
const fixtureDir = "../../fixtures/contract/http"

// The published description must describe every implemented route with a
// stable operation identifier, exactly one domain tag, an explicit
// authentication requirement and the shared problem schema on every failure
// response. A route registered without any of these cannot be published.
func TestPublishedHTTPDescriptionCoversTheImplementedRoutes(t *testing.T) {
	description := published(t)

	paths, ok := description["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatalf("published description has no operations: %v", description)
	}

	for path, item := range paths {
		operations, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("path %s is not an operation item: %v", path, item)
		}
		for method, operation := range operations {
			assertOperation(t, method, path, operation)
		}
	}

	health, ok := paths["/health"].(map[string]any)["get"].(map[string]any)
	if !ok {
		t.Fatalf("the implemented health route is not described: %v", paths)
	}
	assertProblemResponse(t, "/health", "get", health, "503")
}

func assertOperation(t *testing.T, method, path string, value any) {
	t.Helper()

	operation, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s %s is not an operation: %v", method, path, value)
	}
	if id, _ := operation["operationId"].(string); !verbResource.MatchString(id) {
		t.Errorf("operation %s %s has identifier %q, want a VerbResource name", method, path, operation["operationId"])
	}
	if tags, _ := operation["tags"].([]any); len(tags) != 1 {
		t.Errorf("operation %s %s has tags %v, want exactly one domain tag", method, path, operation["tags"])
	}
	if _, stated := operation["security"]; !stated {
		t.Errorf("operation %s %s does not state its authentication requirement", method, path)
	}
	if summary, _ := operation["summary"].(string); summary == "" {
		t.Errorf("operation %s %s has no summary", method, path)
	}

	responses, ok := operation["responses"].(map[string]any)
	if !ok || len(responses) == 0 {
		t.Fatalf("operation %s %s has no responses: %v", method, path, operation)
	}
	for status := range responses {
		if strings.HasPrefix(status, "2") {
			continue
		}
		assertProblemResponse(t, path, method, operation, status)
	}
}

// assertProblemResponse requires the shared problem schema on a failure
// response, so every operation reports failures the same way.
func assertProblemResponse(t *testing.T, path, method string, operation map[string]any, status string) {
	t.Helper()

	responses := operation["responses"].(map[string]any)
	response, ok := responses[status].(map[string]any)
	if !ok {
		t.Fatalf("operation %s %s has no %s response: %v", method, path, status, responses)
	}
	content, ok := response["content"].(map[string]any)
	if !ok {
		t.Fatalf("operation %s %s response %s has no content: %v", method, path, status, response)
	}
	problem, ok := content["application/problem+json"].(map[string]any)
	if !ok {
		t.Fatalf("operation %s %s response %s is not a problem detail: %v", method, path, status, content)
	}
	schema, ok := problem["schema"].(map[string]any)
	if !ok {
		t.Fatalf("operation %s %s response %s has no schema: %v", method, path, status, problem)
	}
	if ref, want := schema["$ref"], protocol.SchemaPrefix+"Problem"; ref != want {
		t.Errorf("operation %s %s response %s references %v, want %s", method, path, status, ref, want)
	}
}

// The shared schemas are part of the published description: a client must
// resolve the problem schema and its validation items without the service
// repository.
func TestPublishedHTTPDescriptionEmbedsItsSharedSchemas(t *testing.T) {
	description := published(t)

	schemas, ok := description["components"].(map[string]any)["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("published description has no components.schemas: %v", description)
	}
	problem, ok := schemas["Problem"].(map[string]any)
	if !ok {
		t.Fatalf("published description does not embed the problem schema: %v", schemas)
	}
	items, ok := problem["properties"].(map[string]any)["errors"].(map[string]any)["items"].(map[string]any)
	if !ok {
		t.Fatalf("problem schema has no validation item schema: %v", problem)
	}
	if ref, want := items["$ref"], protocol.SchemaPrefix+"ErrorItem"; ref != want {
		t.Errorf("problem schema references %v, want %s", ref, want)
	}
	if _, embedded := schemas["ErrorItem"].(map[string]any); !embedded {
		t.Errorf("published description does not embed the shared item schema: %v", schemas)
	}
}

func published(t *testing.T) map[string]any {
	t.Helper()

	var buffer bytes.Buffer
	if err := httpapi.WriteOpenAPI(&buffer); err != nil {
		t.Fatalf("write HTTP description: %v", err)
	}

	description := map[string]any{}
	if err := yaml.Unmarshal(buffer.Bytes(), &description); err != nil {
		t.Fatalf("decode HTTP description: %v: %s", err, buffer.String())
	}
	return description
}

// operationContract is the accepted published shape of one HTTP operation: the
// request, success and credential schemas with the exact field set a released
// client handles. A fixture file holds this part plus its failure statuses, and
// the published description and the composed service are both checked against
// that one file, so a client-visible rename cannot pass one check and fail the
// other.
type operationContract struct {
	OperationID string `json:"operationId"`
	Path        string `json:"path"`
	Method      string `json:"method"`
	Request     struct {
		Schema     string   `json:"schema"`
		Required   []string `json:"required"`
		Properties []string `json:"properties"`
	} `json:"request"`
	Success struct {
		Schema   string   `json:"schema"`
		Status   string   `json:"status"`
		Required []string `json:"required"`
	} `json:"success"`
	Credential struct {
		Schema   string   `json:"schema"`
		Required []string `json:"required"`
	} `json:"credential"`
}

// loginFixture is the accepted v1 compatibility contract of the WeChat login
// operation, whose failures report one code per status.
type loginFixture struct {
	operationContract
	Errors map[string]string `json:"errors"`
}

// refreshFixture is the accepted v1 compatibility contract of credential
// renewal, whose failures can report several codes for one status: an unknown,
// expired or revoked credential is distinguishable from confirmed reuse.
type refreshFixture struct {
	operationContract
	Errors map[string][]string `json:"errors"`
}

// logoutFixture is the accepted v1 compatibility contract of ordinary logout,
// whose success returns no payload and whose failures can report several codes
// for one status: an unknown, expired or revoked credential is distinguishable
// from a missing one.
type logoutFixture struct {
	operationContract
	Errors map[string][]string `json:"errors"`
}

// The published description must keep describing exactly the accepted login
// contract: the documented request fields, success payload, credential payload
// and every failure status a released v1 client handles.
func TestPublishedHTTPDescriptionKeepsTheWeChatLoginContract(t *testing.T) {
	fixture := readFixture[loginFixture](t, "wechat_login_v1.json")
	publishedErrors := assertOperationContract(t, published(t), fixture.operationContract)
	assertFailureStatuses(t, publishedErrors, fixtureStatuses(fixture.Errors))
}

// The published description must keep describing exactly the accepted renewal
// contract: the documented request fields, success payload, credential payload
// and every failure status a released v1 client handles.
func TestPublishedHTTPDescriptionKeepsTheRefreshContract(t *testing.T) {
	fixture := readFixture[refreshFixture](t, "refresh_v1.json")
	publishedErrors := assertOperationContract(t, published(t), fixture.operationContract)
	assertFailureStatuses(t, publishedErrors, fixtureStatuses(fixture.Errors))
}

// The published description must keep describing exactly the accepted ordinary
// logout contract: the documented optional body fields, a success that returns
// no payload at all and every failure status a released v1 client handles.
func TestPublishedHTTPDescriptionKeepsTheLogoutContract(t *testing.T) {
	fixture := readFixture[logoutFixture](t, "logout_v1.json")
	description := published(t)
	operation := operationOf(t, description, fixture.operationContract)

	requestBody := operation["requestBody"].(map[string]any)
	if required, _ := requestBody["required"].(bool); required {
		t.Error("ordinary logout requires a body, but its accepted contract makes the body optional")
	}
	content := requestBody["content"].(map[string]any)["application/json"].(map[string]any)
	requestName, request := schemaOf(t, description, content["schema"])
	if requestName != fixture.Request.Schema {
		t.Errorf("request schema = %q, want %q", requestName, fixture.Request.Schema)
	}
	if required := requiredFields(t, request); !slices.Equal(required, fixture.Request.Required) {
		t.Errorf("request required fields = %v, want %v", required, fixture.Request.Required)
	}
	if properties := propertyFields(t, request); !slices.Equal(properties, fixture.Request.Properties) {
		t.Errorf("request fields = %v, want %v", properties, fixture.Request.Properties)
	}

	responses := operation["responses"].(map[string]any)
	success, ok := responses[fixture.Success.Status].(map[string]any)
	if !ok {
		t.Fatalf("published description does not document success status %s", fixture.Success.Status)
	}
	if content, present := success["content"]; present {
		t.Errorf("the bodyless logout success carries content: %v", content)
	}

	publishedErrors := map[string]bool{}
	for status := range responses {
		if !strings.HasPrefix(status, "2") {
			publishedErrors[status] = true
		}
	}
	assertFailureStatuses(t, publishedErrors, fixtureStatuses(fixture.Errors))
}

// assertOperationContract checks the published request, success and credential
// schemas of one operation against its accepted fixture, and returns the
// failure statuses the description documents for it.
func assertOperationContract(t *testing.T, description map[string]any, fixture operationContract) map[string]bool {
	t.Helper()

	operation := operationOf(t, description, fixture)

	requestBody := operation["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
	requestName, request := schemaOf(t, description, requestBody["schema"])
	if requestName != fixture.Request.Schema {
		t.Errorf("request schema = %q, want %q", requestName, fixture.Request.Schema)
	}
	if required := requiredFields(t, request); !slices.Equal(required, fixture.Request.Required) {
		t.Errorf("request required fields = %v, want %v", required, fixture.Request.Required)
	}
	if properties := propertyFields(t, request); !slices.Equal(properties, fixture.Request.Properties) {
		t.Errorf("request fields = %v, want %v", properties, fixture.Request.Properties)
	}

	responses := operation["responses"].(map[string]any)
	success := responses[fixture.Success.Status].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
	successName, successSchema := schemaOf(t, description, success["schema"])
	if successName != fixture.Success.Schema {
		t.Errorf("success schema = %q, want %q", successName, fixture.Success.Schema)
	}
	if required := requiredFields(t, successSchema); !slices.Equal(required, fixture.Success.Required) {
		t.Errorf("success required fields = %v, want %v", required, fixture.Success.Required)
	}

	credentialName, credential := schemaOf(t, description, successSchema["properties"].(map[string]any)["userSession"])
	if credentialName != fixture.Credential.Schema {
		t.Errorf("credential schema = %q, want %q", credentialName, fixture.Credential.Schema)
	}
	if required := requiredFields(t, credential); !slices.Equal(required, fixture.Credential.Required) {
		t.Errorf("credential required fields = %v, want %v", required, fixture.Credential.Required)
	}

	publishedErrors := map[string]bool{}
	for status := range responses {
		if !strings.HasPrefix(status, "2") {
			publishedErrors[status] = true
		}
	}
	return publishedErrors
}

// assertFailureStatuses requires one operation's published failure statuses to
// be exactly the accepted ones.
func assertFailureStatuses(t *testing.T, published map[string]bool, accepted []string) {
	t.Helper()

	for _, status := range accepted {
		if !published[status] {
			t.Errorf("published description does not document failure status %s", status)
		}
	}
	for status := range published {
		if !slices.Contains(accepted, status) {
			t.Errorf("published failure status %s is not part of the accepted fixture", status)
		}
	}
}

// fixtureStatuses returns the failure statuses one accepted fixture documents.
func fixtureStatuses[Codes any](errors map[string]Codes) []string {
	statuses := make([]string, 0, len(errors))
	for status := range errors {
		statuses = append(statuses, status)
	}
	slices.Sort(statuses)
	return statuses
}

// operationOf returns the published operation the fixture describes.
func operationOf(t *testing.T, description map[string]any, fixture operationContract) map[string]any {
	t.Helper()

	paths, ok := description["paths"].(map[string]any)
	if !ok {
		t.Fatalf("published description has no paths: %v", description)
	}
	path, ok := paths[fixture.Path].(map[string]any)
	if !ok {
		t.Fatalf("published description does not serve %s: %v", fixture.Path, paths)
	}
	operation, ok := path[fixture.Method].(map[string]any)
	if !ok {
		t.Fatalf("published description does not serve %s %s: %v", fixture.Method, fixture.Path, path)
	}
	if id, _ := operation["operationId"].(string); id != fixture.OperationID {
		t.Fatalf("operation %s %s has identifier %q, want %q", fixture.Method, fixture.Path, id, fixture.OperationID)
	}
	return operation
}

// schemaOf resolves a schema reference to the schema name and definition the
// published description embeds.
func schemaOf(t *testing.T, description map[string]any, value any) (string, map[string]any) {
	t.Helper()

	schema, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("schema value %v is not a schema", value)
	}
	ref, _ := schema["$ref"].(string)
	name, shared := strings.CutPrefix(ref, protocol.SchemaPrefix)
	if !shared {
		t.Fatalf("schema %v is not a shared component reference", schema)
	}
	resolved, ok := description["components"].(map[string]any)["schemas"].(map[string]any)[name].(map[string]any)
	if !ok {
		t.Fatalf("published description does not embed schema %q", name)
	}
	return name, resolved
}

// requiredFields returns the sorted required fields of a schema.
func requiredFields(t *testing.T, schema map[string]any) []string {
	t.Helper()

	values, _ := schema["required"].([]any)
	fields := make([]string, 0, len(values))
	for _, value := range values {
		field, ok := value.(string)
		if !ok {
			t.Fatalf("required entry %v is not a field name", value)
		}
		fields = append(fields, field)
	}
	slices.Sort(fields)
	return fields
}

// propertyFields returns the sorted property names of a schema.
func propertyFields(t *testing.T, schema map[string]any) []string {
	t.Helper()

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema %v has no properties", schema)
	}
	fields := make([]string, 0, len(properties))
	for field := range properties {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	return fields
}

// readFixture decodes the accepted contract of one operation from the shared
// fixture directory.
func readFixture[F any](t *testing.T, name string) F {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var fixture F
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return fixture
}
