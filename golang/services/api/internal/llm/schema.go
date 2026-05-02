package llm

import (
	"reflect"
	"strings"
)

// JSONSchema is a minimal JSON Schema object used for structured output.
type JSONSchema struct {
	Type                 string                `json:"type"`
	Properties           map[string]JSONSchema `json:"properties,omitempty"`
	Items                *JSONSchema           `json:"items,omitempty"`
	Description          string                `json:"description,omitempty"`
	Required             []string              `json:"required,omitempty"`
	AdditionalProperties bool                  `json:"additionalProperties,omitempty"`
}

// SchemaOf generates a JSON Schema from a Go struct using `json` and
// `description` struct tags. Supports: string, int, float, bool, slice, struct.
func SchemaOf(v interface{}) JSONSchema {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return schemaOfType(t)
}

func schemaOfType(t reflect.Type) JSONSchema {
	switch t.Kind() {
	case reflect.String:
		return JSONSchema{Type: "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return JSONSchema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return JSONSchema{Type: "number"}
	case reflect.Bool:
		return JSONSchema{Type: "boolean"}
	case reflect.Slice:
		elem := schemaOfType(t.Elem())
		return JSONSchema{Type: "array", Items: &elem}
	case reflect.Map:
		return JSONSchema{Type: "object", AdditionalProperties: true}
	case reflect.Interface:
		return JSONSchema{Type: "object", AdditionalProperties: true}
	case reflect.Struct:
		return schemaOfStruct(t)
	default:
		return JSONSchema{Type: "string"}
	}
}

func schemaOfStruct(t reflect.Type) JSONSchema {
	props := make(map[string]JSONSchema, t.NumField())
	var required []string

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)

		jsonTag := field.Tag.Get("json")
		if jsonTag == "" || jsonTag == "-" {
			continue
		}
		name := strings.Split(jsonTag, ",")[0]
		if name == "" {
			continue
		}

		fieldSchema := schemaOfType(field.Type)
		if desc := field.Tag.Get("description"); desc != "" {
			fieldSchema.Description = desc
		}

		props[name] = fieldSchema
		required = append(required, name)
	}

	return JSONSchema{
		Type:                 "object",
		Properties:           props,
		Required:             required,
		AdditionalProperties: false,
	}
}
