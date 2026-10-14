package api

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateAuthzMatrix = flag.Bool("update-authz-matrix", false,
	"rewrite testdata/authz_cookie_matrix.golden from what the router answers now")

const authzMatrixGolden = "testdata/authz_cookie_matrix.golden"

// cookieMatrix asks every /api/v1 route once per role with a dashboard
// session and returns one line per cell: "<role> <METHOD> <pattern> <status>".
// Each role gets its own harness, so that what one role changes is not seen
// by the next.
func cookieMatrix(t *testing.T) []string {
	t.Helper()
	var lines []string
	for _, role := range authzRoles {
		h := newAuthzHarness(t)
		for _, r := range h.apiRoutes(t) {
			rec := h.call(t, r, h.session(t, role))
			lines = append(lines, fmt.Sprintf("%s %s %d", role, r.key(), rec.Code))
		}
	}
	return lines
}

// intendedCookieChanges lists every cell of the matrix that the permission
// gate changed on purpose: "<role> <METHOD> <pattern>" → the status now. They
// are cells of the two custom roles only; no user could hold a custom role
// when the golden file was recorded. What the built-in roles may call is
// unchanged.
//
// "cr-legacy" gained routes that only the admin role could call and that now
// ask for a permission that existed before; it holds every one of those.
// "cr-none" lost every route outside the allow-list: a role's permissions
// bind its dashboard session too, and this role holds none. Before, a session
// passed wherever no handler or store asked for a permission of its own.
var intendedCookieChanges = map[string]string{
	// users:read / users:manage
	"cr-legacy GET /api/v1/users":         "200",
	"cr-legacy POST /api/v1/users":        "400",
	"cr-legacy PATCH /api/v1/users/{id}":  "400",
	"cr-legacy DELETE /api/v1/users/{id}": "404",
	// roles:read / roles:manage
	"cr-legacy GET /api/v1/roles":        "200",
	"cr-legacy GET /api/v1/roles/{name}": "404",
	// settings:manage
	"cr-legacy GET /api/v1/settings":             "200",
	"cr-legacy PUT /api/v1/settings":             "204",
	"cr-legacy POST /api/v1/settings/test-email": "400",
	"cr-legacy GET /api/v1/settings/retention":   "200",
	"cr-legacy PUT /api/v1/settings/retention":   "204",
	// tunnels:read:any
	"cr-legacy GET /api/v1/clients":             "200",
	"cr-legacy GET /api/v1/clients/known":       "200",
	"cr-legacy GET /api/v1/clients/{sessionID}": "404",
	// services:configure:any
	"cr-legacy POST /api/v1/services": "400",
	// ai:read:any
	"cr-legacy GET /api/v1/ai/requests/{requestID}/attempts": "200",
	// Restoring takes every permission; the role lacks the three of v0.10.
	"cr-legacy POST /api/v1/backups/restore": "403", // was 400

	// tokens:manage:own / :any
	"cr-none GET /api/v1/clients/connect-info":                    "403", // was 200
	"cr-none GET /api/v1/tokens":                                  "403", // was 200
	"cr-none POST /api/v1/tokens":                                 "403", // was 400
	"cr-none DELETE /api/v1/tokens/{id}":                          "403", // was 404
	"cr-none GET /api/v1/client/login/requests/{user_code}":       "403", // was 404
	"cr-none POST /api/v1/client/login/requests/{user_code}/deny": "403", // was 404
	// sessions:manage:own / :any
	"cr-none GET /api/v1/sessions":             "403", // was 200
	"cr-none DELETE /api/v1/sessions/{id}":     "403", // was 404
	"cr-none POST /api/v1/sessions/revoke-all": "403", // was 200
	// tunnels:read:own / :any
	"cr-none GET /api/v1/tunnels": "403", // was 200
	"cr-none GET /api/v1/events":  "403", // was 200
	// services:configure:own / :any
	"cr-none GET /api/v1/services":                          "403", // was 200
	"cr-none GET /api/v1/services/slug-suggestion":          "403", // was 200
	"cr-none PUT /api/v1/services/{serviceID}/access-mode":  "403", // was 400
	"cr-none PUT /api/v1/services/{serviceID}/gateway-only": "403", // was 400
	"cr-none POST /api/v1/services/{serviceID}/api-keys":    "403", // was 400
	"cr-none PUT /api/v1/tunnels/{id}/access-mode":          "403", // was 400
	// ipgeo:manage:own / :any
	"cr-none GET /api/v1/geo/status": "403", // was 200
	// ai:read:own / :any, ai:configure:own / :any
	"cr-none GET /api/v1/ai/gateway":          "403", // was 200
	"cr-none GET /api/v1/ai/models":           "403", // was 200
	"cr-none GET /api/v1/ai/models/{name}":    "403", // was 404
	"cr-none GET /api/v1/ai/keys":             "403", // was 200
	"cr-none GET /api/v1/cache/settings":      "403", // was 200
	"cr-none GET /api/v1/cache/stats":         "403", // was 200
	"cr-none GET /api/v1/redaction/rules":     "403", // was 200
	"cr-none GET /api/v1/redaction/settings":  "403", // was 200
	"cr-none GET /api/v1/guardrails/settings": "403", // was 200
	"cr-none GET /api/v1/guardrails/patterns": "403", // was 200
	// ai:configure:own / :any
	"cr-none POST /api/v1/ai/keys":        "403", // was 400
	"cr-none DELETE /api/v1/ai/keys/{id}": "403", // was 404
	// ai:read:own / :any, ai:providers:manage
	"cr-none GET /api/v1/ai/providers":                "403", // was 200
	"cr-none GET /api/v1/ai/providers/{slug}":         "403", // was 404
	"cr-none GET /api/v1/ai/providers/{slug}/metrics": "403", // was 404
	"cr-none GET /api/v1/ai/providers/{slug}/models":  "403", // was 404
	// quotas:read:own / :any, cost:pricing:manage
	"cr-none GET /api/v1/cost/pricing": "403", // was 200
	// roles:read, roles:manage, automation:tokens:manage:own / :any
	"cr-none GET /api/v1/roles/permissions": "403", // was 200
}

