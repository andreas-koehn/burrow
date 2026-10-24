package mcpserv

import (
	"fmt"
	"os"
	"slices"
	"sort"
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

	create, ok := r.Lookup("budgetCreate")
	if !ok || create.Method != "POST" || create.Kind() != "write" || !create.BodyRequired ||
		!slices.Equal(create.Access.AnyOf, []authz.Permission{authz.PermBudgetsManage}) {
		t.Fatalf("budgetCreate = %+v", create)
	}
	in := create.InputSchema()
	props := in["properties"].(map[string]any)
	if _, ok := props["body"]; !ok {
		t.Error("budgetCreate has no body argument")
	}
	if _, ok := props["dry_run"]; !ok {
		t.Error("budgetCreate has no dry_run")
	}
	if _, ok := props["confirm"]; ok {
		t.Error("budgetCreate asks for confirm")
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
		// Fix round 1: writes that widen access or put a lasting secret or
		// instruction where a model reads it.
		"serviceAccessModeUpdate", "tunnelAccessModeUpdate", "gatewayKeyUpdate",
		"clientTokenCreate", "serviceApiKeyCreate",
		"userCreate", "userUpdate", "userDelete", "roleCreate", "roleUpdate", "roleDelete",
		"aiModelCreate", "aiModelUpdate",
		// It sets a new service's access mode as it creates it.
		"serviceCreate",
		// Native tcp: an agent must not let an address (its own) reach a
		// port; the tcp modes go through serviceAccessModeUpdate.
		"serviceIpGrantsList", "serviceIpGrantCreate", "serviceIpGrantDelete",
		// Fix round 1 of native tcp: the ip lists of a tcp service in mode ip
		// and the roles that may reach a service (burrow_login, and who may
		// forward to a private tcp service) widen access too.
		"serviceIpGeoUpdate", "serviceAccessPolicyUpdate",
		// tcp complete: the PROXY header changes what a service's target is
		// told about its visitors (the source it may trust), and an idle
		// timeout cuts connections off.
		"serviceTcpSettingsUpdate",
		// A TLS passthrough host name gives a tcp service a way in.
		"serviceTcpHostnameUpdate",
		// Lifting a tcp service's limits or a ban lets more in.
		"serviceTcpLimitsUpdate", "serviceTcpBanDelete",
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
		"serviceIpGeoGet", "serviceAccessPolicyGet", "automationTokenCreate", "serviceTcpSettingsGet", "serviceTcpHostnameGet",
		"serviceTcpLimitsGet", "serviceTcpBansList",
		"aiModelsList", "gatewayKeysList", "usersList", "rolesList",
	} {
		if _, ok := r.Lookup(id); !ok {
			t.Errorf("%s is not a management tool", id)
		}
	}
}

// mcpExposedWrites is the golden set of every operation that is not a GET
// and is a /mcp/burrow tool. A write added to the API fails
// TestRegistry_WritesAreClassified until somebody classifies it: adds it
// here, because an agent steered by untrusted text may call it, or marks it
// x-burrow-mcp: false. A name here that is no exposed write fails too.
var mcpExposedWrites = map[string]bool{
	"aiModelDelete":                 true,
	"auditVerify":                   true,
	"automationTokenCreate":         true,
	"automationTokenDelete":         true,
	"backupCreate":                  true,
	"backupDelete":                  true,
	"backupVerify":                  true,
	"budgetCreate":                  true,
	"budgetDelete":                  true,
	"budgetUpdate":                  true,
	"cacheEntriesDelete":            true,
	"cacheSemanticEntriesDelete":    true,
	"cacheSettingsUpdate":           true,
	"clientTokenDelete":             true,
	"costPricingUpdate":             true,
	"gatewayKeyDelete":              true,
	"guardrailPreview":              true,
	"inspectorRequestReplay":        true,
	"inspectorRequestReplayCompare": true,
	"mcpSourceDelete":               true,
	"mcpSourceSync":                 true,
	"oauthClientDelete":             true,
	"providerDelete":                true,
	"providerModelCreate":           true,
	"providerModelDelete":           true,
	"providerModelsSync":            true,
	"rateLimitCreate":               true,
	"rateLimitDelete":               true,
	"rateLimitUpdate":               true,
	"redactionPreview":              true,
	"serviceApiKeyDelete":           true,
	"serviceCacheEntriesDelete":     true,
	"serviceDelete":                 true,
	"serviceDomainDelete":           true,
	"serviceMaxConnectionsUpdate":   true, // a cap of 1 to 256 visitors; widens nobody's access
	"serviceSlugUpdate":             true,
	"serviceTcpBanCreate":           true, // bans one source of a tcp service for at most 30 days; narrows access, never a forward
	"sessionDelete":                 true,
	"sessionsRevokeAll":             true,
	"settingsTestEmail":             true,
	"webhookPreview":                true,
}

