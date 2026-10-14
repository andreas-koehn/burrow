package api

// The contract of docs/openapi.yaml beyond "every route appears"
// (openapi_test.go): operation ids, the x-burrow-mcp extension, real schemas
// in place of Empty, and the structure a generator relies on. The tables the
// tests read are in openapi_tables_test.go; what is not converted yet is in
// openapi_pending_test.go and shrinks to nothing.
//
// Everything is read with yaml.v3 into plain maps: no OpenAPI library is in
// go.mod and none is added for this.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// openAPIMethods are the keys of a path item that are operations.
var openAPIMethods = []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}

// openAPIOp is one operation of the document.
type openAPIOp struct {
	Key      string // "GET /api/v1/services"
	Method   string // upper case
	Path     string
	ID       string         // operationId, "" when the operation has none
	Node     map[string]any // the operation object
	PathItem map[string]any // the path item it sits in
}

// openAPIDoc is the parsed document with its operations in a stable order.
type openAPIDoc struct {
	Root map[string]any
	Ops  []openAPIOp
}

// oaMap returns v as a map with string keys. yaml.v3 decodes a mapping whose
// keys are all strings into map[string]any and any other mapping into
// map[any]any; both are accepted here.
func oaMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = val
		}
		return out, true
	}
	return nil, false
}

// loadOpenAPIDoc parses the canonical file (not the embedded copy).
func loadOpenAPIDoc(t *testing.T) openAPIDoc {
	t.Helper()
	data, err := os.ReadFile(docsOpenAPIPath)
	if err != nil {
		t.Fatalf("read %s: %v", docsOpenAPIPath, err)
	}
	return parseOpenAPIDoc(t, data)
}

func parseOpenAPIDoc(t *testing.T, data []byte) openAPIDoc {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	doc := openAPIDoc{Root: root}
	paths, _ := oaMap(root["paths"])
	for p, raw := range paths {
		item, ok := oaMap(raw)
		if !ok {
			t.Fatalf("path %s is not a mapping", p)
		}
		for _, m := range openAPIMethods {
			node, ok := oaMap(item[m])
			if !ok {
				continue
			}
			id, _ := node["operationId"].(string)
			method := strings.ToUpper(m)
			doc.Ops = append(doc.Ops, openAPIOp{
				Key: method + " " + p, Method: method, Path: p, ID: id, Node: node, PathItem: item,
			})
		}
	}
	sort.Slice(doc.Ops, func(i, j int) bool {
		if doc.Ops[i].Path != doc.Ops[j].Path {
			return doc.Ops[i].Path < doc.Ops[j].Path
		}
		return doc.Ops[i].Method < doc.Ops[j].Method
	})
	return doc
}

// byKey and byID index the operations.
func (d openAPIDoc) byKey() map[string]openAPIOp {
	out := make(map[string]openAPIOp, len(d.Ops))
	for _, op := range d.Ops {
		out[op.Key] = op
	}
	return out
}

func (d openAPIDoc) byID() map[string]openAPIOp {
	out := make(map[string]openAPIOp, len(d.Ops))
	for _, op := range d.Ops {
		if op.ID != "" {
			out[op.ID] = op
		}
	}
	return out
}

