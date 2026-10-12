package api

// The drift guard: the request and response schemas of docs/openapi.yaml are
// compared with the Go types the handlers decode into and write, by
// reflection over the json tags. The tables (operationId → type) are in
// openapi_tables_test.go.
//
// A table row names the handler's own type. A handler that decodes into an
// inline struct or a map has no type to name; writing an equivalent one here
// would compare the spec with a copy and catch nothing, so such a row says
// why instead (the except field) and the verify task reports it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/cache/exact"
	"github.com/ankoehn/burrow/internal/db"
)

// oaField is one field of a struct as encoding/json writes it.
type oaField struct {
	Name string
	Type reflect.Type
	// Omitted is true when the field can be missing from the output:
	// omitzero, or omitempty on a type that has an empty value. omitempty on
	// a struct (time.Time) has no effect and does not count.
	Omitted bool
}

func oaDeref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// oaFields lists the fields of struct type t, embedded structs flattened.
func oaFields(t reflect.Type) []oaField {
	t = oaDeref(t)
	var out []oaField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" && oaDeref(f.Type).Kind() == reflect.Struct {
			out = append(out, oaFields(f.Type)...)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		omitted := false
		for _, o := range strings.Split(opts, ",") {
			switch o {
			case "omitzero":
				omitted = true
			case "omitempty":
				omitted = omitted || f.Type.Kind() != reflect.Struct
			}
		}
		out = append(out, oaField{Name: name, Type: f.Type, Omitted: omitted})
	}
	return out
}

var oaTimeType = reflect.TypeOf(time.Time{})

// oaStructElem returns the struct type behind t (through pointers and one level
// of slice or array), or nil when t is not made of a struct with fields of
// its own. time.Time is a string on the wire, a map has no fixed fields.
func oaStructElem(t reflect.Type) reflect.Type {
	t = oaDeref(t)
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = oaDeref(t.Elem())
	}
	if t.Kind() != reflect.Struct || t == oaTimeType {
		return nil
	}
	return t
}

// oaSchemaView is a schema with its $ref and allOf parts folded into one.
type oaSchemaView struct {
	Names      []string // component names the schema is made of
	Type       string
	Properties map[string]any
	Required   map[string]bool
	Nullable   bool
	ReadOnly   bool
	Items      any
	FreeForm   bool // additionalProperties is present
}

func (d openAPIDoc) view(node any) oaSchemaView {
	v := oaSchemaView{Properties: map[string]any{}, Required: map[string]bool{}}
	d.fold(node, &v, 0)
	return v
}

func (d openAPIDoc) fold(node any, v *oaSchemaView, depth int) {
	m, ok := oaMap(node)
	if !ok || depth > 16 {
		return
	}
	if ref, ok := m["$ref"].(string); ok {
		v.Names = append(v.Names, oaRefName(ref))
		if target, ok := d.resolveRef(ref); ok {
			d.fold(target, v, depth+1)
		}
	}
	if parts, ok := m["allOf"].([]any); ok {
		for _, part := range parts {
			d.fold(part, v, depth+1)
		}
	}
	if t, ok := m["type"].(string); ok {
		v.Type = t
	}
	if m["nullable"] == true {
		v.Nullable = true
	}
	if m["readOnly"] == true {
		v.ReadOnly = true
	}
	if _, ok := m["additionalProperties"]; ok {
		v.FreeForm = true
	}
	if items, ok := m["items"]; ok {
		v.Items = items
	}
	if props, ok := oaMap(m["properties"]); ok {
		for name, p := range props {
			v.Properties[name] = p
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			v.Required[fmt.Sprint(r)] = true
		}
	}
}

// checkRequestShape compares the writable properties of a request schema
// with the fields of the type the handler decodes into, at every depth.
func (d openAPIDoc) checkRequestShape(where string, node any, t reflect.Type) []string {
	var errs []string
	v := d.view(node)
	if v.Type == "array" {
		return []string{where + ": the schema is an array, the handler decodes an object"}
	}
	fields := map[string]oaField{}
	for _, f := range oaFields(t) {
		fields[f.Name] = f
	}
	writable := map[string]any{}
	for name, p := range v.Properties {
		if !d.view(p).ReadOnly {
			writable[name] = p
		}
	}
	for _, name := range oaKeys(fields) {
		p, ok := writable[name]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: the handler decodes %q (%s), the spec does not have it", where, name, oaDeref(t)))
			continue
		}
		if elem := oaStructElem(fields[name].Type); elem != nil {
			child := p
			if cv := d.view(p); cv.Type == "array" {
				child = cv.Items
			}
			errs = append(errs, d.checkRequestShape(where+"."+name, child, elem)...)
		}
	}
	for _, name := range oaKeys(writable) {
		if _, ok := fields[name]; !ok {
			errs = append(errs, fmt.Sprintf("%s: the spec has property %q, which the handler (%s) does not decode", where, name, oaDeref(t)))
		}
	}
	return errs
}

