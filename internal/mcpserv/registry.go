package mcpserv

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/ankoehn/burrow/internal/authz"
	"gopkg.in/yaml.v3"
)

// Access is an operation's x-burrow-permission.
type Access struct {
	Kind  string             // "public" | "self" | "needs" | "dashboard"
	AnyOf []authz.Permission // empty for public and self
	Every bool
}

// Param is one path or query parameter.
type Param struct {
	Name, In, Description string // In: "path" | "query"
	Required              bool
	Schema                map[string]any // JSON Schema
}

// Operation is one operation of the spec that an MCP client may call.
type Operation struct {
	ID, Method, Path, Summary, Description string // Method upper case; Path as in the spec
	Tags                                   []string
	Params                                 []Param        // path parameters in path order, then query parameters in spec order
	Body                                   map[string]any // JSON Schema of the application/json body; nil without one
	BodyRequired                           bool
	Access                                 Access
	Destructive                            bool // DELETE, or x-burrow-destructive: true
}

// Kind is "read" for GET, "delete" when Destructive, else "write".
func (o Operation) Kind() string {
	switch {
	case o.Destructive:
		return "delete"
	case o.Method == http.MethodGet:
		return "read"
	default:
		return "write"
	}
}

// ConfirmValueName is the last path parameter's name, or "" (then the
// operation id is the value to confirm with).
func (o Operation) ConfirmValueName() string {
	for i := len(o.Params) - 1; i >= 0; i-- {
		if o.Params[i].In == "path" {
			return o.Params[i].Name
		}
	}
	return ""
}

