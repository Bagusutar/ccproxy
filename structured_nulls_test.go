package main

import (
	"encoding/json"
	"testing"
)

func TestStructuredNullShapePreservesRequiredNullable(t *testing.T) {
	schema := `{"type":"object","properties":{"optional":{"type":"boolean"},"requiredNullable":{"type":["string","null"]},"required":{"type":"string"}},"required":["requiredNullable","required"]}`
	shape := structuredNullShapeFromSchema(schema)
	got := cleanStructuredJSONWithShape(`{"optional":null,"requiredNullable":null,"required":null}`, shape)
	var value map[string]any
	if err := json.Unmarshal([]byte(got), &value); err != nil {
		t.Fatal(err)
	}
	if _, ok := value["optional"]; ok {
		t.Fatal("optional null was not removed")
	}
	if _, ok := value["requiredNullable"]; !ok {
		t.Fatal("required nullable null was removed")
	}
	if _, ok := value["required"]; !ok {
		t.Fatal("required null was removed")
	}
}

func TestStructuredNullShapeNestedAndArray(t *testing.T) {
	schema := `{"type":"object","properties":{"nested":{"type":"object","properties":{"optional":{"type":"integer"},"requiredNullable":{"type":"null"}},"required":["requiredNullable"]},"items":{"type":"array","items":{"type":"object","properties":{"optional":{"type":"string"},"requiredNullable":{"type":["number","null"]}},"required":["requiredNullable"]}}},"required":["nested","items"]}`
	shape := structuredNullShapeFromSchema(schema)
	got := cleanStructuredJSONWithShape(`{"nested":{"optional":null,"requiredNullable":null},"items":[{"optional":null,"requiredNullable":null},null]}`, shape)
	var value map[string]any
	if err := json.Unmarshal([]byte(got), &value); err != nil {
		t.Fatal(err)
	}
	nested := value["nested"].(map[string]any)
	if _, ok := nested["optional"]; ok {
		t.Fatal("nested optional null was not removed")
	}
	if _, ok := nested["requiredNullable"]; !ok {
		t.Fatal("nested required nullable null was removed")
	}
	items := value["items"].([]any)
	item := items[0].(map[string]any)
	if _, ok := item["optional"]; ok {
		t.Fatal("array item optional null was not removed")
	}
	if _, ok := item["requiredNullable"]; !ok {
		t.Fatal("array item required nullable null was removed")
	}
	if items[1] != nil {
		t.Fatal("array null element was removed or changed")
	}
}

func TestStructuredNullShapeNilIsNoop(t *testing.T) {
	const input = `{"optional":null}`
	if got := cleanStructuredJSONWithShape(input, nil); got != input {
		t.Fatalf("nil shape changed output: %s", got)
	}
}
