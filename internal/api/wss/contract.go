// Package wss owns the Pin Glide WebSocket surface: the published description
// of the connections the service offers, and later the upgrades, frames and
// connection lifecycle that description promises.
package wss

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	"github.com/goccy/go-yaml"
)

// DefinitionsPath is the handwritten WSS definitions relative to the
// repository root. It owns what only a human can state: channels, messages,
// authentication and Close semantics. Payload schemas of shared values come
// from internal/protocol instead of being written again here.
const DefinitionsPath = "protocol/asyncapi.yaml"

// descriptionHeader marks the published artifact as generated, so a maintainer
// changes the definitions instead of the checked-in YAML.
const descriptionHeader = "# Generated from protocol/asyncapi.yaml and internal/protocol; do not edit.\n"

// WriteAsyncAPI writes the published WSS description to writer as YAML,
// assembled from DefinitionsPath and the canonical shared wire schemas. It
// reads no service configuration, opens no listener and contacts no provider.
func WriteAsyncAPI(writer io.Writer) error {
	definitions, err := os.ReadFile(DefinitionsPath)
	if err != nil {
		return fmt.Errorf("read WSS definitions %s (run from the repository root): %w", DefinitionsPath, err)
	}

	schemas, err := protocol.WireSchemas()
	if err != nil {
		return err
	}

	assembled, err := Assemble(definitions, schemas)
	if err != nil {
		return err
	}

	if _, err := writer.Write(assembled); err != nil {
		return fmt.Errorf("write WSS description: %w", err)
	}
	return nil
}

// Assemble builds the published description from the explicit definitions and
// the canonical shared schemas. Each referenced shared schema is embedded,
// together with the schemas it references, so the result stays self-contained;
// a reference without a shared schema fails instead of publishing a
// description that a client cannot resolve.
func Assemble(definitions []byte, shared map[string]any) ([]byte, error) {
	document := map[string]any{}
	if err := yaml.Unmarshal(definitions, &document); err != nil {
		return nil, fmt.Errorf("parse WSS definitions: %w", err)
	}
	if len(document) == 0 {
		return nil, errors.New("WSS definitions are empty")
	}

	components, err := componentsOf(document)
	if err != nil {
		return nil, err
	}
	if _, defined := components["schemas"]; defined {
		return nil, errors.New("WSS definitions must not define payload schemas: they are generated from internal/protocol")
	}
	addCloseCodes(document)

	embedded, err := embedSchemas(document, shared)
	if err != nil {
		return nil, err
	}
	for name, schema := range embedded {
		converted, err := convertSchema(schema, name)
		if err != nil {
			return nil, err
		}
		object, ok := converted.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("shared schema %q is not an object", name)
		}
		addErrorCodes(name, object)
		embedded[name] = object
	}
	components["schemas"] = embedded

	assembled, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode WSS description: %w", err)
	}
	return append([]byte(descriptionHeader), assembled...), nil
}

// addCloseCodes states every channel's Close meanings from internal/protocol,
// so the published description and the connections that close carry one
// authority instead of a repeated table. Close belongs to a channel, so each
// declared channel receives the complete documented set.
func addCloseCodes(document map[string]any) {
	channels, _ := document["channels"].(map[string]any)
	if len(channels) == 0 {
		return
	}

	codes := make([]any, 0, len(protocol.CloseCodes))
	for _, close := range protocol.CloseCodes {
		codes = append(codes, map[string]any{
			"code":        close.Code,
			"reason":      close.Reason,
			"description": close.Description,
		})
	}
	for _, value := range channels {
		channel, ok := value.(map[string]any)
		if !ok {
			continue
		}
		channel["x-close-codes"] = codes
	}
}

// addErrorCodes states the business codes the error message carries, so a
// client reads the accepted set from the description rather than inferring it
// from an open string field.
func addErrorCodes(name string, schema map[string]any) {
	if name != "WSSError" {
		return
	}
	codes := make([]any, 0, len(protocol.WSSCodes))
	for _, code := range protocol.WSSCodes {
		codes = append(codes, map[string]any{
			"code":        string(code.Code),
			"description": code.Description,
		})
	}
	schema["x-error-codes"] = codes
}

// componentsOf returns the document's components mapping, creating an empty one
// so every assembled description has the same shape.
func componentsOf(document map[string]any) (map[string]any, error) {
	value, exists := document["components"]
	if !exists {
		components := map[string]any{}
		document["components"] = components
		return components, nil
	}
	components, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("WSS definitions carry a components value that is not a mapping")
	}
	return components, nil
}

// embedSchemas resolves every shared schema the document references, following
// the references of the embedded schemas as well.
func embedSchemas(document any, shared map[string]any) (map[string]any, error) {
	embedded := map[string]any{}
	pending, err := sharedSchemaNames(document)
	if err != nil {
		return nil, err
	}

	for len(pending) > 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, done := embedded[name]; done {
			continue
		}

		schema, defined := shared[name]
		if !defined {
			return nil, fmt.Errorf("reference %s%s has no shared wire schema in internal/protocol", protocol.SchemaPrefix, name)
		}
		embedded[name] = schema

		referenced, err := sharedSchemaNames(schema)
		if err != nil {
			return nil, err
		}
		pending = append(pending, referenced...)
	}
	return embedded, nil
}

// sharedSchemaNames collects the shared schema names value references, and
// rejects a reference the published description could not resolve by itself.
func sharedSchemaNames(value any) ([]string, error) {
	var names []string
	err := collectRefs(value, func(ref string) error {
		if !strings.HasPrefix(ref, "#") {
			return fmt.Errorf("reference %q leaves the description; the published description must be self-contained", ref)
		}
		if name, ok := strings.CutPrefix(ref, protocol.SchemaPrefix); ok {
			names = append(names, name)
		}
		return nil
	})
	return names, err
}

// collectRefs visits the string value of every $ref in value.
func collectRefs(value any, visit func(string) error) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key != "$ref" {
				if err := collectRefs(item, visit); err != nil {
					return err
				}
				continue
			}
			ref, ok := item.(string)
			if !ok {
				return errors.New("a $ref value is not a string")
			}
			if err := visit(ref); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := collectRefs(item, visit); err != nil {
				return err
			}
		}
	}
	return nil
}