// checkKeys compares the property names of a schema with a closed key set.
func (d openAPIDoc) checkKeys(where string, node any, keys []string) []string {
	var errs []string
	v := d.view(node)
	want := oaSet(keys)
	for _, k := range keys {
		if _, ok := v.Properties[k]; !ok {
			errs = append(errs, fmt.Sprintf("%s: the handler knows the key %q, the spec does not have it", where, k))
		}
	}
	for _, name := range oaKeys(v.Properties) {
		if !want[name] {
			errs = append(errs, fmt.Sprintf("%s: the spec has property %q, which the handler does not know", where, name))
		}
	}
	return errs
}

// checkResponseShape compares a response schema with the type the handler
// writes: the same property names, `required` exactly for the fields that are
// always written, and `nullable: true` exactly for the pointers that are
// written as null. optional names fields that are left out of some answers
// although their tag has no omitempty (two shapes behind one schema).
func (d openAPIDoc) checkResponseShape(where string, node any, t reflect.Type, optional map[string]bool) []string {
	var errs []string
	v := d.view(node)
	lenient := false
	for _, n := range v.Names {
		if openAPILenientSchemas[n] != "" {
			lenient = true
		}
	}
	seen := map[string]bool{}
	for _, f := range oaFields(t) {
		seen[f.Name] = true
		p, ok := v.Properties[f.Name]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: the handler writes %q (%s), the spec does not have it", where, f.Name, oaDeref(t)))
			continue
		}
		always := !f.Omitted && !optional[f.Name]
		if !lenient && always != v.Required[f.Name] {
			if always {
				errs = append(errs, fmt.Sprintf("%s: %q is always written and must be in `required`", where, f.Name))
			} else {
				errs = append(errs, fmt.Sprintf("%s: %q can be left out of the answer and must not be in `required`", where, f.Name))
			}
		}
		pv := d.view(p)
		isPointer := f.Type.Kind() == reflect.Pointer
		switch {
		case isPointer && !f.Omitted && !pv.Nullable:
			errs = append(errs, fmt.Sprintf("%s: %q is written as null when unset and must be `nullable: true`", where, f.Name))
		case isPointer && f.Omitted && pv.Nullable:
			errs = append(errs, fmt.Sprintf("%s: %q is left out when unset, never null: drop `nullable`", where, f.Name))
		case !isPointer && pv.Nullable && oaScalarKind(f.Type.Kind()):
			errs = append(errs, fmt.Sprintf("%s: %q is never null: drop `nullable`", where, f.Name))
		}
		if elem := oaStructElem(f.Type); elem != nil {
			child := p
			if pv.Type == "array" {
				child = pv.Items
			}
			errs = append(errs, d.checkResponseShape(where+"."+f.Name, child, elem, nil)...)
		}
	}
	for _, name := range oaKeys(v.Properties) {
		if !seen[name] {
			errs = append(errs, fmt.Sprintf("%s: the spec has property %q, which the handler (%s) never writes", where, name, oaDeref(t)))
		}
	}
	return errs
}

func oaScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.Slice, reflect.Map, reflect.Interface, reflect.Array:
		return false
	}
	return true
}

// oaJSONBodySchema returns the application/json schema of a request body.
func oaJSONBodySchema(op openAPIOp) (any, bool) {
	rb, ok := oaMap(op.Node["requestBody"])
	if !ok {
		return nil, false
	}
	content, _ := oaMap(rb["content"])
	mt, ok := oaMap(content["application/json"])
	if !ok {
		return nil, false
	}
	return mt["schema"], true
}

