package http_test

import (
	"slices"
	"strings"
	"testing"
)

// The client contract keeps missing display fields nullable, Account identity
// opaque, U/R authorization explicit, and the lookup free of selected IDs.
func TestPublishedOwnAccountContract(t *testing.T) {
	fixture := readFixture[struct {
		operationContract
		Nullable []string            `json:"nullable"`
		Errors   map[string][]string `json:"errors"`
	}](t, "self_account_v1.json")
	description := published(t)
	operation := operationOf(t, description, fixture.operationContract)
	success := successSchemaOf(t, description, operation)
	if success.name != fixture.Success.Schema || !slices.Equal(requiredFields(t, success.schema), fixture.Success.Required) || !slices.Equal(propertyFields(t, success.schema), fixture.Success.Required) {
		t.Fatalf("Account success schema = %s %v", success.name, success.schema)
	}
	for _, field := range fixture.Nullable {
		property := success.schema["properties"].(map[string]any)[field].(map[string]any)
		kinds, _ := property["type"].([]any)
		if !slices.Contains(kinds, any("null")) || !slices.Contains(kinds, any("string")) {
			t.Errorf("%s must allow a string or null: %v", field, property)
		}
	}
	if parameters, _ := operation["parameters"].([]any); len(parameters) != 0 {
		t.Fatal("Account lookup accepts caller-selected parameters")
	}
	if _, ok := operation["requestBody"]; ok {
		t.Fatal("Account lookup accepts a body")
	}
	security := operation["security"].([]any)
	if len(security) != 1 {
		t.Fatalf("Account security = %v", security)
	}
	if _, ok := security[0].(map[string]any)["bearerAuth"]; !ok {
		t.Fatal("Account lookup does not require Bearer authentication")
	}
	assertFailureStatuses(t, publishedFailureStatuses(t, operation), fixtureStatuses(fixture.Errors))
	text, _ := operation["description"].(string)
	for _, phrase := range []string{"Sender U", "Receiver R", "null", "never call QQ", "issuance grants", "cache age"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("Account authorization/cache policy omits %q", phrase)
		}
	}
}
