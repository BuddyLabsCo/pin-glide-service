package protocol

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// SchemaPrefix is the reference prefix both published descriptions use for the
// shared wire schemas, so a shared outcome has one name across HTTP and WSS.
const SchemaPrefix = "#/components/schemas/"

// wireTypes lists the shared wire types embedded in the published
// descriptions. A schema name derived from a Go type name is part of the
// client-visible contract; changing it changes the published descriptions.
var wireTypes = []reflect.Type{
	reflect.TypeOf(ErrorItem{}),
	reflect.TypeOf(PairingSessionView{}),
	reflect.TypeOf(ReceiverAssociation{}),
	reflect.TypeOf(PairingState{}),
	reflect.TypeOf(CredentialReady{}),
	reflect.TypeOf(PairingRemoved{}),
	reflect.TypeOf(DeviceRevoked{}),
	reflect.TypeOf(PairingRelayIn{}),
	reflect.TypeOf(PairingRelayOut{}),
	reflect.TypeOf(WSSError{}),
}

// WireSchemas returns the canonical JSON Schema of every shared wire type,
// keyed by the name clients see in the published descriptions. Huma derives
// each schema from the Go type, so the wire types stay the single authority
// and a shared schema cannot drift from the value the service sends.
func WireSchemas() (map[string]any, error) {
	registry := huma.NewMapRegistry(SchemaPrefix, huma.DefaultSchemaNamer)
	for _, wireType := range wireTypes {
		registry.Schema(wireType, true, "")
	}

	encoded, err := json.Marshal(registry)
	if err != nil {
		return nil, fmt.Errorf("encode shared wire schemas: %w", err)
	}

	var schemas map[string]any
	if err := json.Unmarshal(encoded, &schemas); err != nil {
		return nil, fmt.Errorf("decode shared wire schemas: %w", err)
	}
	return schemas, nil
}
