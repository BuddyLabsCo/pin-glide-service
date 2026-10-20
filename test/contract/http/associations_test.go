// This file checks the published HTTP description of the Sender's own
// association operations against the contract rules those operations must
// satisfy.
package http_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
)

// The Sender's own association operations are part of the published v1
// contract: a released client reads the exact routes, request and success
// fields, the presence values and the failure statuses from the description,
// so a rename or a dropped field fails here instead of reaching that client.
func TestPublishedHTTPDescriptionKeepsTheOwnAssociationContract(t *testing.T) {
	description := published(t)

	list := operationOf(t, description, operationContract{
		OperationID: "GetSelfReceivers",
		Path:        "/v1/self/receivers",
		Method:      "get",
	})
	listSuccess := successSchemaOf(t, description, list)
	if listSuccess.name != "ReceiverList" {
		t.Errorf("list success schema = %q, want ReceiverList", listSuccess.name)
	}
	if required := requiredFields(t, listSuccess.schema); !slices.Equal(required, []string{"items"}) {
		t.Errorf("list required fields = %v, want [items]", required)
	}
	if properties := propertyFields(t, listSuccess.schema); !slices.Equal(properties, []string{"items"}) {
		t.Errorf("list fields = %v, want [items]", properties)
	}
	items, _ := listSuccess.schema["properties"].(map[string]any)["items"].(map[string]any)
	assertArrayType(t, "list items", items)
	element, _ := items["items"].(map[string]any)
	if ref, want := element["$ref"], protocol.SchemaPrefix+"ReceiverAssociation"; ref != want {
		t.Errorf("list items reference %v, want %s", ref, want)
	}
	assertFailureStatuses(t, publishedFailureStatuses(t, list), []string{"401", "500", "503"})

	rename := operationOf(t, description, operationContract{
		OperationID: "PatchSelfPairing",
		Path:        "/v1/self/pairings/{id}",
		Method:      "patch",
	})
	assertAssociationSuccess(t, description, rename)
	requestBody, ok := rename["requestBody"].(map[string]any)
	if !ok {
		t.Fatalf("the rename operation has no request body: %v", rename)
	}
	if required, _ := requestBody["required"].(bool); !required {
		t.Error("the rename request body is optional, but its accepted contract requires an alias")
	}
	content, _ := requestBody["content"].(map[string]any)["application/json"].(map[string]any)
	requestName, request := schemaOf(t, description, content["schema"])
	if requestName != "PairingAlias" {
		t.Errorf("rename request schema = %q, want PairingAlias", requestName)
	}
	if required := requiredFields(t, request); !slices.Equal(required, []string{"alias"}) {
		t.Errorf("rename required fields = %v, want [alias]", required)
	}
	if properties := propertyFields(t, request); !slices.Equal(properties, []string{"alias"}) {
		t.Errorf("rename fields = %v, want [alias]", properties)
	}
	assertFailureStatuses(t, publishedFailureStatuses(t, rename), []string{"401", "404", "422", "500", "503"})

	remove := operationOf(t, description, operationContract{
		OperationID: "DeleteSelfPairing",
		Path:        "/v1/self/pairings/{id}",
		Method:      "delete",
	})
	responses, _ := remove["responses"].(map[string]any)
	success, ok := responses["204"].(map[string]any)
	if !ok {
		t.Fatalf("the published description does not document the bodyless removal success: %v", responses)
	}
	if content, present := success["content"]; present {
		t.Errorf("the bodyless removal success carries content: %v", content)
	}
	assertFailureStatuses(t, publishedFailureStatuses(t, remove), []string{"401", "404", "422", "500", "503"})
}

// associationSuccess is one resolved success payload of an association
// operation.
type associationSuccess struct {
	name   string
	schema map[string]any
}

// successSchemaOf resolves one operation's 200 payload.
func successSchemaOf(t *testing.T, description, operation map[string]any) associationSuccess {
	t.Helper()

	responses := operation["responses"].(map[string]any)
	success, ok := responses["200"].(map[string]any)
	if !ok {
		t.Fatalf("the published operation does not document a 200 success: %v", responses)
	}
	content, ok := success["content"].(map[string]any)["application/json"].(map[string]any)
	if !ok {
		t.Fatalf("the published success carries no JSON payload: %v", success)
	}
	name, schema := schemaOf(t, description, content["schema"])
	return associationSuccess{name: name, schema: schema}
}

// assertAssociationSuccess requires one operation's success to be the shared
// ReceiverAssociation view with exactly the accepted fields and presence
// values.
func assertAssociationSuccess(t *testing.T, description, operation map[string]any) {
	t.Helper()

	success := successSchemaOf(t, description, operation)
	if success.name != "ReceiverAssociation" {
		t.Errorf("success schema = %q, want ReceiverAssociation", success.name)
	}
	fields := []string{"alias", "pairingId", "presence", "receiverId", "stateChangedAt"}
	if required := requiredFields(t, success.schema); !slices.Equal(required, fields) {
		t.Errorf("success required fields = %v, want %v", required, fields)
	}
	if properties := propertyFields(t, success.schema); !slices.Equal(properties, fields) {
		t.Errorf("success fields = %v, want %v", properties, fields)
	}
	presence, _ := success.schema["properties"].(map[string]any)["presence"].(map[string]any)
	values, _ := presence["enum"].([]any)
	accepted := make([]string, 0, len(values))
	for _, value := range values {
		accepted = append(accepted, fmt.Sprint(value))
	}
	if !slices.Equal(accepted, []string{"online", "offline"}) {
		t.Errorf("presence values = %v, want [online offline]", accepted)
	}
}

// publishedFailureStatuses returns the failure statuses one published
// operation documents.
func publishedFailureStatuses(t *testing.T, operation map[string]any) map[string]bool {
	t.Helper()

	responses, ok := operation["responses"].(map[string]any)
	if !ok {
		t.Fatalf("the published operation has no responses: %v", operation)
	}
	statuses := map[string]bool{}
	for status := range responses {
		if !strings.HasPrefix(status, "2") {
			statuses[status] = true
		}
	}
	return statuses
}