// jsonResponseSchema returns the application/json schema of one response.
func (d openAPIDoc) jsonResponseSchema(op openAPIOp, status string) (any, bool) {
	responses, _ := oaMap(op.Node["responses"])
	resp, ok := oaMap(responses[status])
	if !ok {
		return nil, false
	}
	if ref, ok := resp["$ref"].(string); ok {
		resp, _ = d.resolveRef(ref)
	}
	content, _ := oaMap(resp["content"])
	mt, ok := oaMap(content["application/json"])
	if !ok {
		return nil, false
	}
	return mt["schema"], true
}

// hasJSONSuccess reports whether an operation declares a 2xx JSON response.
func (d openAPIDoc) hasJSONSuccess(op openAPIOp) bool {
	responses, _ := oaMap(op.Node["responses"])
	for status := range responses {
		if strings.HasPrefix(status, "2") {
			if _, ok := d.jsonResponseSchema(op, status); ok {
				return true
			}
		}
	}
	return false
}

// oaConvertedOp finds the operation a table row is about. converted is false
// while the operation has no operationId yet (it is in the pending list).
func oaConvertedOp(t *testing.T, doc openAPIDoc, table, id string) (op openAPIOp, converted bool) {
	t.Helper()
	if op, ok := doc.byID()[id]; ok {
		return op, true
	}
	for key, want := range openAPIOperationIDs {
		if want == id {
			if !oaSet(openAPIPendingOperationIDs)[key] {
				t.Errorf("%s has a row for %q, but %s does not carry that operationId", table, id, key)
			}
			return openAPIOp{}, false
		}
	}
	t.Errorf("%s has a row for %q, which is not an operation id of openAPIOperationIDs", table, id)
	return openAPIOp{}, false
}

// TestOpenAPI_RequestBodiesMatchHandlers: for every operation of the
// management API with a JSON request body, the spec's property names are the
// json tags of the struct the handler decodes into. It fails when a struct
// gains a field the spec lacks, and the other way round.
func TestOpenAPI_RequestBodiesMatchHandlers(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	for _, op := range doc.Ops {
		if op.ID == "" || !strings.HasPrefix(op.Path, "/api/v1/") {
			continue
		}
		_, hasBody := oaJSONBodySchema(op)
		_, inTable := openAPIRequestBodies[op.ID]
		if hasBody && !inTable {
			t.Errorf("%s (%s) has a JSON request body but no row in openAPIRequestBodies", op.Key, op.ID)
		}
	}
	for _, id := range oaKeys(openAPIRequestBodies) {
		row := openAPIRequestBodies[id]
		op, converted := oaConvertedOp(t, doc, "openAPIRequestBodies", id)
		if !converted {
			continue
		}
		schema, ok := oaJSONBodySchema(op)
		if !ok {
			t.Errorf("%s (%s): the handler decodes a JSON body, the spec declares none", op.Key, id)
			continue
		}
		switch {
		case row.except != "":
			t.Logf("exception: %s (%s) is not compared: %s", id, op.Key, row.except)
		case row.keys != nil:
			for _, e := range doc.checkKeys(id+" request", schema, row.keys) {
				t.Error(e)
			}
		default:
			for _, e := range doc.checkRequestShape(id+" request", schema, row.typ) {
				t.Error(e)
			}
		}
	}
}