// resolveRef follows a local reference ("#/components/schemas/User").
func (d openAPIDoc) resolveRef(ref string) (map[string]any, bool) {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, false
	}
	cur := d.Root
	for _, part := range strings.Split(rest, "/") {
		next, ok := oaMap(cur[part])
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// oaRefName is the last segment of a reference: the component's name.
func oaRefName(ref string) string {
	return ref[strings.LastIndexByte(ref, '/')+1:]
}

// oaWalkRefs calls fn for every "$ref" value below v, with the place it sits
// in: where is the path of keys from the start (a path of the document is
// "paths./api/v1/x.get.responses.200.content"), so a failure can say which
// operation or schema holds the reference.
func oaWalkRefs(where string, v any, fn func(where, ref string)) {
	if m, ok := oaMap(v); ok {
		for _, k := range oaKeys(m) {
			if k == "$ref" {
				if s, ok := m[k].(string); ok {
					fn(where, s)
				}
				continue
			}
			oaWalkRefs(where+"."+k, m[k], fn)
		}
		return
	}
	if list, ok := v.([]any); ok {
		for i, val := range list {
			oaWalkRefs(fmt.Sprintf("%s[%d]", where, i), val, fn)
		}
	}
}

func oaKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func oaSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

// ─── Rule 1: operation ids ──────────────────────────────────────────────────

var (
	// The rule as it is written down: lowerCamelCase.
	oaOperationIDRe = regexp.MustCompile(`^[a-z][A-Za-z0-9]+$`)
	// One form only: every word starts with one capital, so an acronym is
	// written as a word (serviceApiKeysList, not serviceAPIKeysList) and the
	// snake_case form a tool name is made from is unambiguous.
	oaOperationIDWordsRe = regexp.MustCompile(`^[a-z][a-z0-9]*([A-Z][a-z0-9]+)*$`)
	oaCamelBoundaryRe    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

// oaMaxToolNameLen bounds the snake_case form of an id. Model APIs accept tool
// names of at most 64 characters and the aggregated MCP endpoint puts
// "<source>__" in front, so 48 leaves room for a source slug.
const oaMaxToolNameLen = 48

// oaSnakeCase is the form an operation id takes as a tool name.
func oaSnakeCase(id string) string {
	return strings.ToLower(oaCamelBoundaryRe.ReplaceAllString(id, "${1}_${2}"))
}

// oaCheckOperationIDs is the rule of TestOpenAPI_OperationIDs as a function, so
// that the rule itself can be tested on small documents.
func oaCheckOperationIDs(doc openAPIDoc, golden map[string]string, pending []string, reserved map[string]string) []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	ops := doc.byKey()
	pend := oaSet(pending)
	seenID := map[string]string{}

	for _, op := range doc.Ops {
		if op.ID == "" {
			if !pend[op.Key] {
				fail("%s has no operationId", op.Key)
			}
			continue
		}
		if pend[op.Key] {
			fail("%s has an operationId now: remove it from openAPIPendingOperationIDs", op.Key)
		}
		if !oaOperationIDRe.MatchString(op.ID) {
			fail("%s: operationId %q does not match %s", op.Key, op.ID, oaOperationIDRe)
		} else if !oaOperationIDWordsRe.MatchString(op.ID) {
			fail("%s: operationId %q writes an acronym in capitals; write it as a word (Api, Ai, Ip, Mcp)", op.Key, op.ID)
		}
		if other, dup := seenID[op.ID]; dup {
			fail("operationId %q is used by %s and %s", op.ID, other, op.Key)
		}
		seenID[op.ID] = op.Key
		// With one capital per word, two different ids never share a
		// snake_case form; only the length is left to check.
		if snake := oaSnakeCase(op.ID); len(snake) > oaMaxToolNameLen {
			fail("%s: operationId %q is %d characters as a tool name (%s), more than %d", op.Key, op.ID, len(snake), snake, oaMaxToolNameLen)
		}

		if path, isReserved := oaReservedPath(reserved, op.ID); isReserved {
			if op.Path != path {
				fail("%s: operationId %q is reserved for %s", op.Key, op.ID, path)
			}
			continue
		}
		want, known := golden[op.Key]
		switch {
		case !known:
			fail("%s (operationId %q) is not in openAPIOperationIDs: add the row", op.Key, op.ID)
		case want != op.ID:
			fail("%s: operationId is %q, openAPIOperationIDs says %q (an id is a public name: renaming it breaks generated tools and clients)", op.Key, op.ID, want)
		}
	}
	for path := range oaValues(reserved) {
		for _, op := range doc.Ops {
			if op.Path == path && op.ID != "" {
				if _, isReserved := oaReservedPath(reserved, op.ID); !isReserved {
					fail("%s: %s is reserved for another operation id, found %q", op.Key, path, op.ID)
				}
			}
		}
	}
	for key := range golden {
		if _, ok := ops[key]; !ok {
			fail("openAPIOperationIDs has %q, which is not an operation of the document", key)
		}
	}
	for _, key := range pending {
		if _, ok := ops[key]; !ok {
			fail("openAPIPendingOperationIDs has %q, which is not an operation of the document", key)
		}
	}
	sort.Strings(errs)
	return errs
}

// oaReservedPath reports whether id is a reserved id or starts with one (a
// path with several methods needs one id per method: mcpManagement,
// mcpManagementGet), and the path it is reserved for.
func oaReservedPath(reserved map[string]string, id string) (string, bool) {
	for prefix, path := range reserved {
		if strings.HasPrefix(id, prefix) {
			return path, true
		}
	}
	return "", false
}

func oaValues(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for _, v := range m {
		out[v] = true
	}
	return out
}

// TestOpenAPI_OperationIDs: every operation has an operationId in the one
// form, unique in the document and as a tool name, and equal to the row of
// openAPIOperationIDs. Operations without one must be listed in
// openAPIPendingOperationIDs.
func TestOpenAPI_OperationIDs(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	for _, e := range oaCheckOperationIDs(doc, openAPIOperationIDs, openAPIPendingOperationIDs, openAPIReservedOperationIDs) {
		t.Error(e)
	}
	seen := map[string]string{}
	for key, id := range openAPIOperationIDs {
		if other, dup := seen[id]; dup {
			t.Errorf("openAPIOperationIDs gives %q to %s and %s", id, other, key)
		}
		seen[id] = key
		if !oaOperationIDWordsRe.MatchString(id) {
			t.Errorf("openAPIOperationIDs[%q] = %q is not in the one form", key, id)
		}
		if _, isReserved := oaReservedPath(openAPIReservedOperationIDs, id); isReserved {
			t.Errorf("openAPIOperationIDs[%q] = %q is a reserved id", key, id)
		}
	}
}

// TestOpenAPI_OperationIDRule proves the rule on small documents: it must
// refuse what it is there to refuse.
func TestOpenAPI_OperationIDRule(t *testing.T) {
	const head = "paths:\n"
	op := func(path, method, id string) string {
		s := "  " + path + ":\n    " + method + ":\n"
		if id != "" {
			s += "      operationId: " + id + "\n"
		}
		return s + "      responses:\n        '204': { description: ok }\n"
	}
	reserved := map[string]string{"mcpManagement": "/mcp/burrow"}
	cases := []struct {
		name    string
		yaml    string
		golden  map[string]string
		pending []string
		want    string // substring of the one expected error; "" = no error
	}{
		{"ok", head + op("/a", "get", "thingsList"), map[string]string{"GET /a": "thingsList"}, nil, ""},
		{"missing", head + op("/a", "get", ""), nil, nil, "has no operationId"},
		{"pending is allowed", head + op("/a", "get", ""), nil, []string{"GET /a"}, ""},
		{"pending but converted", head + op("/a", "get", "thingsList"), map[string]string{"GET /a": "thingsList"}, []string{"GET /a"}, "remove it from openAPIPendingOperationIDs"},
		{"upper first", head + op("/a", "get", "ThingsList"), map[string]string{"GET /a": "ThingsList"}, nil, "does not match"},
		{"underscore", head + op("/a", "get", "things_list"), map[string]string{"GET /a": "things_list"}, nil, "does not match"},
		{"acronym", head + op("/a", "get", "serviceAPIKeysList"), map[string]string{"GET /a": "serviceAPIKeysList"}, nil, "acronym"},
		{"duplicate", head + op("/a", "get", "thingsList") + op("/b", "get", "thingsList"),
			map[string]string{"GET /a": "thingsList", "GET /b": "thingsList"}, nil, "is used by"},
		{"renamed", head + op("/a", "get", "thingList"), map[string]string{"GET /a": "thingsList"}, nil, "renaming it breaks"},
		{"not in table", head + op("/a", "get", "thingsList"), nil, nil, "add the row"},
		{"stale row", head + op("/a", "get", "thingsList"), map[string]string{"GET /a": "thingsList", "GET /gone": "goneGet"}, nil, "not an operation of the document"},
		{"too long", head + op("/a", "get", "aVeryLongResourceNameThatGoesOnAndOnAndOnAndOnList"),
			map[string]string{"GET /a": "aVeryLongResourceNameThatGoesOnAndOnAndOnAndOnList"}, nil, "more than 48"},
		{"reserved id at its path", head + op("/mcp/burrow", "post", "mcpManagement"), nil, nil, ""},
		{"reserved id with a suffix at its path", head + op("/mcp/burrow", "get", "mcpManagementGet"), nil, nil, ""},
		{"reserved id elsewhere", head + op("/a", "post", "mcpManagement"), nil, nil, "is reserved for /mcp/burrow"},
		{"other id at a reserved path", head + op("/mcp/burrow", "post", "thingCreate"), map[string]string{"POST /mcp/burrow": "thingCreate"}, nil, "reserved for another operation id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := oaCheckOperationIDs(parseOpenAPIDoc(t, []byte(tc.yaml)), tc.golden, tc.pending, reserved)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("want no error, got %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0], tc.want) {
				t.Fatalf("want one error containing %q, got %v", tc.want, errs)
			}
		})
	}
	if got := oaSnakeCase("serviceApiKeysList"); got != "service_api_keys_list" {
		t.Errorf("oaSnakeCase = %q", got)
	}
}