// mcpSensitiveAllowed are exposed writes the heuristic below flags that stay
// management tools on purpose, each with the reason.
var mcpSensitiveAllowed = map[string]string{
	"redactionPreview": "runs a body through the rules and changes nothing",
	"guardrailPreview": "tries a pattern on a text and changes nothing",
	"webhookPreview":   "renders a webhook's payload template with given fields and sends nothing",
	// A replay goes to the same service as the captured request, under
	// inspector:replay; it changes no setting and sends nothing elsewhere.
	"inspectorRequestReplay":        "replays a captured request to its own service; changes no setting",
	"inspectorRequestReplayCompare": "replays a captured request to its own service; changes no setting",
	// The permissions of a token minted by a token are at most the calling
	// token's own (and its expiry no later), see PostAutomationToken.
	"automationTokenCreate": "mints a token with at most the calling token's permissions and expiry; widens nothing",
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

// sensitivePathSegments: a write under a path segment that starts with one
// of these changes what is inspected, redacted, refused, kept or where a
// credential goes.
var sensitivePathSegments = []string{"inspector", "guardrails", "redaction", "retention", "ai-config", "upstream", "webhooks"}

// sensitiveWrite says why op looks like it could re-route a credential or
// switch off inspection, or "" when nothing in it looks like that. It is the
// reason text of TestRegistry_WritesAreClassified, not the gate: the golden
// set is.
func sensitiveWrite(op Operation) string {
	if op.Method == "GET" {
		return ""
	}
	for _, seg := range strings.Split(op.Path, "/") {
		for _, p := range sensitivePathSegments {
			if strings.HasPrefix(seg, p) {
				return "its path has the segment /" + seg
			}
		}
	}
	names := map[string]bool{}
	bodyPropertyNames(op.Body, names)
	var address, credential []string
	for n := range names {
		switch {
		case n == "url" || strings.HasSuffix(n, "_url") || n == "host" || strings.HasSuffix(n, "_host") || strings.HasSuffix(n, ".host") ||
			n == "endpoint" || strings.HasSuffix(n, "_endpoint") || n == "uri" || strings.HasSuffix(n, "_uri") || strings.HasSuffix(n, "_uris"):
			address = append(address, n)
		case n == "token" || strings.HasSuffix(n, "_token") || n == "api_key" || strings.HasSuffix(n, "_api_key") || n == "secret" ||
			strings.HasSuffix(n, "_secret") || n == "credential_slot" || n == "slot" || n == "auth_header" || n == "header_name" ||
			n == "extra_headers" || strings.HasSuffix(n, "username") || strings.HasSuffix(n, "password") || n == "key_pem":
			credential = append(credential, n)
		}
	}
	sort.Strings(address)
	sort.Strings(credential)
	switch {
	case names["base_url"]:
		return "the body has base_url"
	case len(credential) > 0:
		return "the body has a credential field (" + strings.Join(credential, ", ") + ")"
	case len(address) > 0:
		return "the body has an address (" + strings.Join(address, ", ") + ")"
	}
	// Who may reach a service (access_mode, mtls_ca_pem) and what a person
	// or a role may do (role, permissions) are as sensitive as inspection.
	for _, n := range []string{"redaction", "guardrails", "inspector", "gateway_only", "presidio_url", "redact_for_logs_only",
		"custom_patterns", "inspector_retention_count", "audit_retention_days", "redaction_retention_days", "system_text",
		"access_mode", "mtls_ca_pem", "permissions", "role"} {
		if names[n] {
			return "the body has " + n
		}
	}
	return ""
}

// classifyWrites is the rule of TestRegistry_WritesAreClassified.
func classifyWrites(r *Registry, golden map[string]bool, allowed map[string]string) []string {
	var errs []string
	exposed := map[string]bool{}
	for _, op := range r.Operations() {
		if op.Method == "GET" {
			continue
		}
		exposed[op.ID] = true
		why := sensitiveWrite(op)
		if !golden[op.ID] {
			msg := fmt.Sprintf("%s (%s %s) is a write exposed as a /mcp/burrow tool that nobody classified: add it to mcpExposedWrites if an agent steered by untrusted text may call it, or mark it x-burrow-mcp: false", op.ID, op.Method, op.Path)
			if why != "" {
				msg += "; it looks sensitive: " + why + " (an agent must not re-route credentials or switch off inspection)"
			}
			errs = append(errs, msg)
			continue
		}
		if why != "" && allowed[op.ID] == "" {
			errs = append(errs, fmt.Sprintf("%s (%s %s) is a management tool although %s: mark it x-burrow-mcp: false (an agent must not re-route credentials or switch off inspection) or add it to mcpSensitiveAllowed with the reason", op.ID, op.Method, op.Path, why))
		}
	}
	for id := range golden {
		if !exposed[id] {
			errs = append(errs, "mcpExposedWrites names "+id+", which is no write exposed as a tool: drop it")
		}
	}
	for id := range allowed {
		if !exposed[id] {
			errs = append(errs, "mcpSensitiveAllowed names "+id+", which is no write exposed as a tool: drop it")
		}
	}
	sort.Strings(errs)
	return errs
}

// TestRegistry_WritesAreClassified is the guard for operations added later:
// every write that is a management tool is in the golden set, and one that
// looks like it routes a credential or changes inspection also has a reason
// in mcpSensitiveAllowed.
func TestRegistry_WritesAreClassified(t *testing.T) {
	r, err := NewRegistry(realSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range classifyWrites(r, mcpExposedWrites, mcpSensitiveAllowed) {
		t.Error(e)
	}
}

// The reviewer's probes: writes of shapes the heuristic of round 2 missed,
// and two real operations with their x-burrow-mcp: false taken away, are
// each refused, and the heuristic names a reason for the shapes.
func TestRegistry_GuardCatchesTheProbes(t *testing.T) {
	for name, paths := range map[string]string{
		"host and api_key": `
  /api/v1/ldap:
    put:
      operationId: ldapUpdate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { host: { type: string }, api_key: { type: string } } } } } }
      responses: { '200': { description: ok } }
`,
		"endpoint and token": `
  /api/v1/exporters:
    post:
      operationId: exporterCreate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { endpoint: { type: string }, token: { type: string } } } } } }
      responses: { '200': { description: ok } }
`,
		"an alert channel with a url only": `
  /api/v1/alerts/channels:
    post:
      operationId: alertChannelCreate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { url: { type: string }, events: { type: string } } } } } }
      responses: { '200': { description: ok } }
`,
		"inspector settings": `
  /api/v1/inspector/settings:
    put:
      operationId: inspectorSettingsUpdate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { enabled: { type: boolean } } } } } }
      responses: { '200': { description: ok } }
`,
		"deleting a service's guardrails": `
  /api/v1/services/{slug}/guardrails:
    parameters: [{ $ref: '#/components/parameters/PathSlug' }]
    delete:
      operationId: serviceGuardrailsDelete
      x-burrow-permission: {kind: self, any_of: []}
      responses: { '204': { description: ok } }
`,
		// Round 2 deferred: who may reach a service, the CA its mTLS trusts,
		// and what a role may do.
		"access mode": `
  /api/v1/gizmos/{slug}/access:
    parameters: [{ $ref: '#/components/parameters/PathSlug' }]
    put:
      operationId: gizmoAccessUpdate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { access_mode: { type: string } } } } } }
      responses: { '200': { description: ok } }
`,
		"an mTLS CA": `
  /api/v1/gizmos/{slug}/trust:
    parameters: [{ $ref: '#/components/parameters/PathSlug' }]
    put:
      operationId: gizmoTrustUpdate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { mtls_ca_pem: { type: string } } } } } }
      responses: { '200': { description: ok } }
`,
		"a role's permissions": `
  /api/v1/teams:
    post:
      operationId: teamCreate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { name: { type: string }, permissions: { type: array, items: { type: string } } } } } } }
      responses: { '200': { description: ok } }
`,
		"a member's role": `
  /api/v1/members:
    post:
      operationId: memberCreate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { type: object, properties: { email: { type: string }, role: { type: string } } } } } }
      responses: { '200': { description: ok } }
`,
		"base_url nested under allOf": `
  /api/v1/things:
    post:
      operationId: thingCreate
      x-burrow-permission: {kind: self, any_of: []}
      requestBody: { content: { application/json: { schema: { allOf: [ { type: object, properties: { target: { type: object, properties: { base_url: { type: string } } } } } ] } } } }
      responses: { '200': { description: ok } }
`,
	} {
		mr, err := NewRegistry([]byte(miniHead + paths))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ops := mr.Operations()
		if len(ops) != 1 || sensitiveWrite(ops[0]) == "" {
			t.Errorf("%s: the heuristic gives no reason for %+v", name, ops)
			continue
		}
		if errs := classifyWrites(mr, map[string]bool{}, nil); len(errs) != 1 || !strings.Contains(errs[0], ops[0].ID) || !strings.Contains(errs[0], "looks sensitive") {
			t.Errorf("%s: the guard says %v", name, errs)
		}
	}

	// Real operations whose flag somebody removed.
	spec := string(realSpec(t))
	for _, id := range []string{"providerUpdate", "serviceUpstreamCredentialDelete", "aiModelUpdate", "serviceAccessModeUpdate"} {
		i := strings.Index(spec, "operationId: "+id+"\n")
		if i < 0 {
			t.Fatalf("no operation %s", id)
		}
		j := i + strings.Index(spec[i:], "x-burrow-mcp: false\n")
		j = strings.LastIndex(spec[:j], "\n") + 1 // the start of that line
		k := j + strings.Index(spec[j:], "\n") + 1
		stripped := spec[:j] + spec[k:]
		mr, err := NewRegistry([]byte(stripped))
		if err != nil {
			t.Fatalf("%s without its flag: %v", id, err)
		}
		errs := classifyWrites(mr, mcpExposedWrites, mcpSensitiveAllowed)
		if len(errs) != 1 || !strings.Contains(errs[0], id+" (") {
			t.Errorf("%s without its flag: the guard says %v", id, errs)
		}
	}
}
