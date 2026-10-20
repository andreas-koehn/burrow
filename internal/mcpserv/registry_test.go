package mcpserv

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
)

// realSpec is the document the relay embeds.
func realSpec(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const miniHead = `openapi: 3.0.3
info: { title: t, version: "1" }
components:
  parameters:
    PathSlug: { name: slug, in: path, required: true, schema: { type: string } }
  schemas:
    Thing: { type: object, required: [name], properties: { name: { type: string } } }
paths:
`

func TestRegistryFromTheRealSpec(t *testing.T) {
	r, err := NewRegistry(realSpec(t))
	if err != nil {
		t.Fatalf("the relay's own spec does not build a registry: %v", err)
	}
	ops := r.Operations()
	if len(ops) < 100 {
		t.Fatalf("only %d operations", len(ops))
	}
	names := map[string]bool{}
	for _, op := range ops {
		if op.Access.Kind == "dashboard" {
			t.Errorf("%s is dashboard-only and must not be an operation", op.ID)
		}
		n := ToolName(op.ID)
		if names[n] {
			t.Errorf("tool name %s twice", n)
		}
		names[n] = true
	}
	for _, id := range []string{"authLogin", "backupRestore", "eventsStream", "inspectorStream", "mcpManagement"} {
		if _, ok := r.Lookup(id); ok {
			t.Errorf("%s is excluded in the spec but is in the registry", id)
		}
	}

	create, ok := r.Lookup("aiModelCreate")
	if !ok || create.Method != "POST" || create.Kind() != "write" || !create.BodyRequired ||
		!slices.Equal(create.Access.AnyOf, []authz.Permission{authz.PermAIConfigureAny}) {
		t.Fatalf("aiModelCreate = %+v", create)
	}
	in := create.InputSchema()
	props := in["properties"].(map[string]any)
	if _, ok := props["body"]; !ok {
		t.Error("aiModelCreate has no body argument")
	}
	if _, ok := props["dry_run"]; !ok {
		t.Error("aiModelCreate has no dry_run")
	}
	if _, ok := props["confirm"]; ok {
		t.Error("aiModelCreate asks for confirm")
	}

	del, _ := r.Lookup("providerDelete")
	if del.Kind() != "delete" || del.ConfirmValueName() != "slug" {
		t.Errorf("providerDelete kind %s confirm %q", del.Kind(), del.ConfirmValueName())
	}
	if req := del.InputSchema()["required"].([]string); !slices.Contains(req, "confirm") || !slices.Contains(req, "slug") {
		t.Errorf("providerDelete required = %v", req)
	}

	list, _ := r.Lookup("providersList")
	if list.Kind() != "read" {
		t.Errorf("providersList kind %s", list.Kind())
	}
	if _, ok := list.InputSchema()["properties"].(map[string]any)["dry_run"]; ok {
		t.Error("a GET has dry_run")
	}

	me, _ := r.Lookup("meGet")
	if me.Access.Kind != "self" {
		t.Errorf("meGet access %+v", me.Access)
	}
	metrics, ok := r.Lookup("metricsGet")
	if !ok || !slices.Equal(metrics.Access.AnyOf, []authz.Permission{authz.PermMetricsRead}) {
		t.Errorf("metricsGet = %+v, %v", metrics.Access, ok)
	}
}

func TestRegistryRules(t *testing.T) {
	cases := []struct {
		name, paths, wantErr string
	}{
		{"no permission", `
  /api/v1/things:
    get: { operationId: thingsList, responses: { '200': { description: ok } } }
`, "thingsList: no x-burrow-permission"},
		{"header parameter", `
  /api/v1/things:
    get:
      operationId: thingsList
      x-burrow-permission: {kind: self, any_of: []}
      parameters: [{ name: X-Thing, in: header, schema: { type: string } }]
      responses: { '200': { description: ok } }
`, "parameter X-Thing is in header"},
		{"reserved name", `
  /api/v1/things:
    get:
      operationId: thingsList
      x-burrow-permission: {kind: self, any_of: []}
      parameters: [{ name: confirm, in: query, schema: { type: string } }]
      responses: { '200': { description: ok } }
`, "parameter confirm is reserved"},
		{"multipart body", `
  /api/v1/things:
    post:
      operationId: thingCreate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { multipart/form-data: { schema: { type: object } } } }
      responses: { '200': { description: ok } }
`, "thingCreate: body is not application/json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewRegistry([]byte(miniHead + c.paths))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %v, want an error containing %q", err, c.wantErr)
			}
		})
	}

	ok := miniHead + `
  /api/v1/things/{slug}:
    parameters: [{ $ref: '#/components/parameters/PathSlug' }]
    get:
      operationId: thingGet
      x-burrow-permission: {kind: needs, any_of: ["ai:read:own", "ai:read:any"]}
      parameters: [{ name: verbose, in: query, schema: { type: boolean }, description: More fields. }]
      responses: { '200': { description: ok } }
    put:
      operationId: thingUpdate
      x-burrow-permission: {kind: needs, any_of: ["ai:configure:any"]}
      requestBody: { required: true, content: { application/json: { schema: { $ref: '#/components/schemas/Thing' } } } }
      responses: { '200': { description: ok } }
    delete:
      operationId: thingDelete
      x-burrow-permission: {kind: needs, any_of: ["ai:configure:any"]}
      responses: { '204': { description: gone } }
  /api/v1/things/reset:
    post:
      operationId: thingsReset
      x-burrow-permission: {kind: needs, every: true, any_of: ["settings:manage"]}
      x-burrow-destructive: true
      responses: { '204': { description: gone } }
  /api/v1/things/live:
    get:
      operationId: thingsLive
      x-burrow-permission: {kind: dashboard, any_of: ["ai:read:any"]}
      responses: { '200': { description: ok } }
  /api/v1/things/hidden:
    get:
      operationId: thingsHidden
      x-burrow-mcp: false
      responses: { '200': { description: ok } }
`
	r, err := NewRegistry([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, op := range r.Operations() {
		ids = append(ids, op.ID)
	}
	if want := []string{"thingsReset", "thingGet", "thingUpdate", "thingDelete"}; !slices.Equal(ids, want) {
		t.Errorf("operations = %v, want %v (by path, then get put delete)", ids, want)
	}
	get, _ := r.Lookup("thingGet")
	if len(get.Params) != 2 || get.Params[0].Name != "slug" || get.Params[0].In != "path" || !get.Params[0].Required || get.Params[1].Name != "verbose" {
		t.Errorf("thingGet params = %+v", get.Params)
	}
	verbose := get.InputSchema()["properties"].(map[string]any)["verbose"].(map[string]any)
	if verbose["description"] != "More fields." {
		t.Errorf("parameter description not carried: %v", verbose)
	}
	reset, _ := r.Lookup("thingsReset")
	if !reset.Destructive || reset.Kind() != "delete" || reset.ConfirmValueName() != "" || !reset.Access.Every {
		t.Errorf("thingsReset = %+v", reset)
	}
	upd, _ := r.Lookup("thingUpdate")
	if req := upd.InputSchema()["required"].([]string); !slices.Equal(req, []string{"slug", "body"}) {
		t.Errorf("thingUpdate required = %v", req)
	}
}

func TestToolName(t *testing.T) {
	for in, want := range map[string]string{
		"providerCreate": "provider_create", "meGet": "me_get",
		"providerModelsSync": "provider_models_sync", "aiRequestsList": "ai_requests_list",
	} {
		if got := ToolName(in); got != want {
			t.Errorf("ToolName(%s) = %s, want %s", in, got, want)
		}
	}
}

// Final review: tool text a source's upstream wrote may steer an agent, so
// the management server offers neither the writes that point a source at a
// host and a credential nor the list of tool definitions. The reads of the
// sources stay.
func TestRegistry_ToolSourceWritesAreNotTools(t *testing.T) {
	r, err := NewRegistry(realSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mcpSourceCreate", "mcpSourceUpdate", "mcpSourceToolsList", "mcpSourceToolApprove", "mcpSourceApproveAll", "mcpSourceToolUpdate"} {
		if _, ok := r.Lookup(id); ok {
			t.Errorf("%s is a management tool", id)
		}
	}
	for _, id := range []string{"mcpSourcesList", "mcpSourceGet"} {
		if _, ok := r.Lookup(id); !ok {
			t.Errorf("%s is not a management tool", id)
		}
	}
}

// Round 2: an agent steered by untrusted text (a tool result, a model answer,
// a sync error) must not re-route a credential nor switch off inspection
// through /mcp/burrow. None of these writes is a tool; their reads stay.
func TestRegistry_CredentialRoutingAndInspectionWritesAreNotTools(t *testing.T) {
	r, err := NewRegistry(realSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		// credentials routed to an address
		"providerCreate", "providerUpdate", "providerUpstreamUpdate",
		"serviceUpstreamCredentialUpdate", "serviceUpstreamCredentialDelete",
		"settingsUpdate",
		"serviceDomainCreate", "serviceDomainUpdate",
		// outbound webhooks
		"webhookCreate", "webhookUpdate", "webhookDelete", "webhookTest", "webhookPause", "webhookResume",
		// inspection: guardrails, redaction, the inspector, retention, the gateway's own checks
		"guardrailSettingsUpdate",
		"redactionSettingsUpdate", "redactionRuleCreate", "redactionRuleUpdate", "redactionRuleDelete",
		"serviceAiConfigUpdate", "retentionSettingsUpdate", "serviceGatewayOnlyUpdate",
	} {
		if _, ok := r.Lookup(id); ok {
			t.Errorf("%s is a management tool", id)
		}
	}
	for _, id := range []string{
		"providersList", "providerGet", "serviceUpstreamCredentialGet", "settingsGet",
		"serviceDomainsList", "serviceDomainGet", "webhooksList", "webhookDeliveriesList",
		"guardrailSettingsGet", "redactionSettingsGet", "redactionRulesList", "serviceAiConfigGet",
		"retentionSettingsGet", "redactionPreview", "guardrailPreview", "webhookPreview",
	} {
		if _, ok := r.Lookup(id); !ok {
			t.Errorf("%s is not a management tool", id)
		}
	}
}

// mcpSensitiveAllowed are operations the guard below flags that stay
// management tools on purpose, each with the reason.
var mcpSensitiveAllowed = map[string]string{
	"redactionPreview": "runs a body through the rules and changes nothing",
	"guardrailPreview": "tries a pattern on a text and changes nothing",
	"webhookPreview":   "renders a webhook's payload template with given fields and sends nothing",
}

// bodyPropertyNames collects every property name of a JSON Schema, nested
// objects, arrays and combinations included.
func bodyPropertyNames(s any, into map[string]bool) {
	switch v := s.(type) {
	case map[string]any:
		if props, ok := v["properties"].(map[string]any); ok {
			for k, p := range props {
				into[k] = true
				bodyPropertyNames(p, into)
			}
		}
		for _, k := range []string{"items", "additionalProperties"} {
			bodyPropertyNames(v[k], into)
		}
		for _, k := range []string{"allOf", "oneOf", "anyOf"} {
			if list, ok := v[k].([]any); ok {
				for _, x := range list {
					bodyPropertyNames(x, into)
				}
			}
		}
	case []any:
		for _, x := range v {
			bodyPropertyNames(x, into)
		}
	}
}

// sensitiveWrite says why op could re-route a credential or switch off
// inspection, or "" when nothing in it looks like that.
func sensitiveWrite(op Operation) string {
	if op.Method == "GET" {
		return ""
	}
	for _, p := range []string{"/api/v1/redaction/", "/api/v1/guardrails/", "/api/v1/webhooks"} {
		if strings.HasPrefix(op.Path, p) {
			return "a redaction, guardrail or webhook write (" + p + ")"
		}
	}
	names := map[string]bool{}
	bodyPropertyNames(op.Body, names)
	address, credential := false, false
	for n := range names {
		if n == "url" || strings.HasSuffix(n, "_url") || strings.HasSuffix(n, ".host") {
			address = true
		}
		if n == "auth_header" || n == "header_name" || n == "extra_headers" || strings.HasSuffix(n, "username") || strings.HasSuffix(n, "password") {
			credential = true
		}
	}
	switch {
	case names["base_url"]:
		return "the body has base_url"
	case names["credential_slot"] || names["slot"]:
		return "the body names a credential slot"
	case address && credential:
		return "the body has an address together with a credential"
	}
	for _, n := range []string{"redaction", "guardrails", "inspector", "gateway_only", "presidio_url", "redact_for_logs_only",
		"custom_patterns", "inspector_retention_count", "audit_retention_days", "redaction_retention_days", "key_pem"} {
		if names[n] {
			return "the body has " + n
		}
	}
	return ""
}

// TestRegistry_SensitiveWritesNeedAnAllowList is the guard for operations
// added later: a write whose body routes a credential to an address, or that
// changes redaction, guardrails or inspection, is not a management tool
// unless mcpSensitiveAllowed names it.
func TestRegistry_SensitiveWritesNeedAnAllowList(t *testing.T) {
	r, err := NewRegistry(realSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[string]bool{}
	for _, op := range r.Operations() {
		why := sensitiveWrite(op)
		if why == "" {
			continue
		}
		flagged[op.ID] = true
		if _, ok := mcpSensitiveAllowed[op.ID]; !ok {
			t.Errorf("%s (%s %s) is a management tool although %s: mark it x-burrow-mcp: false (an agent must not re-route credentials or switch off inspection) or add it to mcpSensitiveAllowed with the reason", op.ID, op.Method, op.Path, why)
		}
	}
	for id := range mcpSensitiveAllowed {
		if !flagged[id] {
			t.Errorf("mcpSensitiveAllowed names %s, which is no flagged management tool: drop it", id)
		}
	}

	// The guard itself flags each kind of body.
	for name, paths := range map[string]string{
		"base_url": `
  /api/v1/things:
    post:
      operationId: thingCreate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { base_url: { type: string } } } } } }
      responses: { '200': { description: ok } }
`,
		"url and slot": `
  /api/v1/things:
    put:
      operationId: thingUpdate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { target: { type: object, properties: { url: { type: string }, credential_slot: { type: string } } } } } } } }
      responses: { '200': { description: ok } }
`,
		"inspection": `
  /api/v1/things/{slug}/settings:
    parameters: [{ $ref: '#/components/parameters/PathSlug' }]
    put:
      operationId: thingSettingsUpdate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { allOf: [ { type: object, properties: { guardrails: { type: object } } } ] } } } }
      responses: { '200': { description: ok } }
`,
	} {
		mr, err := NewRegistry([]byte(miniHead + paths))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ops := mr.Operations()
		if len(ops) != 1 || sensitiveWrite(ops[0]) == "" {
			t.Errorf("%s: the guard does not flag %+v", name, ops)
		}
	}
}
