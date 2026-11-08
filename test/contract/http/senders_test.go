package http_test

import (
	"fmt"
	"slices"
	"testing"
)

// Generated clients need the complete named-Sender contract, including nullable
// revocation time and M-only authentication, without any credential fields.
func TestPublishedHTTPDescriptionKeepsSenderInventoryContract(t *testing.T) {
	fixture := readFixture[struct {
		operationContract
		Item struct {
			Schema   string   `json:"schema"`
			Required []string `json:"required"`
		} `json:"item"`
		Errors map[string][]string `json:"errors"`
	}](t, "senders_v1.json")
	description := published(t)
	operation := operationOf(t, description, fixture.operationContract)
	success := successSchemaOf(t, description, operation)
	if success.name != fixture.Success.Schema {
		t.Errorf("success=%s", success.name)
	}
	if !slices.Equal(requiredFields(t, success.schema), fixture.Success.Required) || !slices.Equal(propertyFields(t, success.schema), fixture.Success.Required) {
		t.Errorf("list fields=%v", success.schema)
	}
	items := success.schema["properties"].(map[string]any)["items"].(map[string]any)
	assertArrayType(t, "Sender inventory", items)
	name, item := schemaOf(t, description, items["items"])
	if name != fixture.Item.Schema || !slices.Equal(requiredFields(t, item), fixture.Item.Required) || !slices.Equal(propertyFields(t, item), fixture.Item.Required) {
		t.Errorf("item schema=%s %v", name, item)
	}
	properties := item["properties"].(map[string]any)
	deviceName := properties["deviceName"].(map[string]any)
	if fmt.Sprint(deviceName["minLength"]) != "1" || fmt.Sprint(deviceName["maxLength"]) != "64" {
		t.Errorf("device name limits=%v", deviceName)
	}
	revokedAt := properties["revokedAt"].(map[string]any)
	types, _ := revokedAt["type"].([]any)
	if !slices.Contains(types, any("null")) {
		t.Errorf("revokedAt must allow null: %v", revokedAt)
	}
	security, _ := operation["security"].([]any)
	if len(security) != 1 || security[0].(map[string]any)["bearerAuth"] == nil {
		t.Errorf("security=%v", security)
	}
	if parameters, _ := operation["parameters"].([]any); len(parameters) != 0 {
		t.Errorf("unexpected inventory selectors: %v", parameters)
	}
	assertFailureStatuses(t, publishedFailureStatuses(t, operation), fixtureStatuses(fixture.Errors))
}
