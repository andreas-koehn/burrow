package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A token carries its scope; the default is all; the audit row of the mint
// names it.
func TestClientTokenScope_IssueAuthenticateAudit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sink := &auditSink{}
	s.SetAuditLogger(sink)
	u := mustCreateUser(t, s, "scope@x", "user")

	for _, tc := range []struct{ name, scope, want string }{
		{"default", "", "all"}, {"all", "all", "all"}, {"fwd", "forward", "forward"}, {"pub", "publish", "publish"},
	} {
		pt, err := s.IssueClientTokenUntil(ctx, u.ID, tc.name, nil, tc.scope)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		uid, _, _, scope, err := s.AuthenticateTokenScope(ctx, pt)
		if err != nil || uid != u.ID || scope != tc.want {
			t.Fatalf("%s: uid=%q scope=%q err=%v, want scope %q", tc.name, uid, scope, err, tc.want)
		}
	}
	if _, err := s.IssueClientTokenUntil(ctx, u.ID, "bad", nil, "admin"); !errors.Is(err, ErrTokenScope) {
		t.Fatalf("an unknown scope: %v", err)
	}
	toks, err := s.ListClientTokens(ctx, u.ID)
	if err != nil || len(toks) != 4 {
		t.Fatalf("list: %v %+v", err, toks)
	}
	// The old call is unchanged: scope all.
	pt, err := s.IssueClientToken(ctx, u.ID, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, scope, _ := s.AuthenticateTokenScope(ctx, pt); scope != "all" {
		t.Fatalf("IssueClientToken scope = %q", scope)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var mints []string
	for _, e := range sink.ev {
		if e.Action == "token.mint" {
			mints = append(mints, string(e.Payload))
		}
	}
	if len(mints) != 5 {
		t.Fatalf("mint rows: %v", mints)
	}
	for i, want := range []string{`"scope":"all"`, `"scope":"all"`, `"scope":"forward"`, `"scope":"publish"`, `"scope":"all"`} {
		if !strings.Contains(mints[i], want) {
			t.Fatalf("mint row %d lacks %s: %s", i, want, mints[i])
		}
	}
}

// The device flow: the client wishes, the approver decides, the token gets
// the decision; the audit rows name it.
func TestClientLogin_Scope(t *testing.T) {
	ctx := context.Background()
	s, _, clk, u1 := loginFixture(t)
	sink := &auditSink{}
	s.SetAuditLogger(sink)

	wish := func(scope string) LoginStart {
		m := loginMeta
		m.Scope = scope
		st, err := s.StartClientLogin(ctx, m)
		if err != nil {
			t.Fatalf("start %q: %v", scope, err)
		}
		clk.add(3 * time.Second)
		return st
	}
	collect := func(st LoginStart) LoginResult {
		clk.add(3 * time.Second)
		res, err := s.PollClientLogin(ctx, st.DeviceCode)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		return res
	}

	// No wish: all.
	st := wish("")
	if v, _ := s.GetClientLogin(ctx, st.UserCode); v.Scope != "all" {
		t.Fatalf("view scope %q", v.Scope)
	}
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "a", ""); err != nil {
		t.Fatal(err)
	}
	if res := collect(st); res.Scope != "all" {
		t.Fatalf("scope %q", res.Scope)
	}

	// The wish is kept when the approver says nothing.
	st = wish("forward")
	if v, _ := s.GetClientLogin(ctx, st.UserCode); v.Scope != "forward" {
		t.Fatalf("view scope %q", v.Scope)
	}
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "b", ""); err != nil {
		t.Fatal(err)
	}
	res := collect(st)
	if res.Scope != "forward" {
		t.Fatalf("scope %q", res.Scope)
	}
	if _, _, _, scope, err := s.AuthenticateTokenScope(ctx, res.Token); err != nil || scope != "forward" {
		t.Fatalf("token scope %q %v", scope, err)
	}

	// The approver may change it, in both directions.
	st = wish("forward")
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "c", "publish"); err != nil {
		t.Fatal(err)
	}
	res = collect(st)
	if _, _, _, scope, _ := s.AuthenticateTokenScope(ctx, res.Token); scope != "publish" || res.Scope != "publish" {
		t.Fatalf("token scope %q (%q)", scope, res.Scope)
	}
	st = wish("")
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "d", "forward"); err != nil {
		t.Fatal(err)
	}
	res = collect(st)
	if _, _, _, scope, _ := s.AuthenticateTokenScope(ctx, res.Token); scope != "forward" {
		t.Fatalf("token scope %q", scope)
	}

	// Unknown scopes are refused: at the start, and at the approval (the
	// request stays pending).
	bad := loginMeta
	bad.Scope = "root"
	if _, err := s.StartClientLogin(ctx, bad); !errors.Is(err, ErrTokenScope) {
		t.Fatalf("start with a bad scope: %v", err)
	}
	st = wish("")
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "e", "root"); !errors.Is(err, ErrTokenScope) {
		t.Fatalf("approve with a bad scope: %v", err)
	}
	if v, err := s.GetClientLogin(ctx, st.UserCode); err != nil || v.Status != "pending" {
		t.Fatalf("after a bad approval: %+v %v", v, err)
	}

	// Every mint row and approval row names the scope that was decided.
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var mints, approvals []string
	for _, e := range sink.ev {
		switch e.Action {
		case "token.mint":
			mints = append(mints, string(e.Payload))
		case "client.login.approved":
			approvals = append(approvals, string(e.Payload))
		}
	}
	for i, want := range []string{"all", "forward", "publish", "forward"} {
		if !strings.Contains(mints[i], `"scope":"`+want+`"`) || !strings.Contains(approvals[i], `"scope":"`+want+`"`) {
			t.Fatalf("row %d does not say %s:\nmint %s\napproved %s", i, want, mints[i], approvals[i])
		}
	}
}