// TestOpenAPI_ResponsesMatchHandlers: for every operation of the management
// API with a JSON success response, the schema has the fields of the type the
// handler writes, with `required` and `nullable` as the json tags make them.
func TestOpenAPI_ResponsesMatchHandlers(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	for _, op := range doc.Ops {
		if op.ID == "" || !strings.HasPrefix(op.Path, "/api/v1/") {
			continue
		}
		if _, inTable := openAPIResponses[op.ID]; doc.hasJSONSuccess(op) && !inTable {
			t.Errorf("%s (%s) has a JSON success response but no row in openAPIResponses", op.Key, op.ID)
		}
	}
	for _, id := range oaKeys(openAPIResponses) {
		row := openAPIResponses[id]
		op, converted := oaConvertedOp(t, doc, "openAPIResponses", id)
		if !converted {
			continue
		}
		schema, ok := doc.jsonResponseSchema(op, row.status)
		if !ok {
			t.Errorf("%s (%s): the handler answers %s with JSON, the spec declares no such response", op.Key, id, row.status)
			continue
		}
		// The success status is part of the contract: no other 2xx with a
		// JSON body than the one the handler writes (and those of row.also).
		responses, _ := oaMap(op.Node["responses"])
		for _, status := range oaKeys(responses) {
			if _, isJSON := doc.jsonResponseSchema(op, status); !isJSON || !strings.HasPrefix(status, "2") {
				continue
			}
			if status != row.status && !oaSet(row.also)[status] {
				t.Errorf("%s (%s): the spec declares a JSON response %s, the handler answers %s", op.Key, id, status, row.status)
			}
		}
		v := doc.view(schema)
		if row.list != (v.Type == "array") {
			t.Errorf("%s (%s) response %s: handler writes a list = %v, the spec's schema type is %q", op.Key, id, row.status, row.list, v.Type)
			continue
		}
		if row.list {
			schema = v.Items
		}
		where := id + " response " + row.status
		switch {
		case row.except != "":
			t.Logf("exception: %s (%s) response is not compared: %s", id, op.Key, row.except)
		case row.keys != nil:
			for _, e := range doc.checkKeys(where, schema, row.keys) {
				t.Error(e)
			}
		default:
			for _, e := range doc.checkResponseShape(where, schema, row.typ, oaSet(row.optional)) {
				t.Error(e)
			}
		}
	}
	for _, name := range oaKeys(openAPILenientSchemas) {
		if _, ok := doc.resolveRef("#/components/schemas/" + name); !ok {
			t.Errorf("openAPILenientSchemas has %q, which is not a component schema", name)
		}
	}
}

// TestOpenAPI_RequiredFieldsMatchValidation: for the request bodies whose
// handler validates through a function of its own, the spec's `required` list
// is exactly the set of fields the validation refuses to go without.
func TestOpenAPI_RequiredFieldsMatchValidation(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	for _, id := range oaKeys(openAPIRequiredProbes) {
		probe := openAPIRequiredProbes[id]
		t.Run(id, func(t *testing.T) {
			op, converted := oaConvertedOp(t, doc, "openAPIRequiredProbes", id)
			if !converted {
				t.Skip("not converted yet")
			}
			schema, ok := oaJSONBodySchema(op)
			if !ok {
				t.Fatalf("%s has no JSON request body", op.Key)
			}
			required := doc.view(schema).Required
			var body map[string]any
			if err := json.Unmarshal([]byte(probe.valid), &body); err != nil {
				t.Fatalf("probe body: %v", err)
			}
			if msg := probe.validate([]byte(probe.valid)); msg != "" {
				t.Fatalf("the probe body must be valid, the handler says %q", msg)
			}
			for name := range required {
				if _, ok := body[name]; !ok {
					t.Errorf("the spec requires %q, the probe body does not have it: add it to the probe", name)
				}
			}
			names := oaKeys(body)
			sort.Strings(names)
			for _, name := range names {
				without := map[string]any{}
				for k, v := range body {
					if k != name {
						without[k] = v
					}
				}
				raw, _ := json.Marshal(without)
				refused := probe.validate(raw) != ""
				switch {
				case refused && !required[name]:
					t.Errorf("the handler refuses a body without %q, the spec does not list it in `required`", name)
				case !refused && required[name]:
					t.Errorf("the spec lists %q in `required`, the handler accepts a body without it", name)
				}
			}
		})
	}
}

// TestOpenAPI_CacheSettingsBody covers the request body whose decoding struct
// is out of reach: PUT /cache/settings goes through exact.SettingsFromJSON,
// which decodes into an inline struct. A body with every property of the
// schema set to a value that is not the zero value must fill every field of
// exact.Settings, and the schema must have as many properties as that struct
// has fields.
func TestOpenAPI_CacheSettingsBody(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	op, converted := oaConvertedOp(t, doc, "TestOpenAPI_CacheSettingsBody", "cacheSettingsUpdate")
	if !converted {
		t.Skip("not converted yet")
	}
	schema, ok := oaJSONBodySchema(op)
	if !ok {
		t.Fatalf("%s has no JSON request body", op.Key)
	}
	v := doc.view(schema)
	body := map[string]any{}
	for name, p := range v.Properties {
		switch pv := doc.view(p); pv.Type {
		case "boolean":
			body[name] = true
		case "integer":
			body[name] = 7
		case "string":
			m, _ := oaMap(p)
			enum, _ := m["enum"].([]any)
			if len(enum) == 0 {
				t.Fatalf("property %s: a string property of this schema needs an enum", name)
			}
			body[name] = enum[len(enum)-1]
		default:
			t.Fatalf("property %s has type %q, which this test does not know", name, pv.Type)
		}
	}
	raw, _ := json.Marshal(body)
	got, err := exact.SettingsFromJSON(raw)
	if err != nil {
		t.Fatalf("exact.SettingsFromJSON(%s): %v", raw, err)
	}
	rv := reflect.ValueOf(got)
	if rv.NumField() != len(v.Properties) {
		t.Errorf("the schema has %d properties, exact.Settings has %d fields", len(v.Properties), rv.NumField())
	}
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).IsZero() {
			t.Errorf("exact.Settings.%s is not set by %s: the schema lacks its property", rv.Type().Field(i).Name, raw)
		}
	}
}