// ─── Rule 4: x-burrow-mcp ───────────────────────────────────────────────────

// oaMCPFlag reports the value of x-burrow-mcp on an operation.
func oaMCPFlag(op openAPIOp) (disabled, present, isBool bool) {
	v, present := op.Node["x-burrow-mcp"]
	if !present {
		return false, false, true
	}
	b, isBool := v.(bool)
	return isBool && !b, true, isBool
}

// oaCheckMCPFlag is the rule of TestOpenAPI_MCPFlag as a function.
func oaCheckMCPFlag(doc openAPIDoc, pinned map[string]string) []string {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	ops := doc.byKey()
	pinnedKeys := oaValues(pinned)

	for _, op := range doc.Ops {
		disabled, present, isBool := oaMCPFlag(op)
		if present && !isBool {
			fail("%s: x-burrow-mcp must be a boolean, found %v", op.Key, op.Node["x-burrow-mcp"])
			continue
		}
		if strings.HasPrefix(op.Path, "/api/v1/internal/") {
			fail("%s: test-only routes are never part of the document", op.Key)
			continue
		}
		managementAPI := strings.HasPrefix(op.Path, "/api/v1/") && !strings.HasPrefix(op.Path, "/api/v1/openapi")
		if !managementAPI && op.Key != "GET /metrics" {
			// Probes, hand-out, the document itself and the data plane. An
			// operation without an id is not converted yet;
			// TestOpenAPI_OperationIDs tracks it.
			if !disabled && op.ID != "" {
				fail("%s is outside the management API: it must carry x-burrow-mcp: false", op.Key)
			}
			continue
		}
		if disabled && op.ID == "" {
			fail("%s carries x-burrow-mcp: false but has no operationId", op.Key)
		}
		if disabled && op.ID != "" && !pinnedKeys[op.Key] {
			fail("%s (%s) carries x-burrow-mcp: false but is not in openAPIMCPDisabled: add it there with the reason in the README list, or drop the flag", op.Key, op.ID)
		}
	}
	for id, key := range pinned {
		op, ok := ops[key]
		switch {
		case !ok:
			fail("openAPIMCPDisabled[%q] names %q, which is not an operation of the document", id, key)
		case op.ID == "":
			// Not converted yet; TestOpenAPI_OperationIDs tracks it.
		case op.ID != id:
			fail("openAPIMCPDisabled[%q] names %s, whose operationId is %q", id, key, op.ID)
		default:
			if disabled, _, _ := oaMCPFlag(op); !disabled {
				fail("%s (%s) must carry x-burrow-mcp: false: it must never become an agent tool", key, id)
			}
		}
	}
	sort.Strings(errs)
	return errs
}

