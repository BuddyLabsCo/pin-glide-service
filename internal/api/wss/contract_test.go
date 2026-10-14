package wss

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/goccy/go-yaml"
)

// fixtureDir holds the shared contract fixtures, relative to this package.
const fixtureDir = "../../../test/fixtures/contract/asyncapi"

// definitionsPath is the handwritten WSS surface the published description is
// assembled from.
const definitionsPath = "../../../protocol/asyncapi.yaml"

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

// publishedDefinitions assembles the repository's handwritten WSS surface, so
// the checks below judge the description this release publishes.
func publishedDefinitions(t *testing.T) map[string]any {
	t.Helper()

	definitions, err := os.ReadFile(definitionsPath)
	if err != nil {
		t.Fatalf("read %s: %v", definitionsPath, err)
	}
	shared, err := protocol.WireSchemas()
	if err != nil {
		t.Fatalf("wire schemas: %v", err)
	}
	assembled, err := Assemble(definitions, shared)
	if err != nil {
		t.Fatalf("assemble WSS description: %v", err)
	}
	return decode(t, assembled)
}

// Every closed channel documents the Close meanings of this surface with the
// codes and reasons the connections actually send, so a client reads one
// authority instead of a channel description that drifted from the handlers.
func TestPublishedDescriptionDefinesEveryCloseMeaning(t *testing.T) {
	channels, ok := publishedDefinitions(t)["channels"].(map[string]any)
	if !ok || len(channels) == 0 {
		t.Fatal("the published description declares no channel")
	}

	want := map[int]protocol.CloseCode{}
	for _, documented := range protocol.CloseCodes {
		want[documented.Code] = documented
	}
	// Each channel owns its Close list, so a later change to one channel's
	// meanings cannot silently change another channel's.
	backings := map[uintptr]string{}
	for name, value := range channels {
		channel, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("channel %s is not a mapping", name)
		}
		published, ok := channel["x-close-codes"].([]any)
		if !ok || len(published) == 0 {
			t.Errorf("channel %s states no Close meanings: %v", name, channel["x-close-codes"])
			continue
		}
		backing := reflect.ValueOf(published).Pointer()
		if owner, shared := backings[backing]; shared {
			t.Errorf("channel %s shares its Close meanings with channel %s", name, owner)
		}
		backings[backing] = name

		documented := map[int]protocol.CloseCode{}
		for _, item := range published {
			entry, ok := item.(map[string]any)
			if !ok {
				t.Errorf("channel %s has a Close meaning that is not a mapping: %v", name, item)
				continue
			}
			code, ok := closeCodeValue(entry["code"])
			if !ok {
				t.Errorf("channel %s Close meaning has no integer code: %v", name, entry)
				continue
			}
			reason, _ := entry["reason"].(string)
			documented[code] = protocol.CloseCode{Code: code, Reason: reason}
		}
		for code, meaning := range want {
			stated, found := documented[code]
			if !found {
				t.Errorf("channel %s does not document Close %d", name, code)
				continue
			}
			if stated.Reason != meaning.Reason {
				t.Errorf("channel %s documents Close %d reason %q, want %q", name, code, stated.Reason, meaning.Reason)
			}
			if _, reason := closeMeaning(intentForCode(t, code)); reason != meaning.Reason {
				t.Errorf("Close %d sends reason %q, want the documented %q", code, reason, meaning.Reason)
			}
		}
	}
}

// closeCodeValue reads one published Close code, whose YAML form is a number
// the decoder may represent as either signed or unsigned.
func closeCodeValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case uint64:
		return int(typed), true
	default:
		return 0, false
	}
}

// intentForCode names the intent one documented Close code belongs to, so the
// published meaning and the connection that closes are compared by code.
func intentForCode(t *testing.T, code int) closeIntent {
	t.Helper()

	for _, means := range closeCodes {
		if int(means.code) == code {
			return means.intent
		}
	}
	t.Fatalf("Close %d has no runtime meaning", code)
	return closeNothing
}

// The published error message states every business code the service can send,
// so a client reads the accepted set from the description and a code added to
// the relay cannot ship without the contract that documents it.
func TestPublishedDescriptionDefinesEveryReportedErrorCode(t *testing.T) {
	want := map[string]bool{}
	for _, outcome := range frameCodes {
		want[string(outcome.code)] = true
	}
	want[string(protocol.InternalError)] = true
	want[string(recoverableCode(t))] = true

	declared := publishedErrorCodes(t)
	if len(declared) == 0 {
		t.Fatal("the published description does not state the error message's business codes")
	}
	for reported := range want {
		if !declared[reported] {
			t.Errorf("the service reports %q but the published description does not define it", reported)
		}
	}

	// A code the registry does not carry is not reported as defined, so the
	// check above cannot pass by accepting everything.
	if declared["not_a_published_code"] {
		t.Error("a code the registry does not carry was reported as defined")
	}
}

// publishedErrorCodes returns the business codes the published error message
// declares, keyed by the value a client matches on.
func publishedErrorCodes(t *testing.T) map[string]bool {
	t.Helper()

	schemas, ok := publishedDefinitions(t)["components"].(map[string]any)["schemas"].(map[string]any)
	if !ok {
		t.Fatal("the published description embeds no shared schema mapping")
	}
	errorSchema, ok := schemas["WSSError"].(map[string]any)
	if !ok {
		t.Fatal("the published description does not embed the error message schema")
	}
	available, ok := errorSchema["x-error-codes"].([]any)
	if !ok {
		t.Fatal("the published error message states no business code")
	}
	declared := map[string]bool{}
	for _, item := range available {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("the published error message carries a code entry that is not a mapping: %v", item)
		}
		code, ok := entry["code"].(string)
		if !ok {
			t.Fatalf("the published error message carries an entry without a code: %v", entry)
		}
		declared[code] = true
	}
	return declared
}

// recoverableCode returns the code one recoverable frame failure reports, so
// the set of codes a client can receive covers both outcomes.
func recoverableCode(t *testing.T) protocol.ErrorCode {
	t.Helper()

	code := frameErrorCodeOf(errUnreachablePeer())
	if code == protocol.InternalError {
		t.Fatal("a recoverable frame failure does not report a business code")
	}
	return code
}