// TestCookieSessionMatrix pins what each role may call with a dashboard
// session: the status of every /api/v1 route for an admin, a plain user, a
// custom role without permissions and a custom role with every permission of
// v0.9. The golden file was recorded before the permission gate was unified
// and must not change with it.
func TestCookieSessionMatrix(t *testing.T) {
	// The gate must leave the built-in roles alone, whatever else it changes.
	for cell := range intendedCookieChanges {
		if !strings.HasPrefix(cell, "cr-none ") && !strings.HasPrefix(cell, "cr-legacy ") {
			t.Errorf("intendedCookieChanges names %q: only cells of the custom roles may change", cell)
		}
	}
	got := cookieMatrix(t)
	if *updateAuthzMatrix {
		if err := os.MkdirAll(filepath.Dir(authzMatrixGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(authzMatrixGolden, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d cells to %s", len(got), authzMatrixGolden)
		return
	}
	raw, err := os.ReadFile(authzMatrixGolden)
	if err != nil {
		t.Fatalf("%v (record it with -update-authz-matrix)", err)
	}
	want := map[string]string{} // "<role> <METHOD> <pattern>" → status
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		i := strings.LastIndexByte(line, ' ')
		want[line[:i]] = line[i+1:]
	}
	for cell, status := range intendedCookieChanges {
		switch was, ok := want[cell]; {
		case !ok:
			t.Errorf("intendedCookieChanges names %q, which is not in the golden file", cell)
		case was == status:
			t.Errorf("intendedCookieChanges: %s answered %s before as well; drop the entry", cell, status)
		}
		want[cell] = status
	}
	seen := map[string]bool{}
	for _, line := range got {
		i := strings.LastIndexByte(line, ' ')
		cell, status := line[:i], line[i+1:]
		seen[cell] = true
		was, ok := want[cell]
		switch {
		case !ok:
			t.Errorf("%s: answers %s and is not in the golden file (a new route: record it with -update-authz-matrix)", cell, status)
		case was != status:
			t.Errorf("%s: answered %s before, %s now", cell, was, status)
		}
	}
	for cell := range want {
		if !seen[cell] {
			t.Errorf("%s: in the golden file, but the router has no such route", cell)
		}
	}
}
