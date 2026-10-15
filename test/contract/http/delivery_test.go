// This file checks the published HTTP description of the Delivery operations
// against the accepted v1 compatibility contract clients generate from.
package http_test

import (
	"slices"
	"strings"
	"testing"
)

// statusContract is the accepted published shape of one indexed result or
// retained metadata view: the schema a client resolves, the exact fields it may
// rely on and the status values it must be able to handle.
type statusContract struct {
	Schema     string   `json:"schema"`
	Required   []string `json:"required"`
	Properties []string `json:"properties"`
	Statuses   []string `json:"statuses"`
}

// metadataContract is the accepted published shape of the retained metadata
// query of one Message.
type metadataContract struct {
	operationContract
	View   statusContract      `json:"view"`
	Errors map[string][]string `json:"errors"`
}

// deliveryFixture is the accepted v1 compatibility contract of Delivery
// admission: the upload envelope and its entries, the 207 result tree with the
// shared business error, the retained metadata query of one Message and every
// failure status a released v1 client handles.
type deliveryFixture struct {
	operationContract
	Item          statusContract      `json:"item"`
	Result        statusContract      `json:"result"`
	BusinessError statusContract      `json:"businessError"`
	Errors        map[string][]string `json:"errors"`
	Metadata      metadataContract    `json:"metadata"`
}

// The published description must keep describing exactly the accepted Delivery
// contract: the documented upload envelope, the terminal status of every
// indexed result, the shared business error of failed entries, every failure
// status a released v1 client handles and the metadata query of one Message.
func TestPublishedHTTPDescriptionKeepsTheDeliveryContract(t *testing.T) {
	fixture := readFixture[deliveryFixture](t, "messages_v1.json")
	description := published(t)
	upload := operationOf(t, description, fixture.operationContract)
	responses := upload["responses"].(map[string]any)

	body := responses[fixture.Success.Status].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
	successName, success := schemaOf(t, description, body["schema"])
	if successName != fixture.Success.Schema {
		t.Errorf("upload success schema = %q, want %q", successName, fixture.Success.Schema)
	}
	if required := requiredFields(t, success); !slices.Equal(required, fixture.Success.Required) {
		t.Errorf("upload success required fields = %v, want %v", required, fixture.Success.Required)
	}

	requestBody := upload["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
	requestName, request := schemaOf(t, description, requestBody["schema"])
	if requestName != fixture.Request.Schema {
		t.Errorf("upload request schema = %q, want %q", requestName, fixture.Request.Schema)
	}
	if required := requiredFields(t, request); !slices.Equal(required, fixture.Request.Required) {
		t.Errorf("upload request required fields = %v, want %v", required, fixture.Request.Required)
	}
	if properties := propertyFields(t, request); !slices.Equal(properties, fixture.Request.Properties) {
		t.Errorf("upload request fields = %v, want %v", properties, fixture.Request.Properties)
	}

	entry := request["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)
	entryName, entrySchema := schemaOf(t, description, entry)
	assertStatusContract(t, "upload entry", entryName, entrySchema, fixture.Item)

	result := success["properties"].(map[string]any)["results"].(map[string]any)["items"].(map[string]any)
	resultName, resultSchema := schemaOf(t, description, result)
	assertStatusContract(t, "upload result", resultName, resultSchema, fixture.Result)

	failureName, failure := schemaOf(t, description, resultSchema["properties"].(map[string]any)["error"])
	assertStatusContract(t, "business error", failureName, failure, fixture.BusinessError)

	publishedErrors := map[string]bool{}
	for status := range responses {
		if !strings.HasPrefix(status, "2") {
			publishedErrors[status] = true
		}
	}
	assertFailureStatuses(t, publishedErrors, fixtureStatuses(fixture.Errors))

	metadata := operationOf(t, description, fixture.Metadata.operationContract)
	metadataResponses := metadata["responses"].(map[string]any)
	metadataBody := metadataResponses[fixture.Metadata.Success.Status].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
	metadataName, metadataSuccess := schemaOf(t, description, metadataBody["schema"])
	if metadataName != fixture.Metadata.Success.Schema {
		t.Errorf("metadata success schema = %q, want %q", metadataName, fixture.Metadata.Success.Schema)
	}
	if required := requiredFields(t, metadataSuccess); !slices.Equal(required, fixture.Metadata.Success.Required) {
		t.Errorf("metadata required fields = %v, want %v", required, fixture.Metadata.Success.Required)
	}

	views := metadataSuccess["properties"].(map[string]any)["deliveries"].(map[string]any)["items"].(map[string]any)
	viewName, view := schemaOf(t, description, views)
	assertStatusContract(t, "delivery view", viewName, view, fixture.Metadata.View)

	metadataErrors := map[string]bool{}
	for status := range metadataResponses {
		if !strings.HasPrefix(status, "2") {
			metadataErrors[status] = true
		}
	}
	assertFailureStatuses(t, metadataErrors, fixtureStatuses(fixture.Metadata.Errors))
}

// assertStatusContract requires one published schema to keep exactly the
// accepted name, fields and status values.
func assertStatusContract(t *testing.T, what, name string, schema map[string]any, accepted statusContract) {
	t.Helper()

	if name != accepted.Schema {
		t.Errorf("%s schema = %q, want %q", what, name, accepted.Schema)
	}
	if required := requiredFields(t, schema); !slices.Equal(required, accepted.Required) {
		t.Errorf("%s required fields = %v, want %v", what, required, accepted.Required)
	}
	if properties := propertyFields(t, schema); !slices.Equal(properties, accepted.Properties) {
		t.Errorf("%s fields = %v, want %v", what, properties, accepted.Properties)
	}

	status, _ := schema["properties"].(map[string]any)["status"].(map[string]any)
	values, _ := status["enum"].([]any)
	statuses := make([]string, 0, len(values))
	for _, value := range values {
		statuses = append(statuses, value.(string))
	}
	slices.Sort(statuses)
	if !slices.Equal(statuses, accepted.Statuses) {
		t.Errorf("%s statuses = %v, want %v", what, statuses, accepted.Statuses)
	}
}
