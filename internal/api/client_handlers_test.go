package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// knownFake is a ClientLister over client tokens and live sessions: it joins
// them the way the relay does (KnownClients).
type knownFake struct {
	tokens []db.ClientToken
	live   []LiveClient
	err    error
	// raw, when set, is returned as it is.
	raw []KnownClient
	// asked collects the ids GetClient was asked for.
	asked *[]string
}

func (f knownFake) ListClients() []ClientView { return nil }
func (f knownFake) GetClient(id string) (ClientDetail, bool) {
	if f.asked != nil {
		*f.asked = append(*f.asked, id)
	}
	return ClientDetail{ClientView: ClientView{SessionID: id}}, true
}
func (f knownFake) ListKnownClients(context.Context) ([]KnownClient, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.raw != nil {
		return f.raw, nil
	}
	return KnownClients(f.tokens, f.live), nil
}

// getKnown asks GET /api/v1/clients/known as an admin and returns the rows
// both decoded and as raw JSON (to tell null from []).
func getKnown(t *testing.T, f ClientLister) ([]KnownClient, []map[string]json.RawMessage) {
	t.Helper()
	d := Deps{Log: discardLog(), Clients: f, Users: &fakeUserStore{role: "admin"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	r := authedClient(t, srv).get(t, "/api/v1/clients/known")
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("known clients status=%d body=%s", r.StatusCode, body)
	}
	if strings.TrimSpace(string(body)) == "null" {
		t.Fatal("the list must be [], never null")
	}
	var rows []KnownClient
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return rows, raw
}

func at(minutesAgo int) *time.Time {
	ts := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute)
	return &ts
}

func liveSession(sessionID, tokenID string, connected *time.Time, served ...string) LiveClient {
	return LiveClient{
		ClientView: ClientView{
			SessionID: sessionID, UserID: "u1", TokenName: "laptop", RemoteAddr: "10.0.0.9:1" + sessionID[len(sessionID)-1:],
			OS: "os-" + sessionID, Arch: "arch-" + sessionID, ClientVersion: "v-" + sessionID, ServiceCount: len(served),
		},
		TokenID: tokenID, ConnectedAt: *connected, Served: served,
	}
}

// Rule 1: a token with a live session is online; last_seen is the stored one
// and the platform comes from the newest live session.
func TestKnownClients_OnlineTakesThePlatformOfTheNewestSession(t *testing.T) {
	f := knownFake{
		tokens: []db.ClientToken{{
			ID: "tok-1", UserID: "u1", Name: "laptop", TokenHash: "secret-hash",
			LastSeen: at(3), LastOS: "stored-os", LastArch: "stored-arch", LastClientVersion: "stored-v",
			LastRemoteAddr: "stored-addr", LastServed: []string{"stored"},
		}},
		live: []LiveClient{liveSession("s-old", "tok-1", at(60), "web"), liveSession("s-new", "tok-1", at(5), "ollama", "web")},
	}
	rows, raw := getKnown(t, f)
	if len(rows) != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	kc := rows[0]
	if kc.TokenID != "tok-1" || kc.TokenName != "laptop" || kc.UserID != "u1" || kc.Status != "online" {
		t.Fatalf("row: %+v", kc)
	}
	if kc.LastSeen == nil || !kc.LastSeen.Equal(*at(3)) {
		t.Fatalf("last_seen = %v, want the stored %v", kc.LastSeen, at(3))
	}
	if kc.OS != "os-s-new" || kc.Arch != "arch-s-new" || kc.ClientVersion != "v-s-new" || kc.RemoteAddr != "10.0.0.9:1w" {
		t.Fatalf("platform must come from the newest live session: %+v", kc)
	}
	if !reflect.DeepEqual(kc.Served, []string{"ollama", "web"}) {
		t.Fatalf("served = %v, want what the live sessions serve", kc.Served)
	}
	for k, v := range raw[0] {
		if strings.Contains(string(v), "secret-hash") {
			t.Fatalf("field %s leaks the token hash", k)
		}
	}
}

