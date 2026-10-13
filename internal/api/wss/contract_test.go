package wss

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/goccy/go-yaml"
)

// fixtureDir holds the shared contract fixtures, relative to this package.
const fixtureDir = "../../../test/fixtures/contract/asyncapi"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return contents
}

// loadSchema reads a canonical shared schema fixture as the JSON value the
// schema registry produces.
func loadSchema(t *testing.T, name string) map[string]any {
	t.Helper()

	schema := map[string]any{}
	if err := json.Unmarshal(readFixture(t, name), &schema); err != nil {
		t.Fatalf("decode schema fixture %s: %v", name, err)
	}
	return schema
}

func decode(t *testing.T, document []byte) map[string]any {
	t.Helper()

	decoded := map[string]any{}
	if err := yaml.Unmarshal(document, &decoded); err != nil {
		t.Fatalf("decode assembled description: %v: %s", err, document)
	}
	return decoded
}

// A declared surface keeps its channels and messages, and every payload schema
// it shares with the service is embedded, so a client can validate the
// published description without the service repository.
func TestAssembleEmbedsTheSharedSchemasItReferences(t *testing.T) {
	shared, err := protocol.WireSchemas()
	if err != nil {
		t.Fatalf("wire schemas: %v", err)
	}

	assembled, err := Assemble(readFixture(t, "coverage.yaml"), shared)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	document := decode(t, assembled)

	components, ok := document["components"].(map[string]any)
	if !ok {
		t.Fatalf("assembled description has no components: %s", assembled)
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("assembled description has no components.schemas: %s", assembled)
	}
	if _, embedded := schemas["ErrorItem"]; !embedded {
		t.Errorf("shared schema ErrorItem is not embedded: %v", schemas)
	}

	channels, ok := document["channels"].(map[string]any)
	if !ok || channels["receiverEvents"] == nil {
		t.Errorf("assembled description lost its declared channels: %s", assembled)
	}
	if _, kept := document["operations"].(map[string]any)["deliver"]; !kept {
		t.Errorf("assembled description lost its declared operations: %s", assembled)
	}
}

// The AsyncAPI schema dialect is JSON Schema draft-07, so a shared schema is
// converted instead of published with keywords the dialect would ignore.
func TestAssembleConvertsSharedSchemasToTheAsyncAPIDialect(t *testing.T) {
	definitions := []byte(`
asyncapi: 3.1.0
info:
  title: Conversion fixture
  version: 0.1.0
channels:
  events:
    address: /v1/receiver/events
    messages:
      converted:
        payload:
          $ref: '#/components/schemas/Convertible'
operations: {}
`)
	shared := map[string]any{"Convertible": loadSchema(t, "convertible_schema.json")}

	assembled, err := Assemble(definitions, shared)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	components := decode(t, assembled)["components"].(map[string]any)
	converted, ok := components["schemas"].(map[string]any)["Convertible"].(map[string]any)
	if !ok {
		t.Fatalf("shared schema Convertible is not embedded: %s", assembled)
	}

	if _, leaked := converted["$schema"]; leaked {
		t.Error("conversion kept the source dialect marker $schema")
	}
	definitions7, ok := converted["definitions"].(map[string]any)
	if !ok || definitions7["Nested"] == nil {
		t.Errorf("conversion did not move $defs to definitions: %v", converted)
	}

	properties := converted["properties"].(map[string]any)
	nested := properties["nested"].(map[string]any)
	if nested["$ref"] != "#/definitions/Nested" {
		t.Errorf("conversion did not rewrite the subschema reference: %v", nested)
	}
	bounded := properties["bounded"].(map[string]any)
	if bounded["exclusiveMinimum"] != true || bounded["minimum"] != 7.0 {
		t.Errorf("conversion did not express the exclusive bound in draft-07: %v", bounded)
	}
}

// A shared schema the dialect cannot express must fail assembly instead of
// publishing a description that validates less than the service promises.
func TestAssembleRejectsAnUnsupportedSharedSchemaConstruct(t *testing.T) {
	definitions := []byte(`
asyncapi: 3.1.0
channels:
  events:
    address: /v1/receiver/events
    messages:
      unsupported:
        payload:
          $ref: '#/components/schemas/Unsupported'
operations: {}
`)
	shared := map[string]any{"Unsupported": loadSchema(t, "unsupported_schema.json")}

	_, err := Assemble(definitions, shared)
	if err == nil {
		t.Fatal("assembly accepted a shared schema the AsyncAPI dialect cannot express")
	}
	if !strings.Contains(err.Error(), "unevaluatedProperties") {
		t.Errorf("assembly error %q does not name the unsupported construct", err)
	}
}

func TestAssembleRejectsDefinitionsItCannotPublish(t *testing.T) {
	tests := []struct {
		name        string
		definitions string
		want        string
	}{
		{
			name: "reference without a shared schema",
			definitions: `
asyncapi: 3.1.0
channels:
  events:
    address: /v1/receiver/events
    messages:
      missing:
        payload:
          $ref: '#/components/schemas/Missing'
operations: {}
`,
			want: "internal/protocol",
		},
		{
			name: "reference outside the description",
			definitions: `
asyncapi: 3.1.0
channels:
  events:
    address: /v1/receiver/events
    messages:
      external:
        payload:
          $ref: './payloads.yaml#/Delivery'
operations: {}
`,
			want: "self-contained",
		},
		{
			name: "payload schemas written by hand",
			definitions: `
asyncapi: 3.1.0
channels: {}
operations: {}
components:
  schemas:
    Delivery:
      type: object
`,
			want: "generated from internal/protocol",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Assemble([]byte(test.definitions), map[string]any{})
			if err == nil {
				t.Fatalf("assembly accepted %s", test.name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("assembly error %q does not mention %q", err, test.want)
			}
		})
	}
}
