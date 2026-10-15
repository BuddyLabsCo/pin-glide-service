// Package wss_test checks the published WSS description against the contract
// rules every declared channel, message and operation must satisfy.
package wss_test

import (
	"fmt"
	"os"
	"path"
	"slices"
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

// v1Inventory is every application message of the adopted v1 contract: the
// channel that carries it, the direction it travels in, the type a released
// client matches on and the shared payload schema it resolves.
var v1Inventory = []struct {
	address string
	action  string
	schema  string
	name    string
}{
	// The Receiver's single delivery channel: the service writes one delivery
	// per admitted Delivery, reports what withdrew a Sender's authority, and
	// accepts only the arrival Receipt of the authenticated Receiver.
	{address: "/v1/receiver/events", action: "send", schema: "Delivery", name: "delivery"},
	{address: "/v1/receiver/events", action: "send", schema: "PairingRemoved", name: "pairing_removed"},
	{address: "/v1/receiver/events", action: "send", schema: "DeviceRevoked", name: "device_revoked"},
	{address: "/v1/receiver/events", action: "receive", schema: "Receipt", name: "receipt"},
	// One Pairing Session's restricted channel: opaque relay traffic in both
	// directions, the session state, the credential notice and the recoverable
	// application error. Only this channel carries error: a delivery-channel
	// frame the service cannot read is a protocol error that closes instead.
	{address: "/v1/pairing-sessions/{id}/events", action: "receive", schema: "PairingRelayIn", name: "pairing_relay"},
	{address: "/v1/pairing-sessions/{id}/events", action: "send", schema: "PairingRelayOut", name: "pairing_relay"},
	{address: "/v1/pairing-sessions/{id}/events", action: "send", schema: "PairingState", name: "pairing_state"},
	{address: "/v1/pairing-sessions/{id}/events", action: "send", schema: "CredentialReady", name: "credential_ready"},
	{address: "/v1/pairing-sessions/{id}/events", action: "send", schema: "WSSError", name: "error"},
}

// The shipped description must publish every message of the adopted v1
// inventory, so a released client reads the direction, the message name, the
// channel and the exact payload schema from the description instead of guessing
// them from the service.
func TestPublishedWSSDescriptionCarriesTheAdoptedV1Inventory(t *testing.T) {
	definitions, err := os.ReadFile(definitionsPath)
	if err != nil {
		t.Fatalf("read %s: %v", definitionsPath, err)
	}
	description := published(t, definitions)

	for _, message := range v1Inventory {
		t.Run(message.action+"-"+message.schema, func(t *testing.T) {
			assertChannelMessage(t, description, message.address, message.action, message.schema, message.name)
		})
	}
}

// The shipped description must carry nothing beyond the adopted v1 inventory: a
// message type a released client does not know is a protocol error, so a new
// service-initiated message needs a new negotiated version rather than
// appearing on this one.
func TestPublishedWSSDescriptionCarriesNothingBeyondTheAdoptedV1Inventory(t *testing.T) {
	definitions, err := os.ReadFile(definitionsPath)
	if err != nil {
		t.Fatalf("read %s: %v", definitionsPath, err)
	}
	description := published(t, definitions)
	channels, _ := description["channels"].(map[string]any)
	descriptions, _ := description["components"].(map[string]any)["messages"].(map[string]any)

	// One operation names the direction, the channel address and the shared
	// payload schema of every message it carries, which is the triple a
	// generated client resolves.
	carried := []string{}
	operations, _ := description["operations"].(map[string]any)
	for name, value := range operations {
		operation, _ := value.(map[string]any)
		action, _ := operation["action"].(string)
		channelRef, _ := operation["channel"].(map[string]any)["$ref"].(string)
		channel, _ := channels[path.Base(channelRef)].(map[string]any)
		address, _ := channel["address"].(string)
		messages, _ := operation["messages"].([]any)
		for _, value := range messages {
			messageRef, _ := value.(map[string]any)["$ref"].(string)
			named, _ := channel["messages"].(map[string]any)[path.Base(messageRef)].(map[string]any)
			shared, _ := named["$ref"].(string)
			definition, _ := descriptions[path.Base(shared)].(map[string]any)
			schema, declared := definition["payload"].(map[string]any)["$ref"].(string)
			if !declared {
				t.Fatalf("operation %s carries message %s, which has no shared payload schema", name, messageRef)
			}
			carried = append(carried, action+" "+address+" "+strings.TrimPrefix(schema, protocol.SchemaPrefix))
		}
	}

	adopted := make([]string, 0, len(v1Inventory))
	for _, message := range v1Inventory {
		adopted = append(adopted, message.action+" "+message.address+" "+message.schema)
	}
	slices.Sort(carried)
	slices.Sort(adopted)
	if !slices.Equal(carried, adopted) {
		t.Errorf("the shipped description carries %v, want the adopted v1 inventory %v", carried, adopted)
	}
}

// assertChannelMessage requires one message of the adopted inventory to travel
// in one direction of one channel: its shared payload schema, the type a client
// matches on, the channel that carries it and the operation that names it.
func assertChannelMessage(t *testing.T, description map[string]any, address, action, schema, name string) {
	t.Helper()

	channels, _ := description["channels"].(map[string]any)
	channelName, declared := channelNamed(channels, address)
	if !declared {
		t.Fatalf("the shipped WSS description does not declare the channel %s: %v", address, channels)
	}
	channel, _ := channels[channelName].(map[string]any)
	descriptions, _ := description["components"].(map[string]any)["messages"].(map[string]any)
	messageKey := ""
	for key, value := range descriptions {
		message, _ := value.(map[string]any)
		payload, _ := message["payload"].(map[string]any)
		if payload["$ref"] == protocol.SchemaPrefix+schema {
			if message["name"] != name {
				t.Errorf("message %s states the type %v, want %s", key, message["name"], name)
			}
			messageKey = key
		}
	}
	if messageKey == "" {
		t.Fatalf("the description declares no message of the shared %s schema: %v", schema, descriptions)
	}
	messageRef := "#/components/messages/" + messageKey

	// The channel carries it, so a client subscribing to that channel knows the
	// message can arrive there.
	messages, _ := channel["messages"].(map[string]any)
	channelKey := ""
	for key, value := range messages {
		reference, _ := value.(map[string]any)["$ref"].(string)
		if reference == messageRef {
			channelKey = key
		}
	}
	if channelKey == "" {
		t.Fatalf("the channel %s does not carry %s: %v", address, messageRef, messages)
	}

	// The direction it travels in must name it, so a generator reading that
	// operation carries the message instead of inferring it from the channel.
	channelMessageRef := "#/channels/" + channelName + "/messages/" + channelKey
	declaredDirection := false
	operations, _ := description["operations"].(map[string]any)
	for operationName, value := range operations {
		published, _ := value.(map[string]any)
		reference, _ := published["channel"].(map[string]any)["$ref"].(string)
		if published["action"] != action || reference != "#/channels/"+channelName {
			continue
		}
		declaredDirection = true
		named := false
		messageList, _ := published["messages"].([]any)
		for _, message := range messageList {
			referenced, _ := message.(map[string]any)["$ref"].(string)
			if referenced == channelMessageRef {
				named = true
			}
		}
		if !named {
			t.Errorf("operation %s travels on %s without naming %s", operationName, address, channelMessageRef)
		}
	}
	if !declaredDirection {
		t.Errorf("the description declares no %s operation on %s, so %s has no direction", action, address, name)
	}
}

// channelWithAddress reports whether the description declares a channel at one
// served address, so a release cannot document a channel the service does not
// serve or omit one it does.
func channelWithAddress(channels map[string]any, address string) bool {
	_, declared := channelNamed(channels, address)
	return declared
}

// channelNamed returns the declared key of the channel at one served address,
// so a test can follow the references that name its messages.
func channelNamed(channels map[string]any, address string) (string, bool) {
	for name, value := range channels {
		channel, _ := value.(map[string]any)
		if channel["address"] == address {
			return name, true
		}
	}
	return "", false
}
