package main

import (
	"encoding/json"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// structuredNullShape describes only null object properties introduced by
// normalizeSchemaForOpenAI. A nil shape means that no cleanup is permitted.
// Array elements are never removed; items is used only to inspect objects
// contained in an array.
type structuredNullShape struct {
	properties map[string]structuredNullProperty
	items      *structuredNullShape
}

type structuredNullProperty struct {
	drop  bool
	child *structuredNullShape
}

// structuredNullShapeFromSchema derives cleanup permissions from the original
// client schema. It deliberately does not guess through combinators or $ref:
// a null is removed only when the schema proves that the property was optional
// and did not already permit null.
func structuredNullShapeFromSchema(raw string) *structuredNullShape {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	return buildStructuredNullShape(v)
}

func buildStructuredNullShape(v any) *structuredNullShape {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}

	shape := &structuredNullShape{}
	if props, ok := m["properties"].(map[string]any); ok {
		shape.properties = make(map[string]structuredNullProperty)
		required := schemaRequired(m["required"])
		for name, sub := range props {
			child := buildStructuredNullShape(sub)
			shape.properties[name] = structuredNullProperty{
				drop:  !required[name] && !schemaAllowsNull(sub),
				child: child,
			}
		}
	}
	if items, ok := m["items"]; ok {
		shape.items = buildStructuredNullShape(items)
	}
	if len(shape.properties) == 0 && shape.items == nil {
		return nil
	}
	return shape
}

func schemaRequired(v any) map[string]bool {
	out := map[string]bool{}
	if arr, ok := v.([]any); ok {
		for _, item := range arr {
			if name, ok := item.(string); ok {
				out[name] = true
			}
		}
	}
	return out
}

func schemaAllowsNull(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if typ, ok := m["type"].(string); ok && typ == "null" {
		return true
	}
	if types, ok := m["type"].([]any); ok {
		for _, typ := range types {
			if typ == "null" {
				return true
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if branches, ok := m[key].([]any); ok {
			for _, branch := range branches {
				if schemaAllowsNull(branch) {
					return true
				}
			}
		}
	}
	return false
}

func cleanStructuredJSONWithShape(s string, shape *structuredNullShape) string {
	if shape == nil {
		return s
	}
	var value any
	if err := json.Unmarshal([]byte(s), &value); err != nil {
		return s
	}
	cleaned, changed := cleanStructuredValue(value, shape)
	if !changed {
		return s
	}
	out, err := json.Marshal(cleaned)
	if err != nil {
		return s
	}
	return string(out)
}

// stripNullFieldsWithShape cleans text content using schema-derived permissions.
func stripNullFieldsWithShape(body []byte, shape *structuredNullShape) []byte {
	if shape == nil {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return body
	}
	out, changed := body, false
	content.ForEach(func(idx, block gjson.Result) bool {
		if block.Get("type").String() != "text" {
			return true
		}
		text := block.Get("text").String()
		cleaned := cleanStructuredJSONWithShape(text, shape)
		if cleaned == text {
			return true
		}
		path := "content." + strconv.Itoa(int(idx.Int())) + ".text"
		if next, err := sjson.SetBytes(out, path, cleaned); err == nil {
			out, changed = next, true
		}
		return true
	})
	if !changed {
		return body
	}
	return out
}

func cleanStructuredValue(v any, shape *structuredNullShape) (any, bool) {
	if shape == nil {
		return v, false
	}
	switch value := v.(type) {
	case map[string]any:
		changed := false
		for name, childValue := range value {
			rule, known := shape.properties[name]
			if childValue == nil && known && rule.drop {
				delete(value, name)
				changed = true
				continue
			}
			if known && rule.child != nil {
				cleaned, childChanged := cleanStructuredValue(childValue, rule.child)
				value[name] = cleaned
				changed = changed || childChanged
			}
		}
		return value, changed
	case []any:
		if shape.items == nil {
			return value, false
		}
		changed := false
		for i, item := range value {
			cleaned, itemChanged := cleanStructuredValue(item, shape.items)
			value[i] = cleaned
			changed = changed || itemChanged
		}
		return value, changed
	default:
		return v, false
	}
}
