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

	create, ok := r.Lookup("providerCreate")
	if !ok || create.Method != "POST" || create.Kind() != "write" || !create.BodyRequired ||
		!slices.Equal(create.Access.AnyOf, []authz.Permission{authz.PermAIProvidersManage}) {
		t.Fatalf("providerCreate = %+v", create)
	}
	in := create.InputSchema()
	props := in["properties"].(map[string]any)
	if _, ok := props["body"]; !ok {
		t.Error("providerCreate has no body argument")
	}
	if _, ok := props["dry_run"]; !ok {
		t.Error("providerCreate has no dry_run")
	}
	if _, ok := props["confirm"]; ok {
		t.Error("providerCreate asks for confirm")
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