// TestOpenAPI_MCPFlag: x-burrow-mcp is a boolean where present; exactly the
// operations of openAPIMCPDisabled carry false inside the management API;
// everything outside it carries false.
func TestOpenAPI_MCPFlag(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	for _, e := range oaCheckMCPFlag(doc, openAPIMCPDisabled) {
		t.Error(e)
	}
	for id, key := range openAPIMCPDisabled {
		if openAPIOperationIDs[key] != id {
			t.Errorf("openAPIMCPDisabled[%q] = %q, but openAPIOperationIDs[%q] = %q", id, key, key, openAPIOperationIDs[key])
		}
	}
}

func TestOpenAPI_MCPFlagRule(t *testing.T) {
	op := func(path, id, flag string) string {
		s := "  " + path + ":\n    post:\n"
		if id != "" {
			s += "      operationId: " + id + "\n"
		}
		if flag != "" {
			s += "      x-burrow-mcp: " + flag + "\n"
		}
		return s + "      responses:\n        '204': { description: ok }\n"
	}
	pinned := map[string]string{"authLogin": "POST /api/v1/auth/login"}
	login := op("/api/v1/auth/login", "authLogin", "false")
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"ok", login + op("/api/v1/things", "thingCreate", ""), ""},
		{"true is a boolean too", login + op("/api/v1/things", "thingCreate", "true"), ""},
		{"not a boolean", login + op("/api/v1/things", "thingCreate", `"false"`), "must be a boolean"},
		{"pinned operation without the flag", op("/api/v1/auth/login", "authLogin", ""), "must carry x-burrow-mcp: false"},
		{"pinned operation not converted yet", op("/api/v1/auth/login", "", ""), ""},
		{"false outside the pinned set", login + op("/api/v1/things", "thingCreate", "false"), "is not in openAPIMCPDisabled"},
		{"data plane without the flag", login + op("/mcp/burrow", "mcpManagement", ""), "outside the management API"},
		{"data plane with the flag", login + op("/mcp/burrow", "mcpManagement", "false"), ""},
		{"the document itself", login + op("/api/v1/openapi.json", "openapiJsonGet", ""), "outside the management API"},
		{"test-only route", login + op("/api/v1/internal/test-reset", "testReset", "false"), "test-only routes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := oaCheckMCPFlag(parseOpenAPIDoc(t, []byte("paths:\n"+tc.yaml)), pinned)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("want no error, got %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0], tc.want) {
				t.Fatalf("want one error containing %q, got %v", tc.want, errs)
			}
		})
	}
}

