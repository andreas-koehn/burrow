package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// authorizeFrom sends an authorization request from a client address.
func (h *harness) authorizeFrom(t *testing.T, addr string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	req.RemoteAddr = addr + ":40000"
	rec := httptest.NewRecorder()
	h.s.Authorize(rec, req)
	return rec
}

func pendingOK(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusFound && strings.Contains(rec.Header().Get("Location"), "/oauth/consent?request=")
}

func refusedBusy(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusFound && strings.Contains(rec.Header().Get("Location"), "error=temporarily_unavailable")
}

// One address holds at most maxPendingPerAddr waiting requests; another
// address is not affected.
func TestPending_PerAddressCap(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxPendingPerAddr; i++ {
		if rec := h.authorizeFrom(t, "198.51.100.7", h.authParams()); !pendingOK(rec) {
			t.Fatalf("request %d: %d %q", i, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := h.authorizeFrom(t, "198.51.100.7", h.authParams()); !refusedBusy(rec) {
		t.Fatalf("over the address cap: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := h.authorizeFrom(t, "198.51.100.8", h.authParams()); !pendingOK(rec) {
		t.Fatalf("another address: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Deciding one frees a place; so does expiry.
	id := h.requestID(t, h.authParams())
	_, _ = h.s.Deny(context.Background(), id)
	h.clk.add(RequestTTL)
	if rec := h.authorizeFrom(t, "198.51.100.7", h.authParams()); !pendingOK(rec) {
		t.Fatalf("after expiry: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// All addresses together hold at most maxPending.
func TestPending_GlobalCap(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < maxPending; i++ {
		addr := "203.0." + strconv.Itoa(i/250) + "." + strconv.Itoa(i%250)
		if rec := h.authorizeFrom(t, addr, h.authParams()); !pendingOK(rec) {
			t.Fatalf("request %d: %d %q", i, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := h.authorizeFrom(t, "192.0.2.200", h.authParams()); !refusedBusy(rec) {
		t.Fatalf("over the global cap: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	h.clk.add(RequestTTL)
	if rec := h.authorizeFrom(t, "192.0.2.200", h.authParams()); !pendingOK(rec) {
		t.Fatalf("after expiry: %d", rec.Code)
	}
	if maxPendingPerAddr*10 > maxPending {
		t.Fatal("the address cap should be well below the global one")
	}
}

// A pending request keeps only what consent and the code need: no list of
// the client's redirect URIs (a metadata document may hold up to 32).
func TestPending_KeepsNoRedirectList(t *testing.T) {
	typ := reflect.TypeOf(pendingRequest{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if (f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeOf(time.Time{})) || (f.Type.Kind() == reflect.Slice && f.Name != "Scopes") {
			t.Errorf("pendingRequest.%s (%s) holds more than it needs", f.Name, f.Type)
		}
	}
}

// A second redemption that lands before the first one has linked its grant
// still wins: the first exchange fails instead of handing out a grant.
func TestCodeReuseBeforeTheGrantIsLinked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	code := h.code(t, h.authParams())
	first, err := h.x.RedeemOAuthCode(ctx, hashSecret(code)) // the first exchange, in flight
	if err != nil || first.Used {
		t.Fatalf("first redeem: %+v %v", first, err)
	}
	wantTokenError(t, "second use before the link", h.exchange(t, code), 400, "invalid_grant")
	g := db.OAuthGrant{ID: "g-late", ClientID: first.ClientID, ClientName: first.ClientName, UserID: first.UserID, Resource: first.Resource}
	if err := h.x.CreateOAuthGrant(ctx, g, first.Hash); !errors.Is(err, db.ErrOAuthCodeReused) {
		t.Fatalf("linking after a reuse: %v", err)
	}
	if _, err := h.x.GetOAuthGrant(ctx, "g-late"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("the grant was stored: %v", err)
	}
	if !strings.Contains(strings.Join(h.audit.actions(), ","), audit.ActionOAuthRefreshReuse) {
		t.Fatalf("reuse not audited: %v", h.audit.actions())
	}
}

// Two redemptions at once: at most one gets tokens, and whatever it got is
// revoked by the other (a code is used once).
func TestCodeRedeemedTwiceAtOnce(t *testing.T) {
	h := newHarness(t)
	for round := 0; round < 5; round++ {
		code := h.code(t, h.authParams())
		answers := make([]tokenAnswer, 2)
		var wg sync.WaitGroup
		for i := range answers {
			wg.Add(1)
			go func(i int) { defer wg.Done(); answers[i] = h.exchange(t, code) }(i)
		}
		wg.Wait()
		ok := 0
		for _, a := range answers {
			if a.status == http.StatusOK {
				ok++
				if _, err := h.s.LookupAccessToken(context.Background(), a.str("access_token")); !errors.Is(err, ErrInvalidToken) {
					t.Fatalf("round %d: the winner's token survived the second use: %v", round, err)
				}
			} else if a.str("error") != "invalid_grant" {
				t.Fatalf("round %d: %d %v", round, a.status, a.body)
			}
		}
		if ok > 1 {
			t.Fatalf("round %d: both redemptions got tokens", round)
		}
	}
}

// Two refreshes with one token at once: the strict rule holds, at most one
// gets new tokens and the family is revoked by the second use.
func TestRefreshedTwiceAtOnce(t *testing.T) {
	h := newHarness(t)
	for round := 0; round < 5; round++ {
		rt := h.exchange(t, h.code(t, h.authParams())).str("refresh_token")
		answers := make([]tokenAnswer, 2)
		var wg sync.WaitGroup
		for i := range answers {
			wg.Add(1)
			go func(i int) { defer wg.Done(); answers[i] = h.refresh(t, rt) }(i)
		}
		wg.Wait()
		ok := 0
		for _, a := range answers {
			if a.status == http.StatusOK {
				ok++
				if _, err := h.s.LookupAccessToken(context.Background(), a.str("access_token")); !errors.Is(err, ErrInvalidToken) {
					t.Fatalf("round %d: the winner's token survived the reuse: %v", round, err)
				}
			} else if a.str("error") != "invalid_grant" {
				t.Fatalf("round %d: %d %v", round, a.status, a.body)
			}
		}
		if ok > 1 {
			t.Fatalf("round %d: both refreshes got tokens", round)
		}
	}
}

// A refresh asks for exactly the grant's scopes or none: narrowing is refused.
func TestRefresh_NarrowerScopeRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.requestID(t, h.authParams())
	got, err := h.s.Approve(ctx, id, Decision{UserID: "u-admin", Role: "admin", Scopes: []string{"burrow:read", "users:read"}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got.RedirectURL)
	rt := h.exchange(t, u.Query().Get("code")).str("refresh_token")
	ask := func(scope string) tokenAnswer {
		return h.post(t, h.s.Token, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {h.client.ClientID}, "scope": {scope}})
	}
	wantTokenError(t, "narrower", ask("burrow:read"), 400, "invalid_scope")
	a := ask("users:read burrow:read")
	if a.status != http.StatusOK || a.str("scope") != "burrow:read users:read" {
		t.Fatalf("the same set: %d %v", a.status, a.body)
	}
}

// HTTP Basic: the client id may arrive form-urlencoded (RFC 6749) or raw.
// A client mismatch does not burn the code.
func TestToken_BasicRawClientIDAndNoBurnOnMismatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.x.CreateOAuthClient(ctx, db.OAuthClient{ClientID: "bcl_a+b", Name: "Plus", RedirectURIs: []string{"http://127.0.0.1:5000/callback"}}); err != nil {
		t.Fatal(err)
	}
	q := h.authParams()
	q.Set("client_id", "bcl_a+b")
	form := func(code string) url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://127.0.0.1:5000/callback"}, "code_verifier": {verifier}}
	}
	code := h.code(t, q)
	wantTokenError(t, "another client", h.post(t, h.s.Token, form(code), func(r *http.Request) { r.SetBasicAuth("bcl_other", "") }), 400, "invalid_grant")
	if a := h.post(t, h.s.Token, form(code), func(r *http.Request) { r.SetBasicAuth("bcl_a+b", "") }); a.status != http.StatusOK {
		t.Fatalf("raw client id after a mismatch: %d %v", a.status, a.body)
	}
	code = h.code(t, q)
	if a := h.post(t, h.s.Token, form(code), func(r *http.Request) { r.SetBasicAuth(url.QueryEscape("bcl_a+b"), "") }); a.status != http.StatusOK {
		t.Fatalf("escaped client id: %d %v", a.status, a.body)
	}
}

// Deleting a pre-registered client ends its open codes and waiting requests.
func TestDeleteClientEndsCodesAndRequests(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	code := h.code(t, h.authParams())
	id := h.requestID(t, h.authParams())
	if _, err := h.s.DeleteClient(ctx, h.client.ClientID); err != nil {
		t.Fatal(err)
	}
	wantTokenError(t, "code of a deleted client", h.exchange(t, code), 400, "invalid_grant")
	if _, err := h.s.Approve(ctx, id, Decision{UserID: "u-admin", Role: "admin"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("request of a deleted client: %v", err)
	}
}

// The loopback exception needs a loopback Host and a loopback peer: a
// remote client cannot switch it on with "Host: localhost".
func TestIssuer_LoopbackNeedsALoopbackPeer(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Issuer = "" })
	for _, tc := range []struct {
		host, remote string
		on           bool
	}{
		{"localhost:8080", "127.0.0.1:5555", true},
		{"[::1]:8080", "[::1]:5555", true},
		{"localhost:8080", "203.0.113.9:5555", false},
		{"127.0.0.1", "[2001:db8::1]:5555", false},
		{"relay.example", "127.0.0.1:5555", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
		req.Host, req.RemoteAddr = tc.host, tc.remote
		rec := httptest.NewRecorder()
		h.s.Metadata(rec, req)
		if on := rec.Code == http.StatusOK; on != tc.on || h.s.Available(req) != tc.on {
			t.Errorf("Host %s from %s: %d, want on=%v", tc.host, tc.remote, rec.Code, tc.on)
		}
	}
}

// The per-address cap counts an IPv6 client by its /64.
func TestClientAddrMasksIPv6(t *testing.T) {
	for remote, want := range map[string]string{
		"[2001:db8:1:2:aaaa::1]:1": "2001:db8:1:2::",
		"[2001:db8:1:2:bbbb::9]:1": "2001:db8:1:2::",
		"198.51.100.7:1":           "198.51.100.7",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		if got := clientAddr(req); got != want {
			t.Errorf("%s: %q, want %q", remote, got, want)
		}
	}
}

// An empty Basic user name counts as no Basic header: the body's client_id
// decides.
func TestToken_EmptyBasicUserIsNoBasic(t *testing.T) {
	h := newHarness(t)
	code := h.code(t, h.authParams())
	f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {h.client.ClientID},
		"redirect_uri": {"http://127.0.0.1:5000/callback"}, "code_verifier": {verifier}}
	if a := h.post(t, h.s.Token, f, func(r *http.Request) { r.SetBasicAuth("", "") }); a.status != http.StatusOK {
		t.Fatalf("empty Basic user with client_id in the body: %d %v", a.status, a.body)
	}
}