// InputSchema is the JSON Schema of the tool's arguments. It is built fresh
// on every call.
func (o Operation) InputSchema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range o.Params {
		s := deepCopyMap(p.Schema)
		if _, has := s["description"]; !has && p.Description != "" {
			s["description"] = p.Description
		}
		props[p.Name] = s
		if p.Required {
			required = append(required, p.Name)
		}
	}
	if o.Body != nil {
		props["body"] = deepCopyMap(o.Body)
		if o.BodyRequired {
			required = append(required, "body")
		}
	}
	if o.Method != http.MethodGet {
		props["dry_run"] = map[string]any{
			"type":        "boolean",
			"description": "Check the arguments and the token's permission and show the request that would be sent, without sending it.",
		}
	}
	if o.Destructive {
		what := "the text " + o.ID
		if n := o.ConfirmValueName(); n != "" {
			what = "the value of " + n
		}
		props["confirm"] = map[string]any{
			"type":        "string",
			"description": "Required. " + strings.ToUpper(what[:1]) + what[1:] + " to confirm this removes something.",
		}
		required = append(required, "confirm")
	}
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// ToolName puts "_" before every upper-case letter and lower-cases.
func ToolName(operationID string) string {
	var b strings.Builder
	for _, r := range operationID {
		if unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// Registry is the set of callable operations of one spec document.
type Registry struct {
	ops  []Operation
	byID map[string]Operation
}

// Operations lists the operations in spec order: by path, then get, post,
// put, patch, delete.
func (r *Registry) Operations() []Operation { return append([]Operation(nil), r.ops...) }

// Lookup finds an operation by its operationId.
func (r *Registry) Lookup(id string) (Operation, bool) {
	op, ok := r.byID[id]
	return op, ok
}

var (
	methodOrder   = []string{"get", "post", "put", "patch", "delete"}
	toolNameRe    = regexp.MustCompile(`^[a-z0-9_]+$`)
	pathVarRe     = regexp.MustCompile(`\{([^}/]+)\}`)
	reservedNames = map[string]bool{"body": true, "dry_run": true, "confirm": true}
)

// NewRegistry builds the registry from the bytes of an OpenAPI 3.0 document.
func NewRegistry(doc []byte) (*Registry, error) {
	var d struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas    map[string]any `yaml:"schemas"`
			Parameters map[string]any `yaml:"parameters"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("parse openapi document: %w", err)
	}
	paths := make([]string, 0, len(d.Paths))
	for p := range d.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	reg := &Registry{byID: map[string]Operation{}}
	toolNames := map[string]string{}
	for _, path := range paths {
		item := d.Paths[path]
		for _, method := range methodOrder {
			raw, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			id, _ := raw["operationId"].(string)
			if id == "" {
				return nil, fmt.Errorf("%s %s: no operationId", strings.ToUpper(method), path)
			}
			op, include, err := buildOperation(id, method, path, item, raw, d.Components.Schemas, d.Components.Parameters)
			if err != nil {
				return nil, fmt.Errorf("operation %s: %w", id, err)
			}
			if !include {
				continue
			}
			name := ToolName(id)
			if len(name) > 64 || !toolNameRe.MatchString(name) {
				return nil, fmt.Errorf("operation %s: tool name %q is longer than 64 characters or not [a-z0-9_]", id, name)
			}
			if other, dup := toolNames[name]; dup {
				return nil, fmt.Errorf("operation %s: tool name %q is also the tool name of %s", id, name, other)
			}
			toolNames[name] = id
			reg.ops = append(reg.ops, op)
			reg.byID[id] = op
		}
	}
	return reg, nil
}

type permissionDoc struct {
	Kind  string   `yaml:"kind"`
	Every bool     `yaml:"every"`
	AnyOf []string `yaml:"any_of"`
}

func buildOperation(id, method, path string, item, raw, schemas, compParams map[string]any) (Operation, bool, error) {
	if v, ok := raw["x-burrow-mcp"].(bool); ok && !v {
		return Operation{}, false, nil
	}
	var access Access
	permRaw, ok := raw["x-burrow-permission"]
	if ok {
		b, err := yaml.Marshal(permRaw)
		if err != nil {
			return Operation{}, false, err
		}
		var pd permissionDoc
		if err := yaml.Unmarshal(b, &pd); err != nil {
			return Operation{}, false, fmt.Errorf("x-burrow-permission: %w", err)
		}
		switch pd.Kind {
		case "public", "self", "needs", "dashboard":
		default:
			return Operation{}, false, fmt.Errorf("x-burrow-permission: unknown kind %q", pd.Kind)
		}
		known := map[string]bool{}
		for _, p := range authz.AllPermissions() {
			known[string(p)] = true
		}
		access = Access{Kind: pd.Kind, Every: pd.Every}
		for _, p := range pd.AnyOf {
			if !known[p] {
				return Operation{}, false, fmt.Errorf("x-burrow-permission: unknown permission %q", p)
			}
			access.AnyOf = append(access.AnyOf, authz.Permission(p))
		}
		if pd.Kind == "dashboard" {
			return Operation{}, false, nil
		}
	} else {
		return Operation{}, false, fmt.Errorf("no x-burrow-permission")
	}

	op := Operation{
		ID: id, Method: strings.ToUpper(method), Path: path, Access: access,
	}
	op.Summary, _ = raw["summary"].(string)
	op.Description, _ = raw["description"].(string)
	if tags, ok := raw["tags"].([]any); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok {
				op.Tags = append(op.Tags, s)
			}
		}
	}
	op.Destructive = op.Method == http.MethodDelete || raw["x-burrow-destructive"] == true

	// Parameters: path item's, then the operation's; same name+in is replaced.
	type key struct{ name, in string }
	var order []key
	byKey := map[key]map[string]any{}
	add := func(list any) error {
		items, _ := list.([]any)
		for _, it := range items {
			pm, ok := it.(map[string]any)
			if !ok {
				return fmt.Errorf("parameter is not a mapping")
			}
			if ref, ok := pm["$ref"].(string); ok {
				const prefix = "#/components/parameters/"
				if !strings.HasPrefix(ref, prefix) {
					return fmt.Errorf("unsupported parameter $ref %q", ref)
				}
				target, ok := compParams[strings.TrimPrefix(ref, prefix)].(map[string]any)
				if !ok {
					return fmt.Errorf("parameter $ref %q: no such parameter", ref)
				}
				pm = target
			}
			name, _ := pm["name"].(string)
			in, _ := pm["in"].(string)
			if in != "path" && in != "query" {
				return fmt.Errorf("parameter %s is in %s; mark the operation x-burrow-mcp: false or drop the parameter", name, in)
			}
			if reservedNames[name] {
				return fmt.Errorf("parameter %s is reserved for the tool's own arguments", name)
			}
			k := key{name, in}
			if _, seen := byKey[k]; !seen {
				order = append(order, k)
			}
			byKey[k] = pm
		}
		return nil
	}
	if err := add(item["parameters"]); err != nil {
		return Operation{}, false, err
	}
	if err := add(raw["parameters"]); err != nil {
		return Operation{}, false, err
	}

	toParam := func(k key) (Param, error) {
		pm := byKey[k]
		p := Param{Name: k.name, In: k.in}
		p.Description, _ = pm["description"].(string)
		p.Required = k.in == "path" || pm["required"] == true
		sm, _ := pm["schema"].(map[string]any)
		if sm == nil {
			sm = map[string]any{}
		}
		s, err := toJSONSchema(sm, schemas)
		if err != nil {
			return p, fmt.Errorf("parameter %s: %w", k.name, err)
		}
		p.Schema = s
		return p, nil
	}
	// Path parameters in template order.
	declared := map[string]key{}
	for _, k := range order {
		if k.in == "path" {
			declared[k.name] = k
		}
	}
	used := map[string]bool{}
	for _, m := range pathVarRe.FindAllStringSubmatch(path, -1) {
		k, ok := declared[m[1]]
		if !ok {
			return Operation{}, false, fmt.Errorf("path variable {%s} has no parameter", m[1])
		}
		used[m[1]] = true
		p, err := toParam(k)
		if err != nil {
			return Operation{}, false, err
		}
		op.Params = append(op.Params, p)
	}
	for name := range declared {
		if !used[name] {
			return Operation{}, false, fmt.Errorf("path parameter %s is not in the path %s", name, path)
		}
	}
	for _, k := range order {
		if k.in != "query" {
			continue
		}
		p, err := toParam(k)
		if err != nil {
			return Operation{}, false, err
		}
		op.Params = append(op.Params, p)
	}

	if rb, ok := raw["requestBody"].(map[string]any); ok {
		content, _ := rb["content"].(map[string]any)
		js, ok := content["application/json"].(map[string]any)
		if !ok {
			return Operation{}, false, fmt.Errorf("body is not application/json")
		}
		sm, _ := js["schema"].(map[string]any)
		if sm == nil {
			sm = map[string]any{}
		}
		body, err := toJSONSchema(sm, schemas)
		if err != nil {
			return Operation{}, false, fmt.Errorf("body: %w", err)
		}
		op.Body = body
		op.BodyRequired = rb["required"] == true
	}
	return op, true, nil
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return deepCopy(m).(map[string]any)
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}