// Rule 2: a token that was seen and has no live session is offline, with the
// stored fields.
func TestKnownClients_OfflineShowsTheStoredSession(t *testing.T) {
	rows, _ := getKnown(t, knownFake{tokens: []db.ClientToken{{
		ID: "tok-1", UserID: "u1", Name: "laptop",
		LastSeen: at(90), LastOS: "linux", LastArch: "amd64", LastClientVersion: "0.9.0",
		LastRemoteAddr: "10.0.0.5:4242", LastServed: []string{"ollama", "web"},
	}}})
	want := KnownClient{
		TokenID: "tok-1", TokenName: "laptop", UserID: "u1", Status: "offline", LastSeen: at(90),
		OS: "linux", Arch: "amd64", ClientVersion: "0.9.0", RemoteAddr: "10.0.0.5:4242",
		Served: []string{"ollama", "web"}, Sessions: []ClientView{},
	}
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
}

// Rules 3 and 5: a token that never connected has no platform, no last_seen
// and empty lists.
func TestKnownClients_NeverConnected(t *testing.T) {
	// A record that was never written leaves text behind in no column; a row
	// that holds some without last_seen still reads as never connected.
	rows, raw := getKnown(t, knownFake{tokens: []db.ClientToken{{
		ID: "tok-1", UserID: "u1", Name: "laptop", LastOS: "linux", LastServed: []string{"web"},
	}}})
	if len(rows) != 1 || rows[0].Status != "never_connected" || rows[0].TokenID != "tok-1" || rows[0].TokenName != "laptop" {
		t.Fatalf("rows: %+v", rows)
	}
	for field, want := range map[string]string{
		"last_seen": "null", "os": `""`, "arch": `""`, "client_version": `""`, "remote_addr": `""`,
		"served": "[]", "sessions": "[]",
	} {
		if got := string(raw[0][field]); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
}

// Rule 4: two live sessions of one token are one row with two sessions.
func TestKnownClients_TwoSessionsOfOneTokenAreOneRow(t *testing.T) {
	rows, _ := getKnown(t, knownFake{
		tokens: []db.ClientToken{{ID: "tok-1", UserID: "u1", Name: "laptop", LastSeen: at(1)}},
		live:   []LiveClient{liveSession("s-a", "tok-1", at(30), "web"), liveSession("s-b", "tok-1", at(10), "web")},
	})
	if len(rows) != 1 || len(rows[0].Sessions) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	// Newest first, each as GET /clients shows it.
	if s := rows[0].Sessions; s[0].SessionID != "s-b" || s[1].SessionID != "s-a" || s[1].OS != "os-s-a" || s[0].ServiceCount != 1 {
		t.Fatalf("sessions: %+v", s)
	}
	if !reflect.DeepEqual(rows[0].Served, []string{"web"}) {
		t.Fatalf("served = %v, want each name once", rows[0].Served)
	}
}

// Rule 5: served and sessions are [] whatever the lister returns.
func TestKnownClients_ListsAreNeverNull(t *testing.T) {
	_, raw := getKnown(t, knownFake{raw: []KnownClient{{TokenID: "tok-1", Status: "offline"}}})
	if len(raw) != 1 || string(raw[0]["served"]) != "[]" || string(raw[0]["sessions"]) != "[]" {
		t.Fatalf("raw: %v", raw)
	}
	if rows, _ := getKnown(t, knownFake{}); len(rows) != 0 {
		t.Fatalf("no tokens, no sessions: %+v", rows)
	}
	// Without a lister the list is empty, as for GET /clients.
	d := Deps{Log: discardLog(), Users: &fakeUserStore{role: "admin"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	r := authedClient(t, srv).get(t, "/api/v1/clients/known")
	defer r.Body.Close()
	if body, _ := io.ReadAll(r.Body); r.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("no lister: status=%d body=%s", r.StatusCode, body)
	}
}

// Rule 6: online first, then offline by last_seen descending, then never
// connected by name.
func TestKnownClients_Order(t *testing.T) {
	rows, _ := getKnown(t, knownFake{
		tokens: []db.ClientToken{
			{ID: "never-b", Name: "beta"},
			{ID: "off-old", Name: "alpha", LastSeen: at(500)},
			{ID: "on-z", Name: "zulu", LastSeen: at(900)},
			{ID: "never-a", Name: "alpha"},
			{ID: "off-new", Name: "zeta", LastSeen: at(20)},
			{ID: "on-a", Name: "able", LastSeen: at(1)},
		},
		live: []LiveClient{liveSession("s-z", "on-z", at(900)), liveSession("s-a", "on-a", at(1))},
	})
	var ids, status []string
	for _, kc := range rows {
		ids = append(ids, kc.TokenID)
		status = append(status, kc.Status)
	}
	if want := []string{"on-a", "on-z", "off-new", "off-old", "never-a", "never-b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
	if want := []string{"online", "online", "offline", "offline", "never_connected", "never_connected"}; !reflect.DeepEqual(status, want) {
		t.Fatalf("status = %v", status)
	}
}

// Rule 7: the list sits behind the gate of GET /clients.
func TestKnownClients_SameGateAsClients(t *testing.T) {
	f := knownFake{tokens: []db.ClientToken{{ID: "tok-1", Name: "laptop"}}}
	d := Deps{Log: discardLog(), Clients: f, Users: &fakeUserStore{role: "user"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	var bodies []string
	for _, path := range []string{"/api/v1/clients", "/api/v1/clients/known"} {
		r := c.get(t, path)
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as a non-admin: status=%d body=%s, want 403", path, r.StatusCode, body)
		}
		bodies = append(bodies, string(body))
	}
	if bodies[0] != bodies[1] || strings.Contains(bodies[1], "tok-1") {
		t.Fatalf("a non-admin must get what GET /clients gives: %q vs %q", bodies[0], bodies[1])
	}
	// Without a session there is no list either.
	r, err := http.Get(srv.URL + "/api/v1/clients/known")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session: status=%d, want 401", r.StatusCode)
	}
}

// Rule 8: a live session whose token was revoked meanwhile is still shown.
func TestKnownClients_SessionOfARevokedTokenIsOnlineWithoutTokenID(t *testing.T) {
	rows, raw := getKnown(t, knownFake{
		tokens: []db.ClientToken{{ID: "tok-kept", UserID: "u2", Name: "nas"}},
		live: []LiveClient{
			liveSession("s-1", "tok-revoked", at(40), "web"),
			liveSession("s-2", "tok-revoked", at(4), "ollama"),
			// A session whose authenticator named no token is a row of its own.
			liveSession("s-3", "", at(2)),
		},
	})
	if len(rows) != 3 {
		t.Fatalf("rows: %+v", rows)
	}
	gone := rows[0]
	if gone.Status != "online" || gone.TokenID != "" || gone.TokenName != "laptop" || gone.UserID != "u1" ||
		len(gone.Sessions) != 2 || gone.OS != "os-s-2" || !reflect.DeepEqual(gone.Served, []string{"ollama", "web"}) {
		t.Fatalf("revoked token row: %+v", gone)
	}
	if string(raw[0]["token_id"]) != `""` {
		t.Fatalf("token_id = %s, want \"\"", raw[0]["token_id"])
	}
	// Nothing was stored for it: it was last seen when it connected.
	if gone.LastSeen == nil || !gone.LastSeen.Equal(*at(4)) {
		t.Fatalf("last_seen = %v", gone.LastSeen)
	}
	if rows[1].Status != "online" || rows[1].TokenID != "" || len(rows[1].Sessions) != 1 || rows[1].Sessions[0].SessionID != "s-3" {
		t.Fatalf("session without a token id: %+v", rows[1])
	}
	if rows[2].TokenID != "tok-kept" || rows[2].Status != "never_connected" {
		t.Fatalf("kept token: %+v", rows[2])
	}
}

// "known" is a route of its own, not a session id.
func TestKnownClients_IsNotAnsweredByGetClient(t *testing.T) {
	var asked []string
	f := knownFake{asked: &asked, tokens: []db.ClientToken{{ID: "tok-1", Name: "laptop"}}}
	rows, _ := getKnown(t, f)
	if len(asked) != 0 {
		t.Fatalf("GET /clients/known reached GetClient(%v)", asked)
	}
	if len(rows) != 1 || rows[0].TokenID != "tok-1" {
		t.Fatalf("rows: %+v", rows)
	}
	// A session id still reaches GetClient.
	d := Deps{Log: discardLog(), Clients: f, Users: &fakeUserStore{role: "admin"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	r := authedClient(t, srv).get(t, "/api/v1/clients/s-1")
	r.Body.Close()
	if r.StatusCode != http.StatusOK || !reflect.DeepEqual(asked, []string{"s-1"}) {
		t.Fatalf("GET /clients/s-1: status=%d asked=%v", r.StatusCode, asked)
	}
}

func TestKnownClients_ListerFailureIs500(t *testing.T) {
	d := Deps{Log: discardLog(), Clients: knownFake{err: errors.New("database is locked")}, Users: &fakeUserStore{role: "admin"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	r := authedClient(t, srv).get(t, "/api/v1/clients/known")
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusInternalServerError || strings.Contains(string(body), "locked") {
		t.Fatalf("status=%d body=%s, want 500 without the cause", r.StatusCode, body)
	}
}
