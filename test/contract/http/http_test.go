// Package http_test checks the published HTTP description against the contract
// rules every registered operation must satisfy.
package http_test

import (
	"bytes"
	"regexp"
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
