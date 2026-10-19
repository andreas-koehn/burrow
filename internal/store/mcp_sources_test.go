package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

func TestValidMCPSourceSlug(t *testing.T) {
	good := []string{"a", "gh", "github", "my-tools", "a1", "0", strings.Repeat("a", 24)}
	bad := []string{"", "-", "-a", "a-", "A", "a_b", "a b", "a.b", "burrow", "v1", strings.Repeat("a", 25), "ä"}
	for _, s := range good {
		if !ValidMCPSourceSlug(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range bad {
		if ValidMCPSourceSlug(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func remoteInput(slug string) MCPSourceInput {
	return MCPSourceInput{Slug: slug, Name: "GitHub", Kind: "remote", URL: "https://mcp.example.com/mcp", CredentialSlot: "GH_MCP"}
}

// mcpSourceFixture returns an http service (any access mode) and a tcp one.
func mcpSourceFixture(t *testing.T, s *Store) (userID, httpSvc, tcpSvc string) {
	t.Helper()
	u := mustCreateUser(t, s, "src@x", "admin")
	return u.ID, mustGetOrCreateService(t, s, u.ID, "files", "http").ID, mustGetOrCreateService(t, s, u.ID, "pg", "tcp").ID
}

func TestCreateMCPSource_Remote(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	uid, _, _ := mcpSourceFixture(t, s)

	src, err := s.CreateMCPSource(ctx, uid, remoteInput("github"))
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != "remote" || src.URL != "https://mcp.example.com/mcp" || src.CredentialSlot != "GH_MCP" ||
		src.AuthHeader != "Authorization" || src.AuthFormat != "Bearer {key}" || src.ProtocolVersion != "" ||
		src.TimeoutS != 60 || src.RPM != 0 || src.MaxConcurrent != 0 || !src.Enabled || src.LastSyncAt != nil {
		t.Fatalf("defaults: %+v", src)
	}
	// The backing row is a hidden 'direct' service owned by the creator.
	svc, err := s.q.GetServiceByID(ctx, src.ServiceID)
	if err != nil || svc.Type != "direct" || svc.UserID != uid || svc.AccessMode != "api_key" {
		t.Fatalf("backing service: %v %+v", err, svc)
	}
	// A tunnel cannot register under the backing row's name.
	if _, err := s.q.GetOrCreateService(ctx, uid, svc.Name, "http"); !errors.Is(err, db.ErrServiceNameReserved) {
		t.Fatalf("name guard err = %v", err)
	}
	if _, err := s.CreateMCPSource(ctx, uid, remoteInput("github")); !errors.Is(err, ErrMCPSourceExists) {
		t.Fatalf("duplicate slug err = %v", err)
	}
}

func TestCreateMCPSource_Validation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	uid, svcID, _ := mcpSourceFixture(t, s)
	one := 1

	slugs := []string{"", "burrow", "v1", "Bad_Slug", "-x", strings.Repeat("a", 25)}
	for _, slug := range slugs {
		in := remoteInput(slug)
		if _, err := s.CreateMCPSource(ctx, uid, in); !errors.Is(err, ErrInvalidMCPSourceSlug) {
			t.Errorf("slug %q: err = %v, want ErrInvalidMCPSourceSlug", slug, err)
		}
	}
	mod := func(f func(*MCPSourceInput)) MCPSourceInput { in := remoteInput("x"); f(&in); return in }
	bad := map[string]MCPSourceInput{
		"empty name":      mod(func(i *MCPSourceInput) { i.Name = "  " }),
		"long name":       mod(func(i *MCPSourceInput) { i.Name = strings.Repeat("n", 121) }),
		"no kind":         mod(func(i *MCPSourceInput) { i.Kind = "" }),
		"http url":        mod(func(i *MCPSourceInput) { i.URL = "http://mcp.example.com/mcp" }),
		"empty url":       mod(func(i *MCPSourceInput) { i.URL = "" }),
		"url credentials": mod(func(i *MCPSourceInput) { i.URL = "https://u:p@mcp.example.com/" }),
		"url query":       mod(func(i *MCPSourceInput) { i.URL = "https://mcp.example.com/?k=v" }),
		"url fragment":    mod(func(i *MCPSourceInput) { i.URL = "https://mcp.example.com/#x" }),
		"slot lowercase":  mod(func(i *MCPSourceInput) { i.CredentialSlot = "gh" }),
		"two slots":       mod(func(i *MCPSourceInput) { i.CredentialSlot = "A,B" }),
		"slot too long":   mod(func(i *MCPSourceInput) { i.CredentialSlot = strings.Repeat("A", 33) }),
		"relay header":    mod(func(i *MCPSourceInput) { i.AuthHeader = "Host" }),
		"bad header":      mod(func(i *MCPSourceInput) { i.AuthHeader = "X Key" }),
		"no placeholder":  mod(func(i *MCPSourceInput) { i.AuthFormat = "Bearer" }),
		"two placeholder": mod(func(i *MCPSourceInput) { i.AuthFormat = "{key}{key}" }),
		"control format":  mod(func(i *MCPSourceInput) { i.AuthFormat = "Bearer {key}\r\nX: y" }),
		"protocol":        mod(func(i *MCPSourceInput) { i.ProtocolVersion = "latest" }),
		"rpm":             mod(func(i *MCPSourceInput) { v := -1; i.RPM = &v }),
		"rpm high":        mod(func(i *MCPSourceInput) { v := MaxMCPRPM + 1; i.RPM = &v }),
		"concurrency":     mod(func(i *MCPSourceInput) { v := MaxMCPConcurrency + 1; i.MaxConcurrent = &v }),
		"timeout zero":    mod(func(i *MCPSourceInput) { v := 0; i.TimeoutS = &v }),
		"timeout high":    mod(func(i *MCPSourceInput) { v := MaxMCPTimeoutS + 1; i.TimeoutS = &v }),
		"remote+service":  mod(func(i *MCPSourceInput) { i.ServiceID = svcID }),
		"tunnel+url":      {Slug: "x", Name: "x", Kind: "tunnel", ServiceID: svcID, URL: "https://a.example/"},
		"tunnel+slot":     {Slug: "x", Name: "x", Kind: "tunnel", ServiceID: svcID, CredentialSlot: "A"},
	}
	for name, in := range bad {
		_, err := s.CreateMCPSource(ctx, uid, in)
		if !errors.Is(err, ErrInvalidMCPSource) {
			t.Errorf("%s: err = %v, want ErrInvalidMCPSource", name, err)
		}
		// A reason names the field and never repeats the URL.
		if err != nil && strings.Contains(err.Error(), "example.com") {
			t.Errorf("%s: the reason repeats the URL: %v", name, err)
		}
	}
	// A public server needs no credential; limits take the given values.
	in := remoteInput("public")
	in.CredentialSlot, in.RPM, in.TimeoutS = "", &one, &one
	if src, err := s.CreateMCPSource(ctx, uid, in); err != nil || src.CredentialSlot != "" || src.RPM != 1 || src.TimeoutS != 1 {
		t.Fatalf("public source: %v %+v", err, src)
	}
	off := false
	in = remoteInput("disabled")
	in.Enabled = &off
	if src, err := s.CreateMCPSource(ctx, uid, in); err != nil || src.Enabled {
		t.Fatalf("disabled source: %v %+v", err, src)
	}
	if n := len(mustList(t, s)); n != 2 {
		t.Fatalf("%d sources after the failures, want 2", n)
	}
}

func mustList(t *testing.T, s *Store) []db.MCPSource {
	t.Helper()
	rows, err := s.ListMCPSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCreateMCPSource_Tunnel(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	uid, svcID, tcpID := mcpSourceFixture(t, s)

	src, err := s.CreateMCPSource(ctx, uid, MCPSourceInput{Slug: "files", Name: "Files", Kind: "tunnel", ServiceID: svcID})
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != "tunnel" || src.ServiceID != svcID || src.URL != "" {
		t.Fatalf("tunnel source: %+v", src)
	}
	if svc, _ := s.q.GetServiceByID(ctx, svcID); !svc.GatewayOnly || svc.Type != "http" {
		t.Fatalf("the service is not gateway-only: %+v", svc)
	}
	if _, err := s.CreateMCPSource(ctx, uid, MCPSourceInput{Slug: "second", Name: "S", Kind: "tunnel", ServiceID: svcID}); !errors.Is(err, ErrMCPSourceExists) {
		t.Fatalf("second source on the service err = %v", err)
	}
	for name, id := range map[string]string{"tcp": tcpID, "missing": "no-such-service", "empty": ""} {
		if _, err := s.CreateMCPSource(ctx, uid, MCPSourceInput{Slug: "t-" + name, Name: "T", Kind: "tunnel", ServiceID: id}); !errors.Is(err, ErrMCPSourceService) {
			t.Errorf("%s service err = %v, want ErrMCPSourceService", name, err)
		}
	}
	if len(mustList(t, s)) != 1 {
		t.Fatal("a refused create left a source behind")
	}
}

func TestUpdateMCPSource(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	uid, svcID, _ := mcpSourceFixture(t, s)
	if _, err := s.CreateMCPSource(ctx, uid, remoteInput("github")); err != nil {
		t.Fatal(err)
	}
	str := func(v string) *string { return &v }
	n := func(v int) *int { return &v }
	off := false

	got, err := s.UpdateMCPSource(ctx, "github", MCPSourceUpdate{RPM: n(30), Enabled: &off, AuthHeader: str("X-Api-Key"), AuthFormat: str("{key}"), ProtocolVersion: str("2025-06-18")})
	if err != nil {
		t.Fatal(err)
	}
	// What the body left out stays.
	if got.URL != "https://mcp.example.com/mcp" || got.CredentialSlot != "GH_MCP" || got.Name != "GitHub" || got.TimeoutS != 60 ||
		got.RPM != 30 || got.Enabled || got.AuthHeader != "X-Api-Key" || got.AuthFormat != "{key}" || got.ProtocolVersion != "2025-06-18" {
		t.Fatalf("after update: %+v", got)
	}
	// "" clears the slot and the pinned revision.
	got, err = s.UpdateMCPSource(ctx, "github", MCPSourceUpdate{CredentialSlot: str(""), ProtocolVersion: str(""), URL: str("https://other.example.com/x")})
	if err != nil || got.CredentialSlot != "" || got.ProtocolVersion != "" || got.URL != "https://other.example.com/x" {
		t.Fatalf("clear: %v %+v", err, got)
	}
	for name, u := range map[string]MCPSourceUpdate{
		"http url":   {URL: str("http://x.example.com/")},
		"slot":       {CredentialSlot: str("bad slot")},
		"name":       {Name: str("")},
		"timeout":    {TimeoutS: n(301)},
		"protocol":   {ProtocolVersion: str("x")},
		"empty url":  {URL: str("")},
		"bad format": {AuthFormat: str("nokey")},
	} {
		if _, err := s.UpdateMCPSource(ctx, "github", u); !errors.Is(err, ErrInvalidMCPSource) {
			t.Errorf("%s: err = %v, want ErrInvalidMCPSource", name, err)
		}
	}
	if after, _ := s.MCPSourceBySlug(ctx, "github"); after.URL != "https://other.example.com/x" || after.TimeoutS != 60 {
		t.Fatalf("a refused update changed the row: %+v", after)
	}
	if _, err := s.UpdateMCPSource(ctx, "ghost", MCPSourceUpdate{RPM: n(1)}); !errors.Is(err, ErrMCPSourceNotFound) {
		t.Fatalf("unknown slug err = %v", err)
	}
	// A tunnel source has no URL or credential.
	if _, err := s.CreateMCPSource(ctx, uid, MCPSourceInput{Slug: "files", Name: "Files", Kind: "tunnel", ServiceID: svcID}); err != nil {
		t.Fatal(err)
	}
	for name, u := range map[string]MCPSourceUpdate{"url": {URL: str("https://a.example.com/")}, "slot": {CredentialSlot: str("A")}} {
		if _, err := s.UpdateMCPSource(ctx, "files", u); !errors.Is(err, ErrInvalidMCPSource) {
			t.Errorf("tunnel %s: err = %v, want ErrInvalidMCPSource", name, err)
		}
	}
	if got, err := s.UpdateMCPSource(ctx, "files", MCPSourceUpdate{Name: str("Files!"), MaxConcurrent: n(2)}); err != nil || got.Name != "Files!" || got.MaxConcurrent != 2 {
		t.Fatalf("tunnel update: %v %+v", err, got)
	}
}

func TestDeleteMCPSource(t *testing.T) {
	s, sqldb := newStoreWithDB(t)
	ctx := context.Background()
	uid, svcID, _ := mcpSourceFixture(t, s)
	remote, err := s.CreateMCPSource(ctx, uid, remoteInput("github"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMCPSource(ctx, uid, MCPSourceInput{Slug: "files", Name: "Files", Kind: "tunnel", ServiceID: svcID}); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"github", "files"} {
		if _, err := sqldb.ExecContext(ctx, `INSERT INTO mcp_tools(source_slug, name, status) VALUES(?,?,?), (?,?,?)`,
			slug, "a", "approved", slug, "b", "pending"); err != nil {
			t.Fatal(err)
		}
	}
	rows := mustList(t, s)
	if rows[0].Slug != "files" || rows[0].ToolCount != 2 || rows[0].PendingCount != 1 {
		t.Fatalf("tool counts: %+v", rows[0])
	}

	// Remote: the hidden backing service goes with the source.
	if err := s.DeleteMCPSource(ctx, "github"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.q.GetServiceByID(ctx, remote.ServiceID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("the backing service is still there: %v", err)
	}
	// Tunnel: the service stays, only unbound.
	if err := s.DeleteMCPSource(ctx, "files"); err != nil {
		t.Fatal(err)
	}
	if svc, err := s.q.GetServiceByID(ctx, svcID); err != nil || svc.Type != "http" {
		t.Fatalf("the tunnel service did not survive: %v %+v", err, svc)
	}
	var tools int
	if err := sqldb.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_tools`).Scan(&tools); err != nil || tools != 0 {
		t.Fatalf("%d tools left (%v)", tools, err)
	}
	if err := s.DeleteMCPSource(ctx, "files"); !errors.Is(err, ErrMCPSourceNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	// The service is free to be bound again.
	if _, err := s.CreateMCPSource(ctx, uid, MCPSourceInput{Slug: "files2", Name: "Files", Kind: "tunnel", ServiceID: svcID}); err != nil {
		t.Fatal(err)
	}
}

// A service a tunnel source points at is not deleted, and neither is the
// hidden row of a remote source: the source would go with it unnoticed.
func TestDeleteService_RefusedWhileASourceUsesIt(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	uid, svcID, _ := mcpSourceFixture(t, s)
	remote, err := s.CreateMCPSource(ctx, uid, remoteInput("github"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMCPSource(ctx, uid, MCPSourceInput{Slug: "files", Name: "Files", Kind: "tunnel", ServiceID: svcID}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{svcID, remote.ServiceID} {
		_, err := s.DeleteService(ctx, uid, "admin", id)
		if !errors.Is(err, ErrServiceHasMCPSource) {
			t.Fatalf("delete service %s: err = %v, want ErrServiceHasMCPSource", id, err)
		}
	}
	if err := s.DeleteMCPSource(ctx, "files"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteService(ctx, uid, "admin", svcID); err != nil {
		t.Fatalf("delete service after the source is gone: %v", err)
	}
}
