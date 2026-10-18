package http_test

import (
	"slices"
	"strings"
	"testing"
)

// The published description must describe the QQ authorization-code login
// through the same credential bundle and provider-neutral failure statuses.
func TestPublishedHTTPDescriptionKeepsTheQQLoginContract(t *testing.T) {
	fixture := readFixture[loginFixture](t, "qq_login_v1.json")
	publishedErrors := assertOperationContract(t, published(t), fixture.operationContract)
	assertFailureStatuses(t, publishedErrors, fixtureStatuses(fixture.Errors))
}

// Recovery accepts the legacy WeChat proof and the new QQ proof without making
// either field mandatory for every Pairing Session mode.
func TestPublishedHTTPDescriptionIncludesQQRecoveryProof(t *testing.T) {
	description := published(t)
	operation := operationOf(t, description, operationContract{
		OperationID: "PostPairingSessionAuthorize",
		Path:        "/v1/pairing-sessions/{id}/authorize",
		Method:      "post",
	})

	requestBody := operation["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)
	requestName, request := schemaOf(t, description, requestBody["schema"])
	if requestName != "AuthorizeRequest" {
		t.Errorf("request schema = %q, want AuthorizeRequest", requestName)
	}
	if required := requiredFields(t, request); len(required) != 0 {
		t.Errorf("request required fields = %v, want none", required)
	}
	if properties := propertyFields(t, request); !slices.Equal(properties, []string{"alias", "qqCode", "wechatCode"}) {
		t.Errorf("request fields = %v, want [alias qqCode wechatCode]", properties)
	}
	for _, status := range []string{"401", "422", "502"} {
		if _, ok := operation["responses"].(map[string]any)[status]; !ok {
			t.Errorf("Pairing authorization has no %s failure response", status)
		}
	}
	operationDescription, _ := operation["description"].(string)
	for _, phrase := range []string{
		"401 wechat_authorization_invalid or qq_authorization_invalid",
		"403 fresh_identity_required for absent or foreign recovery proof",
		"422 validation_failed for conflicting proof fields",
		"502 identity_provider_error",
	} {
		if !strings.Contains(operationDescription, phrase) {
			t.Errorf("Pairing authorization description does not document %q", phrase)
		}
	}
}