// ─── Rule 2: Empty is for handlers that answer {} ───────────────────────────

const oaEmptyRef = "#/components/schemas/Empty"

func oaReferencesEmpty(v any) bool {
	found := false
	oaWalkRefs("", v, func(_, ref string) {
		if ref == oaEmptyRef {
			found = true
		}
	})
	return found
}

// TestOpenAPI_EmptySchema: an operation may reference Empty only when its
// handler really answers {} (openAPIEmptyAllowed, by operationId) or while it
// is still to be converted (openAPIPendingEmpty). Component schemas never do.
func TestOpenAPI_EmptySchema(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	pending := oaSet(openAPIPendingEmpty)
	usedAllowed := map[string]bool{}
	usedPending := map[string]bool{}
	for _, op := range doc.Ops {
		if !oaReferencesEmpty(op.Node) {
			continue
		}
		switch {
		case op.ID != "" && openAPIEmptyAllowed[op.ID] != "":
			usedAllowed[op.ID] = true
		case pending[op.Key]:
			usedPending[op.Key] = true
		default:
			t.Errorf("%s (%s) references the Empty schema: write the schema of what the handler decodes or writes", op.Key, op.ID)
		}
	}
	for _, key := range openAPIPendingEmpty {
		if !usedPending[key] {
			t.Errorf("openAPIPendingEmpty has %q, which no longer references Empty: remove it", key)
		}
	}
	for id := range openAPIEmptyAllowed {
		if !usedAllowed[id] {
			t.Errorf("openAPIEmptyAllowed has %q, which does not reference Empty: remove it", id)
		}
	}
	schemas, _ := doc.resolveRef("#/components/schemas")
	for name, s := range schemas {
		if oaReferencesEmpty(s) {
			t.Errorf("component schema %s references Empty", name)
		}
	}
	empty, ok := doc.resolveRef(oaEmptyRef)
	if !ok || empty["type"] != "object" || empty["properties"] != nil {
		t.Errorf("components.schemas.Empty must stay a plain object without properties, found %v", empty)
	}
}

// ─── Rule 5: structure a generator relies on ────────────────────────────────

var oaPathParamRe = regexp.MustCompile(`\{([^}]+)\}`)

