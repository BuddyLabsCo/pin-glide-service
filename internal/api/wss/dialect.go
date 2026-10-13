package wss

import (
	"fmt"
	"strings"
)

// The shared wire schemas are generated as JSON Schema 2020-12, while the
// published WSS description validates payloads with the AsyncAPI Schema Object
// dialect, a superset of JSON Schema draft-07. convertSchema rewrites one
// shared schema for that dialect and fails on a construct the dialect cannot
// express, because publishing it would let a client validate against a schema
// that silently ignores part of the shared definition.
func convertSchema(value any, path string) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		return convertSchemaObject(typed, path)
	case []any:
		converted := make([]any, len(typed))
		for index, item := range typed {
			result, err := convertSchema(item, fmt.Sprintf("%s/%d", path, index))
			if err != nil {
				return nil, err
			}
			converted[index] = result
		}
		return converted, nil
	default:
		return value, nil
	}
}

// unsupportedKeywords lists JSON Schema keywords added after draft-07 that
// have no equivalent in the AsyncAPI schema dialect.
var unsupportedKeywords = map[string]bool{
	"$anchor":               true,
	"$dynamicAnchor":        true,
	"$dynamicRef":           true,
	"$recursiveAnchor":      true,
	"$recursiveRef":         true,
	"$vocabulary":           true,
	"contentSchema":         true,
	"dependentRequired":     true,
	"dependentSchemas":      true,
	"maxContains":           true,
	"minContains":           true,
	"prefixItems":           true,
	"unevaluatedItems":      true,
	"unevaluatedProperties": true,
}

func convertSchemaObject(schema map[string]any, path string) (map[string]any, error) {
	converted := make(map[string]any, len(schema))
	for key, value := range schema {
		if unsupportedKeywords[key] {
			return nil, fmt.Errorf("shared schema %s uses %q, which the AsyncAPI schema dialect cannot express", path, key)
		}

		switch key {
		case "$schema":
			// Names the source dialect; the AsyncAPI document declares its own.
			continue
		case "$defs":
			// draft-07 names subschema definitions differently.
			key = "definitions"
		case "$ref":
			// Keep a converted reference pointing at the converted definitions.
			if ref, ok := value.(string); ok {
				value = strings.Replace(ref, "#/$defs/", "#/definitions/", 1)
			}
		case "exclusiveMinimum", "exclusiveMaximum":
			// 2020-12 writes the bound itself; draft-07 pairs the boolean with
			// the inclusive bound, so a value the dialect cannot express fails.
			bound, numeric := value.(float64)
			if !numeric {
				break
			}
			inclusive := "minimum"
			if key == "exclusiveMaximum" {
				inclusive = "maximum"
			}
			if _, present := schema[inclusive]; present {
				return nil, fmt.Errorf("shared schema %s mixes %q with %q, which the AsyncAPI schema dialect cannot express", path, key, inclusive)
			}
			converted[inclusive] = bound
			converted[key] = true
			continue
		}

		result, err := convertSchema(value, path+"/"+key)
		if err != nil {
			return nil, err
		}
		converted[key] = result
	}
	return converted, nil
}