// oaNoProviders is a provider store that knows no provider.
type oaNoProviders struct{ AIProviderStore }

func (oaNoProviders) ProviderBySlug(context.Context, string) (db.AIProvider, error) {
	return db.AIProvider{}, db.ErrNotFound
}

// TestOpenAPI_ProviderModelBody covers the request body that is decoded into
// an inline struct: POST /ai/providers/{slug}/models. A body made of the
// schema's required properties must pass the handler's check of the body (the
// answer is then 404, because the probe's store knows no provider), and a
// body without them must not (400). It pins the name of the field; a field
// the handler adds without making it required is not seen.
func TestOpenAPI_ProviderModelBody(t *testing.T) {
	doc := loadOpenAPIDoc(t)
	op, converted := oaConvertedOp(t, doc, "TestOpenAPI_ProviderModelBody", "providerModelCreate")
	if !converted {
		t.Skip("not converted yet")
	}
	schema, ok := oaJSONBodySchema(op)
	if !ok {
		t.Fatalf("%s has no JSON request body", op.Key)
	}
	v := doc.view(schema)
	if len(v.Required) == 0 {
		t.Fatalf("%s: the schema requires nothing, the handler requires the model id", op.Key)
	}
	body := map[string]any{}
	for name := range v.Required {
		if pv := doc.view(v.Properties[name]); pv.Type != "string" {
			t.Fatalf("required property %s has type %q, which this test does not know", name, pv.Type)
		}
		body[name] = "probe-model"
	}
	post := func(payload map[string]any) int {
		raw, _ := json.Marshal(payload)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/providers/p/models", strings.NewReader(string(raw)))
		Deps{AIProviders: oaNoProviders{}}.PostAIProviderModel(rec, req)
		return rec.Code
	}
	if code := post(body); code != http.StatusNotFound {
		t.Errorf("a body with the schema's required properties %v: status %d, want 404 (accepted, provider unknown)", oaKeys(body), code)
	}
	if code := post(map[string]any{}); code != http.StatusBadRequest {
		t.Errorf("an empty body: status %d, want 400", code)
	}
}