// TestOpenAPI_Structure: every $ref resolves, every path parameter of a path
// template is declared (and nothing else is declared as one), every operation
// has a response, every response has a description and every content entry a
// schema. Inside the management API (/api/v1/) also: a request body is
// application/json with a schema (or one of openAPINonJSONBodies) and says
// whether it is required, a 200/201/202 has content and a 204 has none.
func TestOpenAPI_Structure(t *testing.T) {
	doc := loadOpenAPIDoc(t)

	oaWalkRefs("document", doc.Root, func(where, ref string) {
		if _, ok := doc.resolveRef(ref); !ok {
			t.Errorf("%s: $ref %q does not resolve", where, ref)
		}
	})

	pendingContent := oaSet(openAPIPendingResponseContent)
	usedPendingContent := map[string]bool{}
	for _, op := range doc.Ops {
		// Path parameters.
		declared := map[string]bool{}
		for _, level := range []any{op.PathItem["parameters"], op.Node["parameters"]} {
			list, _ := level.([]any)
			for _, raw := range list {
				p, _ := oaMap(raw)
				if ref, ok := p["$ref"].(string); ok {
					p, _ = doc.resolveRef(ref)
				}
				name, _ := p["name"].(string)
				if name == "" {
					t.Errorf("%s: a parameter has no name", op.Key)
					continue
				}
				if _, ok := oaMap(p["schema"]); !ok {
					t.Errorf("%s: parameter %s has no schema", op.Key, name)
				}
				if p["in"] == "path" {
					declared[name] = true
					if p["required"] != true {
						t.Errorf("%s: path parameter %s must be required: true", op.Key, name)
					}
				}
			}
		}
		inTemplate := map[string]bool{}
		for _, m := range oaPathParamRe.FindAllStringSubmatch(op.Path, -1) {
			inTemplate[m[1]] = true
			if !declared[m[1]] {
				t.Errorf("%s: path parameter {%s} is not declared", op.Key, m[1])
			}
		}
		for name := range declared {
			if !inTemplate[name] {
				t.Errorf("%s: declares path parameter %s, which the path does not have", op.Key, name)
			}
		}

		// Responses.
		responses, _ := oaMap(op.Node["responses"])
		if len(responses) == 0 {
			t.Errorf("%s has no response", op.Key)
		}
		for code, raw := range responses {
			resp, _ := oaMap(raw)
			if ref, ok := resp["$ref"].(string); ok {
				resp, _ = doc.resolveRef(ref)
			}
			if d, _ := resp["description"].(string); strings.TrimSpace(d) == "" {
				t.Errorf("%s: response %s has no description", op.Key, code)
			}
			content, hasContent := oaMap(resp["content"])
			for mediaType, rawMT := range content {
				mt, _ := oaMap(rawMT)
				if _, ok := oaMap(mt["schema"]); !ok {
					t.Errorf("%s: response %s %s has no schema", op.Key, code, mediaType)
				}
			}
			if !strings.HasPrefix(op.Path, "/api/v1/") {
				continue // the rules below are those of the management API
			}
			// A 200, 201 or 202 of this API always has a body; 204 never has.
			switch code {
			case "200", "201", "202":
				if !hasContent && !pendingContent[op.Key] {
					t.Errorf("%s: response %s declares no content", op.Key, code)
				}
				if !hasContent && pendingContent[op.Key] {
					usedPendingContent[op.Key] = true
				}
			case "204":
				if hasContent {
					t.Errorf("%s: response 204 must not declare content", op.Key)
				}
			}
		}

		// Request body.
		rb, hasBody := oaMap(op.Node["requestBody"])
		if hasBody && !strings.HasPrefix(op.Path, "/api/v1/") {
			continue // a data-plane body follows its own protocol
		}
		if !hasBody {
			if openAPINonJSONBodies[op.Key] != "" {
				t.Errorf("openAPINonJSONBodies has %q, which has no request body", op.Key)
			}
			continue
		}
		content, _ := oaMap(rb["content"])
		want := "application/json"
		if other := openAPINonJSONBodies[op.Key]; other != "" {
			want = other
		}
		if len(content) != 1 {
			t.Errorf("%s: a request body has exactly one media type, found %v", op.Key, oaKeys(content))
		}
		mt, ok := oaMap(content[want])
		if !ok {
			t.Errorf("%s: request body must be %s, found %v", op.Key, want, oaKeys(content))
			continue
		}
		if _, ok := oaMap(mt["schema"]); !ok {
			t.Errorf("%s: request body %s has no schema", op.Key, want)
		}
		if _, ok := rb["required"].(bool); !ok {
			t.Errorf("%s: request body must say required: true or required: false", op.Key)
		}
	}
	for _, key := range openAPIPendingResponseContent {
		if !usedPendingContent[key] {
			t.Errorf("openAPIPendingResponseContent has %q, which has no 200/201/202 without content (it declares its content now, or is no operation of the document): remove it", key)
		}
	}
	for key := range openAPINonJSONBodies {
		if _, ok := doc.byKey()[key]; !ok {
			t.Errorf("openAPINonJSONBodies has %q, which is not an operation of the document", key)
		}
	}
}

