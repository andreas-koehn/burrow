package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/audit"
)

const mcpTestSecret = "sk-mcp-never-leaves-the-vault"

type mcpSourceFixture struct {
	*authzHarness
	aud *stubAuditAppender
}

func newMCPSourceFixture(t *testing.T, mods ...func(*Deps)) *mcpSourceFixture {
	t.Helper()
	f := &mcpSourceFixture{aud: &stubAuditAppender{}}
	f.authzHarness = newAuthzHarness(t, append([]func(*Deps){func(d *Deps) {
		d.AuditAppender = f.aud
		d.CredentialVault = secretVault{"GH_MCP": mcpTestSecret}
		d.HostCheck = func(context.Context, string) error { return nil }
	}}, mods...)...)
	return f
}

func mcpDecode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func (f *mcpSourceFixture) actions() []string {
	var out []string
	for _, e := range f.aud.events {
		out = append(out, e.Action)
	}
	return out
}

func TestMCPSources_RemoteLifecycle(t *testing.T) {
	f := newMCPSourceFixture(t)
	admin := f.session(t, "admin")

	rec := f.do(t, http.MethodPost, "/api/v1/mcp/sources", map[string]any{
		"slug": "github", "name": "GitHub", "kind": "remote", "url": "https://mcp.example.com/mcp",
		"credential_slot": "GH_MCP", "rpm": 30,
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), mcpTestSecret) {
		t.Fatal("the credential is in the response")
	}
	src := mcpDecode[mcpSourceResp](t, rec)
	if src.Slug != "github" || src.Kind != "remote" || src.URL != "https://mcp.example.com/mcp" || src.CredentialSlot != "GH_MCP" ||
		!src.CredentialPresent || src.AuthHeader != "Authorization" || src.AuthFormat != "Bearer {key}" || src.RPM != 30 ||
		src.TimeoutS != 60 || !src.Enabled || src.EndpointURL != "https://relay.test/mcp/github" || src.ServiceID == "" ||
		src.LastSyncAt != nil || src.ToolCount != 0 || src.PendingCount != 0 {
		t.Fatalf("created source: %+v", src)
	}

	// The backing row is hidden from the services list and the service itself.
	rec = f.do(t, http.MethodGet, "/api/v1/services", nil, admin)
	if strings.Contains(rec.Body.String(), src.ServiceID) || strings.Contains(rec.Body.String(), "mcp:github") {
		t.Fatalf("the backing service is listed: %s", rec.Body.String())
	}

	rec = f.do(t, http.MethodGet, "/api/v1/mcp/sources", nil, admin)
	if list := mcpDecode[[]mcpSourceResp](t, rec); rec.Code != http.StatusOK || len(list) != 1 || list[0].Slug != "github" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec = f.do(t, http.MethodGet, "/api/v1/mcp/sources/github", nil, admin); rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	if rec = f.do(t, http.MethodGet, "/api/v1/mcp/sources/ghost", nil, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown: %d", rec.Code)
	}

	// PUT keeps what the body leaves out; "" clears the slot.
	rec = f.do(t, http.MethodPut, "/api/v1/mcp/sources/github", map[string]any{"timeout_s": 20, "credential_slot": "", "enabled": false}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if up := mcpDecode[mcpSourceResp](t, rec); up.TimeoutS != 20 || up.CredentialSlot != "" || up.CredentialPresent || up.Enabled || up.RPM != 30 || up.URL != src.URL {
		t.Fatalf("updated source: %+v", up)
	}

	if rec = f.do(t, http.MethodDelete, "/api/v1/mcp/sources/github", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec = f.do(t, http.MethodGet, "/api/v1/mcp/sources/github", nil, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", rec.Code)
	}
	if n := f.rows(t, "services") - 1; n != 0 { // the harness' own service
		t.Fatalf("%d backing rows left", n)
	}
	want := []string{audit.ActionMCPSourceCreate, audit.ActionMCPSourceUpdate, audit.ActionMCPSourceDelete}
	if got := f.actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
	for _, e := range f.aud.events {
		if strings.Contains(string(e.Payload), mcpTestSecret) || e.SubjectID != "github" {
			t.Errorf("audit event %s: subject %q payload %s", e.Action, e.SubjectID, e.Payload)
		}
	}
}

func TestMCPSources_Validation(t *testing.T) {
	f := newMCPSourceFixture(t)
	admin := f.session(t, "admin")
	post := func(body map[string]any) *httptest.ResponseRecorder {
		return f.do(t, http.MethodPost, "/api/v1/mcp/sources", body, admin)
	}
	ok := func() map[string]any {
		return map[string]any{"slug": "x", "kind": "remote", "url": "https://mcp.example.com/mcp"}
	}
	with := func(k string, v any) map[string]any { m := ok(); m[k] = v; return m }

	for name, body := range map[string]map[string]any{
		"no slug":       with("slug", ""),
		"reserved":      with("slug", "burrow"),
		"uppercase":     with("slug", "Git"),
		"no kind":       with("kind", ""),
		"bad kind":      with("kind", "openapi"),
		"http url":      with("url", "http://mcp.example.com/mcp"),
		"no url":        with("url", ""),
		"bad slot":      with("credential_slot", "gh mcp"),
		"bad timeout":   with("timeout_s", 301),
		"long name":     with("name", strings.Repeat("n", 121)),
		"api_key field": with("api_key", mcpTestSecret),
		"tunnel no svc": {"slug": "x", "kind": "tunnel"},
	} {
		rec := post(body)
		if rec.Code != http.StatusBadRequest && (name != "tunnel no svc" || rec.Code != http.StatusConflict) {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), mcpTestSecret) {
			t.Errorf("%s: the answer repeats the value", name)
		}
	}
	if rec := post(with("api_key", "x")); !strings.Contains(rec.Body.String(), "api_key") || !strings.Contains(rec.Body.String(), "credential_slot") {
		t.Errorf("unknown field answer: %s", rec.Body.String())
	}
	if n := f.rows(t, "mcp_sources"); n != 0 {
		t.Fatalf("%d sources created by refused requests", n)
	}
	if rec := post(ok()); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(ok()); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate slug: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPut, "/api/v1/mcp/sources/x", map[string]any{"url": "http://a.example.com/"}, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("update to http: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPut, "/api/v1/mcp/sources/x", map[string]any{"slug": "y"}, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("update with a slug: %d, want 400 (the slug never changes)", rec.Code)
	}
	if rec := f.do(t, http.MethodPut, "/api/v1/mcp/sources/ghost", map[string]any{"rpm": 1}, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("update unknown: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodDelete, "/api/v1/mcp/sources/ghost", nil, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: %d", rec.Code)
	}
}

// The save-time DNS check follows the providers': a host that resolves to a
// private address, or not at all, is refused, and the switch for private
// upstreams turns the check off.
func TestMCPSources_SSRFCheck(t *testing.T) {
	var checked []string
	var verdict error
	f := newMCPSourceFixture(t, func(d *Deps) {
		d.HostCheck = func(_ context.Context, host string) error { checked = append(checked, host); return verdict }
	})
	admin := f.session(t, "admin")
	create := func(slug string) *httptest.ResponseRecorder {
		return f.do(t, http.MethodPost, "/api/v1/mcp/sources", map[string]any{"slug": slug, "kind": "remote", "url": "https://" + slug + ".example.com/mcp"}, admin)
	}

	verdict = aiprovider.ErrBlockedAddress
	if rec := create("internal"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgURLPrivate("url")) {
		t.Fatalf("private host: %d %s", rec.Code, rec.Body.String())
	}
	verdict = errors.New("no such host")
	if rec := create("nodns"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), msgURLNoDNS("url")) {
		t.Fatalf("unresolvable host: %d %s", rec.Code, rec.Body.String())
	}
	if len(checked) != 2 || checked[0] != "internal.example.com" {
		t.Fatalf("checked = %v", checked)
	}
	verdict = nil
	if rec := create("fine"); rec.Code != http.StatusCreated {
		t.Fatalf("public host: %d %s", rec.Code, rec.Body.String())
	}
	// The URL of an update is checked too.
	verdict = aiprovider.ErrBlockedAddress
	if rec := f.do(t, http.MethodPut, "/api/v1/mcp/sources/fine", map[string]any{"url": "https://10.example.com/mcp"}, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("update to a private host: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodPut, "/api/v1/mcp/sources/fine", map[string]any{"rpm": 5}, admin); rec.Code != http.StatusOK {
		t.Fatalf("update without a URL does not need the check: %d", rec.Code)
	}
	if n := f.rows(t, "mcp_sources"); n != 1 {
		t.Fatalf("%d sources", n)
	}

	// With private upstreams allowed the check is skipped, like the dial guard.
	g := newMCPSourceFixture(t, func(d *Deps) {
		d.AllowPrivateUpstreams = true
		d.HostCheck = func(context.Context, string) error {
			t.Error("host check ran with private upstreams allowed")
			return aiprovider.ErrBlockedAddress
		}
	})
	if rec := g.do(t, http.MethodPost, "/api/v1/mcp/sources", map[string]any{"slug": "lan", "kind": "remote", "url": "https://192.168.1.5/mcp"}, g.session(t, "admin")); rec.Code != http.StatusCreated {
		t.Fatalf("private upstream allowed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMCPSources_TunnelBinding(t *testing.T) {
	f := newMCPSourceFixture(t)
	admin := f.session(t, "admin")
	body := map[string]any{"slug": "files", "name": "Files", "kind": "tunnel", "service_id": authzServiceID}

	// The route's gate is mcp:sources:manage; binding also needs the right to
	// configure the service, which a token must declare.
	rec := f.do(t, http.MethodPost, "/api/v1/mcp/sources", body, f.token(t, "admin", "mcp:sources:manage"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("token without services:configure: %d %s", rec.Code, rec.Body.String())
	}
	if f.service(t, authzServiceID).GatewayOnly || f.rows(t, "mcp_sources") != 0 {
		t.Fatal("a refused request changed something")
	}
	// A URL has no place on a tunnel source.
	bad := map[string]any{"slug": "files", "kind": "tunnel", "service_id": authzServiceID, "url": "https://a.example.com/"}
	if rec = f.do(t, http.MethodPost, "/api/v1/mcp/sources", bad, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("tunnel with a url: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodPost, "/api/v1/mcp/sources", body, f.token(t, "admin", "mcp:sources:manage", "services:configure:any"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("tunnel create: %d %s", rec.Code, rec.Body.String())
	}
	src := mcpDecode[mcpSourceResp](t, rec)
	if src.Kind != "tunnel" || src.ServiceID != authzServiceID || src.URL != "" || src.CredentialPresent {
		t.Fatalf("tunnel source: %+v", src)
	}
	if !f.service(t, authzServiceID).GatewayOnly {
		t.Fatal("the service is not gateway-only")
	}
	// The service is still listed: it is the user's tunnel.
	if rec = f.do(t, http.MethodGet, "/api/v1/services", nil, admin); !strings.Contains(rec.Body.String(), authzServiceID) {
		t.Fatalf("the bound service is not listed: %s", rec.Body.String())
	}
	body["slug"] = "files2"
	if rec = f.do(t, http.MethodPost, "/api/v1/mcp/sources", body, admin); rec.Code != http.StatusConflict {
		t.Fatalf("second source on the service: %d", rec.Code)
	}
	body["service_id"] = "no-such-service"
	if rec = f.do(t, http.MethodPost, "/api/v1/mcp/sources", body, admin); rec.Code != http.StatusConflict {
		t.Fatalf("unknown service: %d", rec.Code)
	}
	if rec = f.do(t, http.MethodPut, "/api/v1/mcp/sources/files", map[string]any{"url": "https://a.example.com/"}, admin); rec.Code != http.StatusBadRequest {
		t.Fatalf("tunnel update with a url: %d", rec.Code)
	}

	// Deleting the service is refused while the source uses it (409), and
	// works once the source is gone; the service survives the source's delete.
	rec = f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, admin)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "files") {
		t.Fatalf("delete the bound service: %d %s", rec.Code, rec.Body.String())
	}
	if rec = f.do(t, http.MethodDelete, "/api/v1/mcp/sources/files", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete source: %d", rec.Code)
	}
	if f.service(t, authzServiceID).ID == "" {
		t.Fatal("the service is gone")
	}
	if rec = f.do(t, http.MethodDelete, "/api/v1/services/"+authzServiceID, nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete the service after the source: %d %s", rec.Code, rec.Body.String())
	}
}

// The hidden backing row of a remote source cannot be deleted as a service.
func TestMCPSources_BackingRowSurvivesServiceDelete(t *testing.T) {
	f := newMCPSourceFixture(t)
	admin := f.session(t, "admin")
	rec := f.do(t, http.MethodPost, "/api/v1/mcp/sources", map[string]any{"slug": "gh", "kind": "remote", "url": "https://mcp.example.com/mcp"}, admin)
	src := mcpDecode[mcpSourceResp](t, rec)
	if rec = f.do(t, http.MethodDelete, "/api/v1/services/"+src.ServiceID, nil, admin); rec.Code != http.StatusConflict {
		t.Fatalf("delete the backing row: %d %s", rec.Code, rec.Body.String())
	}
	if rec = f.do(t, http.MethodGet, "/api/v1/mcp/sources/gh", nil, admin); rec.Code != http.StatusOK {
		t.Fatalf("the source is gone: %d", rec.Code)
	}
}

func TestMCPSources_Permissions(t *testing.T) {
	f := newMCPSourceFixture(t)
	admin := f.session(t, "admin")
	if rec := f.do(t, http.MethodPost, "/api/v1/mcp/sources", map[string]any{"slug": "gh", "kind": "remote", "url": "https://mcp.example.com/mcp"}, admin); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	read := f.token(t, "admin", "mcp:sources:read")
	if rec := f.do(t, http.MethodGet, "/api/v1/mcp/sources", nil, read); rec.Code != http.StatusOK {
		t.Fatalf("read with mcp:sources:read: %d", rec.Code)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/mcp/sources"}, {http.MethodPut, "/api/v1/mcp/sources/gh"}, {http.MethodDelete, "/api/v1/mcp/sources/gh"},
	} {
		if rec := f.do(t, c.method, c.path, map[string]any{}, read); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with mcp:sources:read only: %d, want 403", c.method, c.path, rec.Code)
		}
	}
	// Manage includes read; the user role has neither.
	if rec := f.do(t, http.MethodGet, "/api/v1/mcp/sources/gh", nil, f.token(t, "admin", "mcp:sources:manage")); rec.Code != http.StatusOK {
		t.Fatalf("read with mcp:sources:manage: %d", rec.Code)
	}
	for _, role := range []string{"user", "cr-none", "cr-legacy"} {
		if rec := f.do(t, http.MethodGet, "/api/v1/mcp/sources", nil, f.session(t, role)); rec.Code != http.StatusForbidden {
			t.Errorf("%s lists sources: %d, want 403", role, rec.Code)
		}
	}
}

// Final review: a new source takes at most 16 calls at a time when the body
// leaves max_concurrent out; 0 (no limit) must be asked for.
func TestMCPSources_DefaultMaxConcurrent(t *testing.T) {
	f := newMCPSourceFixture(t)
	admin := f.session(t, "admin")
	rec := f.do(t, http.MethodPost, "/api/v1/mcp/sources", map[string]any{"slug": "a", "kind": "remote", "url": "https://mcp.example.com/mcp"}, admin)
	if src := mcpDecode[mcpSourceResp](t, rec); rec.Code != http.StatusCreated || src.MaxConcurrent != 16 {
		t.Fatalf("without max_concurrent: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodPost, "/api/v1/mcp/sources", map[string]any{"slug": "b", "kind": "remote", "url": "https://mcp.example.com/mcp", "max_concurrent": 0}, admin)
	if src := mcpDecode[mcpSourceResp](t, rec); rec.Code != http.StatusCreated || src.MaxConcurrent != 0 {
		t.Fatalf("max_concurrent 0: %d %s", rec.Code, rec.Body.String())
	}
	// An update that leaves it out keeps what is stored.
	rec = f.do(t, http.MethodPut, "/api/v1/mcp/sources/b", map[string]any{"rpm": 5}, admin)
	if src := mcpDecode[mcpSourceResp](t, rec); rec.Code != http.StatusOK || src.MaxConcurrent != 0 {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
}
