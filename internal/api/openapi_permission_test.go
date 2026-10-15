package api

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// updateSpecEnv makes TestSpecDeclaresEachRoutesAccess write the
// x-burrow-permission lines into docs/openapi.yaml instead of comparing.
const updateSpecEnv = "BURROW_UPDATE_SPEC_PERMISSIONS"

// outsideAPIAccess declares the operations outside /api/v1 that carry an
// x-burrow-permission. The router gates them with require(); this table is
// what the spec must say about them.
var outsideAPIAccess = map[string]access{
	"GET /metrics": needs("metrics:read"),
}

// specKind is the name of an access kind in the spec.
func specKind(a access) string {
	switch a.kind {
	case accessPublic:
		return "public"
	case accessSelf:
		return "self"
	case accessDashboard:
		return "dashboard"
	default:
		return "needs"
	}
}

// specPermissionLine is the line the spec carries for a, without indentation.
func specPermissionLine(a access) string {
	perms := make([]string, len(a.perms))
	for i, p := range a.perms {
		perms[i] = fmt.Sprintf("%q", string(p))
	}
	every := ""
	if a.every {
		every = ", every: true"
	}
	return fmt.Sprintf("x-burrow-permission: {kind: %s%s, any_of: [%s]}", specKind(a), every, strings.Join(perms, ", "))
}

type specOperation struct {
	ID         string
	Key        string // "GET /api/v1/ai/providers"
	Permission *struct {
		Kind  string   `yaml:"kind"`
		Every bool     `yaml:"every"`
		AnyOf []string `yaml:"any_of"`
	}
	Destructive bool
}

func specOperations(t *testing.T, doc []byte) map[string]specOperation {
	t.Helper()
	var parsed struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	out := map[string]specOperation{}
	for path, methods := range parsed.Paths {
		for method, node := range methods {
			if node.Kind != yaml.MappingNode {
				continue // path-level "parameters" is a sequence
			}
			var op struct {
				OperationID string `yaml:"operationId"`
				Permission  *struct {
					Kind  string   `yaml:"kind"`
					Every bool     `yaml:"every"`
					AnyOf []string `yaml:"any_of"`
				} `yaml:"x-burrow-permission"`
				Destructive bool `yaml:"x-burrow-destructive"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatalf("parse %s %s: %v", method, path, err)
			}
			if op.OperationID == "" {
				continue
			}
			key := strings.ToUpper(method) + " " + path
			out[key] = specOperation{ID: op.OperationID, Key: key, Permission: op.Permission, Destructive: op.Destructive}
		}
	}
	return out
}

// TestSpecDeclaresEachRoutesAccess: every route of routeAccess and of
// outsideAPIAccess has an operation in the spec whose x-burrow-permission
// says the same; no other operation carries one.
func TestSpecDeclaresEachRoutesAccess(t *testing.T) {
	want := map[string]access{}
	for k, a := range routeAccess {
		want[k] = a
	}
	for k, a := range outsideAPIAccess {
		want[k] = a
	}

	if os.Getenv(updateSpecEnv) == "1" {
		writeSpecPermissions(t, "../../docs/openapi.yaml", want)
	}

	ops := specOperations(t, openapiYAML)
	for key, a := range want {
		op, ok := ops[key]
		if !ok {
			// TestOpenAPI_RouteCoverage reports a route missing from the
			// spec; the routes it excludes on purpose are not here.
			continue
		}
		if op.Permission == nil {
			t.Errorf("%s (%s): no x-burrow-permission; want %s", op.ID, key, specPermissionLine(a))
			continue
		}
		got := op.Permission
		wantPerms := make([]string, len(a.perms))
		for i, p := range a.perms {
			wantPerms[i] = string(p)
		}
		if got.Kind != specKind(a) || got.Every != a.every || !slices.Equal(got.AnyOf, wantPerms) {
			t.Errorf("%s (%s): spec says %+v; routeAccess says %s", op.ID, key, *got, specPermissionLine(a))
		}
	}
	for key, op := range ops {
		if _, declared := want[key]; !declared && op.Permission != nil {
			t.Errorf("%s (%s) carries x-burrow-permission but no route declares it", op.ID, key)
		}
		if strings.HasPrefix(key, "DELETE ") && op.Destructive {
			t.Errorf("%s: x-burrow-destructive on a DELETE is redundant; remove it", op.ID)
		}
	}
}

var operationIDLine = regexp.MustCompile(`^(\s*)operationId:\s*(\S+)\s*$`)

// writeSpecPermissions puts one x-burrow-permission line below every
// operationId line of the operations in want, replacing an existing one.
func writeSpecPermissions(t *testing.T, path string, want map[string]access) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	byID := map[string]access{}
	for key, op := range specOperations(t, raw) {
		if a, ok := want[key]; ok {
			byID[op.ID] = a
		}
	}
	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines)+len(byID))
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		m := operationIDLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		a, ok := byID[m[2]]
		if !ok {
			continue
		}
		if i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "x-burrow-permission:") {
			i++ // replaced below
		}
		out = append(out, m[1]+specPermissionLine(a))
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote x-burrow-permission for %d operations into %s; copy it to internal/api/openapi.yaml", len(byID), path)
}
