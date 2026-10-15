package mcpserv

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func yamlMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestToJSONSchema(t *testing.T) {
	schemas := yamlMap(t, `
Pet:
  type: object
  required: [name]
  properties:
    name: { type: string, minLength: 1, example: Rex }
    age: { type: integer, minimum: 0, exclusiveMinimum: true }
    owner: { type: string, nullable: true }
    friend: { $ref: '#/components/schemas/Pet' }
    tags: { type: array, items: { type: string }, x-internal: true }
`)
	got, err := toJSONSchema(map[string]any{"$ref": "#/components/schemas/Pet"}, schemas)
	if err != nil {
		t.Fatal(err)
	}
	s := jsonOf(t, got)
	for _, want := range []string{
		`"required":["name"]`,
		`"name":{"minLength":1,"type":"string"}`,
		`"age":{"exclusiveMinimum":0,"type":"integer"}`,
		`"owner":{"type":["string","null"]}`,
		`"friend":{"description":"Pet (recursive, not expanded)","type":"object"}`,
		`"tags":{"items":{"type":"string"},"type":"array"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("converted schema lacks %s\n%s", want, s)
		}
	}
	for _, gone := range []string{"example", "x-internal", "nullable", `"minimum"`} {
		if strings.Contains(s, gone) {
			t.Errorf("converted schema still has %s\n%s", gone, s)
		}
	}

	if _, err := toJSONSchema(map[string]any{"$ref": "#/components/schemas/Nope"}, schemas); err == nil {
		t.Error("a ref to a missing schema converted")
	}
	if _, err := toJSONSchema(map[string]any{"$ref": "#/components/responses/Unauthorized"}, schemas); err == nil {
		t.Error("a ref outside components/schemas converted")
	}
	allOf, err := toJSONSchema(yamlMap(t, `allOf: [{ $ref: '#/components/schemas/Pet' }, { type: object, properties: { x: { type: boolean } } }]`), schemas)
	if err != nil || !strings.Contains(jsonOf(t, allOf), `"x":{"type":"boolean"}`) {
		t.Errorf("allOf: %v %s", err, jsonOf(t, allOf))
	}
}
