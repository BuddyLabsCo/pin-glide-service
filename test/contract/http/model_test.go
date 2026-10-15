package http_test

import (
	"slices"
	"strings"
	"testing"
)

// payloadSchema is one accepted nested payload contract: the schema name a
// client resolves and the exact fields it may rely on being present.
type payloadSchema struct {
	Schema   string   `json:"schema"`
	Required []string `json:"required"`
}

// modelCredentialFixture is the accepted v1 compatibility contract of the
// model credential operation: the success payload with the credential and
// configuration schemas it embeds, and every failure status a released v1
// client handles.
type modelCredentialFixture struct {
	OperationID string `json:"operationId"`
	Path        string `json:"path"`
	Method      string `json:"method"`
	Success     struct {
		Schema   string   `json:"schema"`
		Status   string   `json:"status"`
		Required []string `json:"required"`
	} `json:"success"`
	Credential     payloadSchema       `json:"credential"`
	Configuration  payloadSchema       `json:"configuration"`
	Inference      payloadSchema       `json:"inference"`
	ResponseFormat payloadSchema       `json:"responseFormat"`
	Errors         map[string][]string `json:"errors"`
}

// The published description must keep describing exactly the accepted model
// credential contract: the provider credential and configuration snapshot a
// released v1 client reads, the bearer authentication the operation requires
// and every failure status it reports.
func TestPublishedHTTPDescriptionKeepsTheModelCredentialContract(t *testing.T) {
	fixture := readFixture[modelCredentialFixture](t, "model_credentials_v1.json")
	description := published(t)
	operation := operationOf(t, description, operationContract{
		OperationID: fixture.OperationID,
		Path:        fixture.Path,
		Method:      fixture.Method,
	})

	if body, present := operation["requestBody"]; present {
		t.Errorf("the model credential operation takes no request body, but publishes %v", body)
	}
	security, _ := operation["security"].([]any)
	if len(security) != 1 {
		t.Fatalf("model credential security = %v, want exactly one requirement", operation["security"])
	}
	if requirement, _ := security[0].(map[string]any); len(requirement) != 1 || requirement["bearerAuth"] == nil {
		t.Errorf("model credential security = %v, want the bearer requirement alone", security[0])
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

	assertPayloadSchema(t, description, successSchema, "credential", fixture.Credential)
	configuration := assertPayloadSchema(t, description, successSchema, "configuration", fixture.Configuration)
	inference := assertPayloadSchema(t, description, configuration, "inference", fixture.Inference)
	assertPayloadSchema(t, description, inference, "response_format", fixture.ResponseFormat)

	publishedErrors := map[string]bool{}
	for status := range responses {
		if !strings.HasPrefix(status, "2") {
			publishedErrors[status] = true
		}
	}
	assertFailureStatuses(t, publishedErrors, fixtureStatuses(fixture.Errors))
}

// assertPayloadSchema resolves one property of a published payload and requires
// it to be exactly the accepted schema with the accepted required fields.
func assertPayloadSchema(t *testing.T, description, parent map[string]any, property string, accepted payloadSchema) map[string]any {
	t.Helper()

	properties, ok := parent["properties"].(map[string]any)
	if !ok {
		t.Fatalf("published payload %v has no properties", parent)
	}
	name, schema := schemaOf(t, description, properties[property])
	if name != accepted.Schema {
		t.Errorf("%s schema = %q, want %q", property, name, accepted.Schema)
	}
	if required := requiredFields(t, schema); !slices.Equal(required, accepted.Required) {
		t.Errorf("%s required fields = %v, want %v", property, required, accepted.Required)
	}
	return schema
}