// ─── Schemas are made of schema keywords ────────────────────────────────────

// oaSchemaKeywords are the keys of an OpenAPI 3.0 schema object. A key outside
// this set is a typo or a YAML accident: a comma inside a flow mapping
// ({ description: a, b }) silently makes "b" a key.
var oaSchemaKeywords = oaSet([]string{
	"$ref", "type", "format", "title", "description", "default", "example", "enum",
	"properties", "required", "additionalProperties", "items",
	"allOf", "oneOf", "anyOf", "not",
	"nullable", "readOnly", "writeOnly", "deprecated",
	"minLength", "maxLength", "pattern", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"minItems", "maxItems", "uniqueItems", "minProperties", "maxProperties",
})

var oaSchemaTypes = oaSet([]string{"object", "array", "string", "integer", "number", "boolean"})

// checkSchemaNode reports what is wrong with one schema and the schemas
// nested in it.
func (d openAPIDoc) checkSchemaNode(where string, node any) []string {
	return d.checkSchemaPart(where, node, false)
}

// checkSchemaPart is checkSchemaNode for a schema that may be one part of an
// allOf. A part may require properties that a sibling part declares, so its
// `required` list is checked on the composed schema, not on the part.
func (d openAPIDoc) checkSchemaPart(where string, node any, part bool) []string {
	m, ok := oaMap(node)
	if !ok {
		return []string{where + ": a schema must be a mapping"}
	}
	var errs []string
	for _, key := range oaKeys(m) {
		if !oaSchemaKeywords[key] && !strings.HasPrefix(key, "x-") {
			errs = append(errs, fmt.Sprintf("%s: %q is not a schema keyword", where, key))
		}
	}
	// OpenAPI 3.0 ignores every sibling of $ref: { $ref, nullable: true } is
	// not nullable. Wrap the reference in allOf to add a keyword.
	if _, isRef := m["$ref"]; isRef && len(m) > 1 {
		errs = append(errs, fmt.Sprintf("%s: keys beside $ref are ignored by OpenAPI 3.0 (%v): use allOf", where, oaKeys(m)))
	}
	if req, has := m["required"]; has {
		list, isList := req.([]any)
		for _, r := range list {
			if _, isString := r.(string); !isString {
				isList = false
			}
		}
		if !isList {
			errs = append(errs, fmt.Sprintf("%s: `required` must be a list of property names, found %v", where, req))
		}
	}
	if t, has := m["type"]; has {
		if s, _ := t.(string); !oaSchemaTypes[s] {
			errs = append(errs, fmt.Sprintf("%s: type %v is not a schema type", where, t))
		}
	}
	props, hasProps := oaMap(m["properties"])
	for _, name := range oaKeys(props) {
		errs = append(errs, d.checkSchemaNode(where+"."+name, props[name])...)
	}
	if items, has := m["items"]; has {
		errs = append(errs, d.checkSchemaNode(where+"[]", items)...)
	}
	if ap, ok := oaMap(m["additionalProperties"]); ok {
		errs = append(errs, d.checkSchemaNode(where+".*", ap)...)
	}
	_, composed := m["allOf"]
	for _, key := range []string{"allOf", "oneOf", "anyOf"} {
		parts, _ := m[key].([]any)
		for i, p := range parts {
			errs = append(errs, d.checkSchemaPart(fmt.Sprintf("%s.%s[%d]", where, key, i), p, key == "allOf")...)
		}
	}
	// Every name in `required` is a property.
	if (hasProps || composed) && !part {
		v := d.view(node)
		for _, name := range oaKeys(v.Required) {
			if _, ok := v.Properties[name]; !ok {
				errs = append(errs, fmt.Sprintf("%s: `required` names %q, which is not a property", where, name))
			}
		}
	}
	return errs
}

