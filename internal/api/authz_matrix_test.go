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

// TestCookieSessionMatrix pins what each role may call with a dashboard
// session: the status of every /api/v1 route for an admin, a plain user, a
// custom role without permissions and a custom role with every permission of
// v0.9. The golden file was recorded before the permission gate was unified
// and must not change with it.
func TestCookieSessionMatrix(t *testing.T) {
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
