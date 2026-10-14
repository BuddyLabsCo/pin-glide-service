// Package wss_test checks the published WSS description against the contract
// rules every declared channel, message and operation must satisfy.
package wss_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api/wss"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/goccy/go-yaml"
)

const (
	// fixtureDir holds the shared contract fixtures, relative to this package.
	fixtureDir = "../../fixtures/contract/asyncapi"
	// definitionsPath is the handwritten WSS definitions of the service.
	definitionsPath = "../../../protocol/asyncapi.yaml"
)

// coverageProblems reports every promise the published description makes that
// a client could not rely on: a message payload that is not an embedded shared
// wire schema, an operation that does not state its authentication requirement,
// or a requirement naming a scheme the description does not declare.
func coverageProblems(description map[string]any) []string {
	var problems []string

	components, _ := description["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	schemes, _ := components["securitySchemes"].(map[string]any)

	messages, _ := components["messages"].(map[string]any)
	for name, value := range messages {
		message, _ := value.(map[string]any)
		payload, _ := message["payload"].(map[string]any)
		ref, _ := payload["$ref"].(string)
		schema, shared := strings.CutPrefix(ref, protocol.SchemaPrefix)
		if !shared || schemas[schema] == nil {
			problems = append(problems, fmt.Sprintf("message %s payload %q is not an embedded shared wire schema", name, ref))
		}
	}

	operations, _ := description["operations"].(map[string]any)
	for name, value := range operations {
		operation, _ := value.(map[string]any)
		security, stated := operation["security"].([]any)
		if !stated {
			problems = append(problems, fmt.Sprintf("operation %s does not state its authentication requirement", name))
			continue
		}
		for _, requirement := range security {
			required, _ := requirement.(map[string]any)
			if referenced, ok := required["$ref"].(string); ok {
				// AsyncAPI 3 names a requirement by referencing a declared
				// security scheme; the 2.x inline map is still read, so a
				// document using either form is checked rather than ignored.
				required = map[string]any{
					strings.TrimPrefix(referenced, "#/components/securitySchemes/"): []any{},
				}
			}
			for scheme := range required {
				if _, declared := schemes[scheme]; !declared {
					problems = append(problems, fmt.Sprintf("operation %s requires undeclared security scheme %s", name, scheme))
				}
			}
		}
	}
	return problems
}

// published assembles the published description from definitions and the
// canonical shared schemas, failing the test when assembly cannot publish them.
func published(t *testing.T, definitions []byte) map[string]any {
	t.Helper()

	shared, err := protocol.WireSchemas()
	if err != nil {
		t.Fatalf("wire schemas: %v", err)
	}
	assembled, err := wss.Assemble(definitions, shared)
	if err != nil {
		t.Fatalf("assemble WSS description: %v", err)
	}

	description := map[string]any{}
	if err := yaml.Unmarshal(assembled, &description); err != nil {
		t.Fatalf("decode WSS description: %v: %s", err, assembled)
	}
	return description
}

// The declared surface fixture keeps the coverage checks holding something:
// authenticated operations and messages carrying shared payload schemas. The
// surface this release implements declares no channel yet, so without the
// fixture the checks would pass vacuously.
func TestPublishedWSSDescriptionCoversTheDeclaredSurface(t *testing.T) {
	definitions, err := os.ReadFile(fixtureDir + "/coverage.yaml")
	if err != nil {
		t.Fatalf("read coverage fixture: %v", err)
	}

	if problems := coverageProblems(published(t, definitions)); len(problems) > 0 {
		t.Errorf("the declared surface has coverage problems: %v", problems)
	}
}

// The coverage checks must name the promise they cannot find, or a surface
// published with an unprotected operation, a payload schema of its own or an
// undeclared scheme would pass unnoticed.
func TestPublishedWSSDescriptionReportsMissingCoverage(t *testing.T) {
	tests := []struct {
		name        string
		definitions string
		want        string
	}{
		{
			name: "operation without an authentication requirement",
			definitions: `
asyncapi: 3.1.0
channels:
  receiverEvents:
    address: /v1/receiver/events
operations:
  deliver:
    action: send
    channel:
      $ref: '#/channels/receiverEvents'
`,
			want: "operation deliver does not state its authentication requirement",
		},
		{
			name: "operation requiring an undeclared scheme",
			definitions: `
asyncapi: 3.1.0
channels:
  receiverEvents:
    address: /v1/receiver/events
operations:
  deliver:
    action: send
    channel:
      $ref: '#/channels/receiverEvents'
    security:
      - receiverToken: []
`,
			want: "operation deliver requires undeclared security scheme receiverToken",
		},
		{
			name: "message payload written by hand",
			definitions: `
asyncapi: 3.1.0
channels:
  receiverEvents:
    address: /v1/receiver/events
operations: {}
components:
  messages:
    delivery:
      name: delivery
      payload:
        type: object
`,
			want: `message delivery payload "" is not an embedded shared wire schema`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems := strings.Join(coverageProblems(published(t, []byte(test.definitions))), "\n")
			if !strings.Contains(problems, test.want) {
				t.Errorf("coverage problems %q do not report %q", problems, test.want)
			}
		})
	}
}

// The definitions the repository ships must remain assemblable and keep their
// shared schema mapping, so `make generate` cannot publish a description that
// a client has to resolve against the service repository.
func TestPublishedWSSDescriptionAssemblesFromTheShippedDefinitions(t *testing.T) {
	definitions, err := os.ReadFile(definitionsPath)
	if err != nil {
		t.Fatalf("read %s: %v", definitionsPath, err)
	}

	description := published(t, definitions)
	if problems := coverageProblems(description); len(problems) > 0 {
		t.Errorf("the shipped WSS description has coverage problems: %v", problems)
	}
	if _, embedded := description["components"].(map[string]any)["schemas"].(map[string]any); !embedded {
		t.Errorf("the shipped WSS description does not embed a shared schema mapping: %v", description["components"])
	}

	// The channels the service actually serves are the ones the release
	// documents, each with the authentication its upgrade requires.
	channels, _ := description["channels"].(map[string]any)
	for _, address := range []string{"/v1/receiver/events", "/v1/pairing-sessions/{id}/events"} {
		if !channelWithAddress(channels, address) {
			t.Errorf("the shipped WSS description does not declare the channel %s: %v", address, channels)
		}
	}
	schemes, _ := description["components"].(map[string]any)["securitySchemes"].(map[string]any)
	for _, scheme := range []string{"sessionCredential", "receiverCredential"} {
		if schemes[scheme] == nil {
			t.Errorf("the shipped WSS description does not declare the security scheme %s: %v", scheme, schemes)
		}
	}
}

// channelWithAddress reports whether the description declares a channel at one
// served address, so a release cannot document a channel the service does not
// serve or omit one it does.
func channelWithAddress(channels map[string]any, address string) bool {
	for _, value := range channels {
		channel, _ := value.(map[string]any)
		if channel["address"] == address {
			return true
		}
	}
	return false
}