// TestOpenAPI_Schemas: every schema of the document (components, parameters,
// request bodies, responses, response headers) is made of schema keywords,
// has a known type, and requires only properties it has.
func TestOpenAPI_Schemas(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	schemas, _ := doc.resolveRef("#/components/schemas")
	for _, name := range oaKeys(schemas) {
		for _, e := range doc.checkSchemaNode(name, schemas[name]) {
			t.Error(e)
		}
	}
	params, _ := doc.resolveRef("#/components/parameters")
	for _, name := range oaKeys(params) {
		p, _ := oaMap(params[name])
		for _, e := range doc.checkSchemaNode("parameter "+name, p["schema"]) {
			t.Error(e)
		}
	}
	content := func(where string, holder map[string]any) {
		c, _ := oaMap(holder["content"])
		for _, mediaType := range oaKeys(c) {
			mt, _ := oaMap(c[mediaType])
			if s, has := mt["schema"]; has {
				for _, e := range doc.checkSchemaNode(where+" "+mediaType, s) {
					t.Error(e)
				}
			}
		}
	}
	responses, _ := doc.resolveRef("#/components/responses")
	for _, name := range oaKeys(responses) {
		r, _ := oaMap(responses[name])
		content("response "+name, r)
	}
	for _, op := range doc.Ops {
		for _, level := range []any{op.PathItem["parameters"], op.Node["parameters"]} {
			list, _ := level.([]any)
			for _, raw := range list {
				p, _ := oaMap(raw)
				if s, has := p["schema"]; has {
					for _, e := range doc.checkSchemaNode(fmt.Sprintf("%s parameter %v", op.Key, p["name"]), s) {
						t.Error(e)
					}
				}
			}
		}
		if rb, ok := oaMap(op.Node["requestBody"]); ok {
			content(op.Key+" request", rb)
		}
		rs, _ := oaMap(op.Node["responses"])
		for _, code := range oaKeys(rs) {
			r, _ := oaMap(rs[code])
			content(op.Key+" response "+code, r)
			headers, _ := oaMap(r["headers"])
			for _, h := range oaKeys(headers) {
				hm, _ := oaMap(headers[h])
				if s, has := hm["schema"]; has {
					for _, e := range doc.checkSchemaNode(fmt.Sprintf("%s response %s header %s", op.Key, code, h), s) {
						t.Error(e)
					}
				}
			}
		}
	}

	// The rule itself.
	bad := parseOpenAPIDoc(t, []byte("components:\n  schemas:\n    X:\n      type: object\n      required: [a, gone]\n      properties:\n        a: { type: string, description: one, two }\n        b: { type: text }\n"))
	got := strings.Join(bad.checkSchemaNode("X", bad.Root["components"].(map[string]any)["schemas"].(map[string]any)["X"]), "\n")
	for _, want := range []string{`"two" is not a schema keyword`, "type text is not a schema type", `names "gone"`} {
		if !strings.Contains(got, want) {
			t.Errorf("checkSchemaNode: want an error containing %q, got:\n%s", want, got)
		}
	}
	bad2 := parseOpenAPIDoc(t, []byte("components:\n  schemas:\n    X:\n      type: object\n      properties:\n        a: { $ref: '#/components/schemas/Y', nullable: true }\n        b: { type: string, required: true }\n        c: { type: string, required: [d, 3] }\n    Y: { type: object }\n"))
	got = strings.Join(bad2.checkSchemaNode("X", bad2.Root["components"].(map[string]any)["schemas"].(map[string]any)["X"]), "\n")
	for _, want := range []string{"X.a: keys beside $ref are ignored", "X.b: `required` must be a list of property names, found true", "X.c: `required` must be a list of property names"} {
		if !strings.Contains(got, want) {
			t.Errorf("checkSchemaNode: want an error containing %q, got:\n%s", want, got)
		}
	}
}

// TestOpenAPI_NothingPending is the gate of the last task: with every area
// converted, the lists of openapi_pending_test.go are empty. It is skipped
// while openAPIConversionDone is false.
func TestOpenAPI_NothingPending(t *testing.T) {
	if !openAPIConversionDone {
		t.Skip("conversion in progress: openAPIConversionDone is false")
	}
	if n := len(openAPIPendingOperationIDs); n != 0 {
		t.Errorf("openAPIPendingOperationIDs still has %d entries", n)
	}
	if n := len(openAPIPendingEmpty); n != 0 {
		t.Errorf("openAPIPendingEmpty still has %d entries", n)
	}
	if n := len(openAPIPendingResponseContent); n != 0 {
		t.Errorf("openAPIPendingResponseContent still has %d entries", n)
	}
}
