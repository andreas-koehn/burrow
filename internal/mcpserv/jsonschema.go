package mcpserv

import (
	"fmt"
	"strings"
)

const schemaRefPrefix = "#/components/schemas/"

// keptKeywords pass from OpenAPI 3.0 to JSON Schema unchanged.
var keptKeywords = []string{
	"type", "format", "enum", "const", "pattern", "minLength", "maxLength",
	"minimum", "maximum", "minItems", "maxItems", "uniqueItems", "minProperties",
	"maxProperties", "multipleOf", "default", "title", "description", "required", "deprecated",
}

// toJSONSchema turns an OpenAPI 3.0 schema into JSON Schema with every
// reference to components/schemas inlined. schemas is components.schemas.
func toJSONSchema(node map[string]any, schemas map[string]any) (map[string]any, error) {
	return convertSchema(node, schemas, map[string]bool{})
}

func convertSchema(node map[string]any, schemas map[string]any, visiting map[string]bool) (map[string]any, error) {
	if ref, ok := node["$ref"].(string); ok {
		if !strings.HasPrefix(ref, schemaRefPrefix) {
			return nil, fmt.Errorf("unsupported $ref %q", ref)
		}
		name := strings.TrimPrefix(ref, schemaRefPrefix)
		target, ok := schemas[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$ref %q: no such schema", ref)
		}
		if visiting[name] {
			return map[string]any{"type": "object", "description": name + " (recursive, not expanded)"}, nil
		}
		visiting[name] = true
		defer delete(visiting, name)
		return convertSchema(target, schemas, visiting)
	}

	out := map[string]any{}
	for _, k := range keptKeywords {
		if v, ok := node[k]; ok {
			out[k] = v
		}
	}
	if node["nullable"] == true {
		if typ, ok := out["type"]; ok {
			out["type"] = []any{typ, "null"}
		}
	}
	for _, bound := range []struct{ excl, plain string }{{"exclusiveMinimum", "minimum"}, {"exclusiveMaximum", "maximum"}} {
		if node[bound.excl] == true {
			if v, ok := out[bound.plain]; ok {
				out[bound.excl] = v
				delete(out, bound.plain)
			}
		}
	}
	if props, ok := node["properties"].(map[string]any); ok {
		conv := make(map[string]any, len(props))
		for name, p := range props {
			pm, ok := p.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("property %q is not a schema", name)
			}
			c, err := convertSchema(pm, schemas, visiting)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", name, err)
			}
			conv[name] = c
		}
		out["properties"] = conv
	}
	for _, k := range []string{"items", "not"} {
		if sub, ok := node[k].(map[string]any); ok {
			c, err := convertSchema(sub, schemas, visiting)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = c
		}
	}
	switch ap := node["additionalProperties"].(type) {
	case bool:
		out["additionalProperties"] = ap
	case map[string]any:
		c, err := convertSchema(ap, schemas, visiting)
		if err != nil {
			return nil, fmt.Errorf("additionalProperties: %w", err)
		}
		out["additionalProperties"] = c
	}
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		list, ok := node[k].([]any)
		if !ok {
			continue
		}
		conv := make([]any, 0, len(list))
		for i, item := range list {
			im, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s[%d] is not a schema", k, i)
			}
			c, err := convertSchema(im, schemas, visiting)
			if err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", k, i, err)
			}
			conv = append(conv, c)
		}
		out[k] = conv
	}
	return out, nil
}
