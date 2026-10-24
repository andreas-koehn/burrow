package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// POST /tokens takes a scope: all by default, forward or publish; the answer
// and the list name it; an unknown one is a 400 and issues nothing.
func TestCreateToken_Scope(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"name":"a"}`, "all"},
		{`{"name":"a","scope":"all"}`, "all"},
		{`{"name":"a","scope":"forward"}`, "forward"},
		{`{"name":"a","scope":"publish"}`, "publish"},
	} {
		u := &tokUsers{}
		code, body := postToken(t, u, tc.body)
		if code != http.StatusCreated || u.issuedScope != tc.want || !strings.Contains(body, `"scope":"`+tc.want+`"`) {
			t.Fatalf("%s: status=%d issued scope=%q body=%s", tc.body, code, u.issuedScope, body)
		}
	}
	for _, bad := range []string{`{"name":"a","scope":"admin"}`, `{"name":"a","scope":"FORWARD"}`, `{"name":"a","scope":5}`} {
		u := &tokUsers{}
		code, _ := postToken(t, u, bad)
		if code != http.StatusBadRequest || u.issued != "" {
			t.Fatalf("%s: status=%d issued=%q, want 400 and nothing issued", bad, code, u.issued)
		}
	}
}

func TestListTokens_ShowsScope(t *testing.T) {
	u := &tokUsers{tokens: []db.ClientToken{
		{ID: "t1", UserID: "u1", Name: "old", CreatedAt: time.Now()}, // a row from before scopes reads as all
		{ID: "t2", UserID: "u1", Name: "rdp", Scope: "forward", CreatedAt: time.Now()},
		{ID: "t3", UserID: "u1", Name: "box", Scope: "publish", CreatedAt: time.Now()},
	}}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	ts := newTestServer(Deps{Users: u, Log: discardLog()})
	defer ts.Close()
	cl := loggedInClient(t, &httptestServer{ts})
	r, _ := cl.Get(ts.URL + "/api/v1/tokens")
	var got []struct{ ID, Scope string }
	if err := json.Unmarshal([]byte(readBody(t, r)), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"t1": "all", "t2": "forward", "t3": "publish"}
	if len(got) != 3 {
		t.Fatalf("list: %+v", got)
	}
	for _, g := range got {
		if g.Scope != want[g.ID] {
			t.Fatalf("token %s scope %q, want %q", g.ID, g.Scope, want[g.ID])
		}
	}
}

// An automation token mints a client token with a scope, capped in its expiry
// as before.
func TestCreateToken_ScopeByAutomationToken(t *testing.T) {
	exp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	u := &tokUsers{}
	u.verify = func(_, _ string) (bool, error) { return true, nil }
	auto := newFakeAutomationStore()
	_, tok, _ := auto.MintAutomationToken(context.Background(), "u1", "admin", "ci", []string{"tokens:manage:own"}, &exp)
	srv := httptest.NewServer(NewRouter(Deps{Users: u, Automation: auto, Bearer: auto, Log: discardLog()}))
	defer srv.Close()
	wantStatus(t, bearerDo(t, srv, tok, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "ci", "scope": "forward"}), http.StatusCreated)
	if u.issuedScope != "forward" || u.issuedExp == nil || !u.issuedExp.Equal(exp) {
		t.Fatalf("scope %q expiry %v, want forward and %v", u.issuedScope, u.issuedExp, exp)
	}
}

// The device flow: the client asks for a scope when it starts, the dashboard
// sees it, the approver may change it, and the poll names the scope the
// token got. Every other scope is a 400.
func TestClientLogin_Scope(t *testing.T) {
	h := newLoginHarness(t, nil)
	ctx := context.Background()
	c := authedClient(t, h.srv)

	start := func(body map[string]string) (*http.Response, map[string]any) {
		return anonPost(t, h.srv.URL+"/api/v1/client/login/start", body)
	}

	// The wish is shown, kept without a decision of the approver, and the
	// token gets it.
	resp, st := start(map[string]string{"hostname": "rdp-client", "scope": "forward"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start: %d %v", resp.StatusCode, st)
	}
	device, user := st["device_code"].(string), st["user_code"].(string)
	if g := decodeMap(t, c.get(t, "/api/v1/client/login/requests/"+user)); g["scope"] != "forward" {
		t.Fatalf("the request shows scope %v", g["scope"])
	}
	ar := c.post(t, "/api/v1/client/login/requests/"+user+"/approve", map[string]string{"password": "password1", "token_name": "rdp"})
	if body := decodeMap(t, ar); ar.StatusCode != http.StatusOK || body["scope"] != "forward" {
		t.Fatalf("approve: %d %v", ar.StatusCode, body)
	}
	h.clk.add(3 * time.Second)
	_, pol := h.poll(t, device)
	token, _ := pol["token"].(string)
	if pol["scope"] != "forward" {
		t.Fatalf("poll scope %v", pol["scope"])
	}
	if _, _, _, scope, err := h.st.AuthenticateTokenScope(ctx, token); err != nil || scope != "forward" {
		t.Fatalf("token scope %q %v", scope, err)
	}

	// The approver picks another scope.
	resp, st = start(map[string]string{"hostname": "box", "scope": "forward"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	device, user = st["device_code"].(string), st["user_code"].(string)
	// An unknown scope in the approval is a 400 and decides nothing.
	bad := c.post(t, "/api/v1/client/login/requests/"+user+"/approve", map[string]string{"password": "password1", "token_name": "box", "scope": "root"})
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("approve with a bad scope: %d", bad.StatusCode)
	}
	_ = bad.Body.Close()
	ar = c.post(t, "/api/v1/client/login/requests/"+user+"/approve", map[string]string{"password": "password1", "token_name": "box", "scope": "publish"})
	if body := decodeMap(t, ar); ar.StatusCode != http.StatusOK || body["scope"] != "publish" {
		t.Fatalf("approve: %d %v", ar.StatusCode, body)
	}
	h.clk.add(3 * time.Second)
	_, pol = h.poll(t, device)
	if _, _, _, scope, err := h.st.AuthenticateTokenScope(ctx, pol["token"].(string)); err != nil || scope != "publish" || pol["scope"] != "publish" {
		t.Fatalf("token scope %q (%v) %v", scope, pol["scope"], err)
	}

	// No scope anywhere: all. A bad one at the start: 400.
	_, st = start(startBody)
	device, user = st["device_code"].(string), st["user_code"].(string)
	ar = c.post(t, "/api/v1/client/login/requests/"+user+"/approve", map[string]string{"password": "password1", "token_name": "plain"})
	_ = ar.Body.Close()
	h.clk.add(3 * time.Second)
	if _, pol = h.poll(t, device); pol["scope"] != "all" {
		t.Fatalf("default scope %v", pol["scope"])
	}
	if resp, body := start(map[string]string{"scope": "everything"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("start with a bad scope: %d %v", resp.StatusCode, body)
	}

	// The audit rows of the mint carry the scope.
	var seen []string
	for _, e := range h.audit.byAction("token.mint") {
		seen = append(seen, string(e.Payload))
	}
	for i, want := range []string{"forward", "publish", "all"} {
		if i >= len(seen) || !strings.Contains(seen[i], `"scope":"`+want+`"`) {
			t.Fatalf("mint rows %v, want scope %s at %d", seen, want, i)
		}
	}
}