// TestOpenAPI_ShapeRules proves the comparison on small schemas: it must
// report drift in both directions.
func TestOpenAPI_ShapeRules(t *testing.T) {
	type inner struct {
		A string `json:"a"`
	}
	type embedded struct {
		E int `json:"e"`
	}
	type sample struct {
		embedded
		Name  string     `json:"name"`
		Note  string     `json:"note,omitempty"`
		Seen  *time.Time `json:"seen"`
		Gone  *time.Time `json:"gone,omitempty"`
		At    time.Time  `json:"at,omitempty"` // omitempty has no effect on a struct
		Items []inner    `json:"items"`
		Free  map[string]any
		Skip  string `json:"-"`
	}
	const good = `
components:
  schemas:
    Inner:
      type: object
      required: [a]
      properties:
        a: { type: string }
    Sample:
      type: object
      required: [e, name, seen, at, items, Free]
      properties:
        e: { type: integer }
        name: { type: string }
        note: { type: string }
        seen: { type: string, format: date-time, nullable: true }
        gone: { type: string, format: date-time }
        at: { type: string, format: date-time }
        items:
          type: array
          items: { $ref: '#/components/schemas/Inner' }
        Free: { type: object, additionalProperties: true }
        computed: { type: string, readOnly: true }
`
	typ := reflect.TypeOf(sample{})
	ref := map[string]any{"$ref": "#/components/schemas/Sample"}
	edit := func(old, new string) openAPIDoc {
		if !strings.Contains(good, old) {
			t.Fatalf("fixture has no %q", old)
		}
		return parseOpenAPIDoc(t, []byte(strings.Replace(good, old, new, 1)))
	}

	// Requests: read-only properties are not part of a request.
	if errs := parseOpenAPIDoc(t, []byte(good)).checkRequestShape("sample", ref, typ); len(errs) != 0 {
		t.Errorf("request, good schema: %v", errs)
	}
	requestCases := []struct{ name, old, new, want string }{
		{"struct has a field the spec lacks", "        note: { type: string }\n", "", `the handler decodes "note"`},
		{"spec has a property the struct lacks", "        note: { type: string }\n", "        note: { type: string }\n        extra: { type: string }\n", `the spec has property "extra"`},
		{"nested drift", "        a: { type: string }\n", "        b: { type: string }\n", `sample.items: the handler decodes "a"`},
	}
	for _, tc := range requestCases {
		errs := edit(tc.old, tc.new).checkRequestShape("sample", ref, typ)
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), tc.want) {
			t.Errorf("request, %s: want an error containing %q, got %v", tc.name, tc.want, errs)
		}
	}

	// Responses: every property counts, and required and nullable follow the tags.
	respGood := strings.Replace(good, "        computed: { type: string, readOnly: true }\n", "", 1)
	if errs := parseOpenAPIDoc(t, []byte(respGood)).checkResponseShape("sample", ref, typ, nil); len(errs) != 0 {
		t.Errorf("response, good schema: %v", errs)
	}
	responseCases := []struct{ name, old, new, want string }{
		{"property never written", "        note: { type: string }\n", "        note: { type: string }\n        extra: { type: string }\n", `never writes`},
		{"always written, not required", "required: [e, name, seen, at, items, Free]", "required: [e, seen, at, items, Free]", `"name" is always written`},
		{"omitted, but required", "required: [e, name, seen, at, items, Free]", "required: [e, name, note, seen, at, items, Free]", `"note" can be left out`},
		{"null pointer not nullable", "seen: { type: string, format: date-time, nullable: true }", "seen: { type: string, format: date-time }", `"seen" is written as null`},
		{"omitted pointer marked nullable", "gone: { type: string, format: date-time }", "gone: { type: string, format: date-time, nullable: true }", `"gone" is left out when unset`},
		{"plain value marked nullable", "name: { type: string }", "name: { type: string, nullable: true }", `"name" is never null`},
		{"nested required", "      required: [a]\n", "", `sample.items: "a" is always written`},
	}
	for _, tc := range responseCases {
		doc := parseOpenAPIDoc(t, []byte(strings.Replace(respGood, tc.old, tc.new, 1)))
		if !strings.Contains(respGood, tc.old) {
			t.Fatalf("fixture has no %q", tc.old)
		}
		errs := doc.checkResponseShape("sample", ref, typ, nil)
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), tc.want) {
			t.Errorf("response, %s: want an error containing %q, got %v", tc.name, tc.want, errs)
		}
	}
	// A field named optional is exempt from `required`, and must not be in it.
	doc := parseOpenAPIDoc(t, []byte(strings.Replace(respGood, "required: [e, name, seen, at, items, Free]", "required: [e, seen, at, items, Free]", 1)))
	if errs := doc.checkResponseShape("sample", ref, typ, map[string]bool{"name": true}); len(errs) != 0 {
		t.Errorf("response, optional field: %v", errs)
	}

	// Key sets.
	keysDoc := parseOpenAPIDoc(t, []byte(good))
	innerRef := map[string]any{"$ref": "#/components/schemas/Inner"}
	if errs := keysDoc.checkKeys("keys", innerRef, []string{"a"}); len(errs) != 0 {
		t.Errorf("keys, good: %v", errs)
	}
	if errs := keysDoc.checkKeys("keys", innerRef, []string{"a", "b"}); len(errs) != 1 {
		t.Errorf("keys, handler knows more: %v", errs)
	}
	if errs := keysDoc.checkKeys("keys", innerRef, nil); len(errs) != 1 {
		t.Errorf("keys, spec has more: %v", errs)
	}
}
