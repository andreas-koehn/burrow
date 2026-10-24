package api

// The bandwidth, daily volume and automatic bans of a tcp service, and its
// bans, through the API (tcp complete, E6 and E7).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// guardRelay is the relay as the limits and bans routes see it.
type guardRelay struct {
	liveTCP
	usage   map[string][3]any // service → day, in, out
	reached map[string]bool
	bans    []string // "ban <service> <source> <until>" and "unban <service> <source>"
	held    map[string][]server.TCPBan
}

func (g *guardRelay) TCPUsageToday(id string) (string, uint64, uint64, bool) {
	u, ok := g.usage[id]
	if !ok {
		return "", 0, 0, false
	}
	return u[0].(string), u[1].(uint64), u[2].(uint64), true
}
func (g *guardRelay) TCPQuotaReached(id string) bool { return g.reached[id] }
func (g *guardRelay) BanTCPSource(id, source string, until time.Time, reason string) {
	g.bans = append(g.bans, fmt.Sprintf("ban %s %s %s %s", id, source, until.UTC().Format(time.RFC3339), reason))
}
func (g *guardRelay) UnbanTCPSource(id, source string) bool {
	g.bans = append(g.bans, "unban "+id+" "+source)
	for i, b := range g.held[id] {
		if b.Source == source {
			g.held[id] = append(g.held[id][:i], g.held[id][i+1:]...)
			return true
		}
	}
	return false
}
func (g *guardRelay) TCPBans(id string) []server.TCPBan { return g.held[id] }

func decodeTCPLimits(t *testing.T, r *http.Response) tcpLimitsResp {
	t.Helper()
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	var s tcpLimitsResp
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTCPLimits_GetAndPut(t *testing.T) {
	relay := &guardRelay{usage: map[string][3]any{}, reached: map[string]bool{}}
	a := newTCPAPI(t, func(d *Deps) { d.Connections = relay })
	ab := store.DefaultTCPAutoBan()
	a.svcs.tcpLimits = store.TCPLimits{RateLimitKbps: 1024, AutoBan: ab}
	a.svcs.tcpUsage = store.TCPUsage{Day: "2026-10-24", BytesIn: 5, BytesOut: 6}

	// Offline: the usage as stored.
	got := decodeTCPLimits(t, a.c.get(t, "/api/v1/services/rdp/tcp-limits"))
	if got.RateLimitKbps != 1024 || got.DailyBytes != 0 || got.AutoBan != (tcpAutoBanJSON{ShortS: 3, Count: 10, WindowMin: 10, BanMin: 60}) ||
		got.UsageToday != (tcpUsageJSON{Day: "2026-10-24", BytesIn: 5, BytesOut: 6, Bytes: 11}) || got.QuotaReached {
		t.Fatalf("GET offline %+v", got)
	}
	// Served: the relay's count, written or not.
	relay.usage["rdp"] = [3]any{"2026-10-24", uint64(700), uint64(800)}
	relay.reached["rdp"] = true
	got = decodeTCPLimits(t, a.c.get(t, "/api/v1/services/rdp/tcp-limits"))
	if got.UsageToday != (tcpUsageJSON{Day: "2026-10-24", BytesIn: 700, BytesOut: 800, Bytes: 1500}) || !got.QuotaReached {
		t.Fatalf("GET served %+v", got)
	}

	got = decodeTCPLimits(t, a.c.put(t, "/api/v1/services/rdp/tcp-limits", map[string]any{
		"daily_bytes": 1 << 30, "autoban": map[string]any{"enabled": true, "count": 5},
	}))
	if got.DailyBytes != 1<<30 || got.RateLimitKbps != 1024 || !got.AutoBan.Enabled || got.AutoBan.Count != 5 || got.AutoBan.BanMin != 60 {
		t.Fatalf("PUT %+v", got)
	}
	u := a.svcs.lastTCPLimits
	if u.RateLimitKbps != nil || u.DailyBytes == nil || *u.DailyBytes != 1<<30 || u.AutoBan == nil || u.AutoBan.Enabled == nil ||
		!*u.AutoBan.Enabled || u.AutoBan.Count == nil || *u.AutoBan.Count != 5 || u.AutoBan.ShortS != nil {
		t.Fatalf("update %+v", u)
	}
	if refreshed, _ := a.relay.snapshot(); !slices.Equal(refreshed, []string{"rdp"}) {
		t.Fatalf("relay told %v", refreshed)
	}

	for _, c := range []struct {
		name string
		body string
		err  error
		want int
		text string
	}{
		{"nothing to change", `{}`, nil, http.StatusBadRequest, "rate_limit_kbps, daily_bytes or autoban"},
		{"an empty autoban", `{"autoban": {}}`, nil, http.StatusBadRequest, "autoban"},
		{"a field it does not know", `{"idle_timeout_s": 5}`, nil, http.StatusBadRequest, "unknown"},
		{"an autoban field it does not know", `{"autoban": {"forever": true}}`, nil, http.StatusBadRequest, "unknown"},
		{"not json", `nope`, nil, http.StatusBadRequest, ""},
		{"out of bounds", `{"rate_limit_kbps": 1}`, fmt.Errorf("%w: rate_limit_kbps must be 0 (none) or 64 to 10000000", store.ErrInvalidTCPLimits),
			http.StatusBadRequest, "rate_limit_kbps must be 0 (none) or 64 to 10000000"},
		{"http service", `{"rate_limit_kbps": 64}`, store.ErrServiceNotTCP, http.StatusConflict, "tcp services only"},
		{"another user's", `{"rate_limit_kbps": 64}`, store.ErrForbidden, http.StatusForbidden, ""},
		{"missing", `{"rate_limit_kbps": 64}`, db.ErrNotFound, http.StatusNotFound, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a.svcs.tcpLimitsErr = c.err
			defer func() { a.svcs.tcpLimitsErr = nil }()
			r := a.c.putRaw(t, "/api/v1/services/rdp/tcp-limits", c.body)
			body := readBody(t, r)
			if r.StatusCode != c.want || !strings.Contains(body, c.text) || strings.Contains(body, "store:") {
				t.Fatalf("status %d %s, want %d with %q", r.StatusCode, body, c.want, c.text)
			}
		})
	}
	a.svcs.tcpLimitsErr = store.ErrServiceNotTCP
	if r := a.c.get(t, "/api/v1/services/web/tcp-limits"); r.StatusCode != http.StatusConflict {
		t.Fatalf("GET http: %d", r.StatusCode)
	}
	if refreshed, _ := a.relay.snapshot(); len(refreshed) != 1 {
		t.Fatalf("a refused write told the relay: %v", refreshed)
	}
}

func TestTCPBans_ListBanUnban(t *testing.T) {
	relay := &guardRelay{}
	a := newTCPAPI(t, func(d *Deps) { d.Connections = relay })
	until := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
	a.svcs.tcpBans = []db.TCPBan{
		{ServiceID: "rdp", Source: "203.0.113.7", Until: until, Reason: "auto: 10 connections shorter than 3s within 10m0s", CreatedAt: until.Add(-time.Hour)},
		{ServiceID: "rdp", Source: "2001:db8:1:2::/64", Until: until, Reason: "manual: scanner", CreatedAt: until.Add(-time.Hour)},
	}
	r := a.c.get(t, "/api/v1/services/rdp/tcp-bans")
	var bans []tcpBanResp
	if err := json.NewDecoder(r.Body).Decode(&bans); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %v", r.StatusCode, err)
	}
	r.Body.Close()
	if len(bans) != 2 || !bans[0].Auto || bans[1].Auto || bans[1].Source != "2001:db8:1:2::/64" || !bans[0].Until.Equal(until) {
		t.Fatalf("bans %+v", bans)
	}
	a.svcs.tcpBans = nil
	if body := readBody(t, a.c.get(t, "/api/v1/services/rdp/tcp-bans")); strings.TrimSpace(body) != "[]" {
		t.Fatalf("no bans: %s", body)
	}

	r = a.c.post(t, "/api/v1/services/rdp/tcp-bans", map[string]any{"source": "203.0.113.9", "ban_min": 30, "note": "brute force"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("ban: %d %s", r.StatusCode, readBody(t, r))
	}
	var b tcpBanResp
	_ = json.NewDecoder(r.Body).Decode(&b)
	r.Body.Close()
	if b.Source != "203.0.113.9" || b.Reason != "manual: brute force" || b.Auto {
		t.Fatalf("ban answer %+v", b)
	}
	if l := a.svcs.lastBan; l != "203.0.113.9 30 brute force" {
		t.Fatalf("store got %q", l)
	}
	if len(relay.bans) != 1 || !strings.HasPrefix(relay.bans[0], "ban rdp 203.0.113.9 ") || !strings.HasSuffix(relay.bans[0], " manual: brute force") {
		t.Fatalf("relay %v", relay.bans)
	}

	if r := a.c.delete(t, "/api/v1/services/rdp/tcp-bans?source=203.0.113.9"); r.StatusCode != http.StatusNoContent {
		t.Fatalf("unban: %d %s", r.StatusCode, readBody(t, r))
	}
	if a.svcs.lastUnban != "203.0.113.9" || relay.bans[1] != "unban rdp 203.0.113.9" {
		t.Fatalf("unban: store %q relay %v", a.svcs.lastUnban, relay.bans)
	}
	// An address of a /64 lifts the /64: the relay is told the stored source.
	a.svcs.unbanSource = "2001:db8:1:2::/64"
	if r := a.c.delete(t, "/api/v1/services/rdp/tcp-bans?source=2001:db8:1:2::5"); r.StatusCode != http.StatusNoContent {
		t.Fatalf("unban v6: %d", r.StatusCode)
	}
	if relay.bans[2] != "unban rdp 2001:db8:1:2::/64" {
		t.Fatalf("relay %v", relay.bans)
	}

	for _, c := range []struct {
		name, method, path, body string
		err                      error
		want                     int
		text                     string
	}{
		{"ban without a source", http.MethodPost, "/tcp-bans", `{"ban_min": 5}`, nil, http.StatusBadRequest, "source"},
		{"ban without a duration", http.MethodPost, "/tcp-bans", `{"source": "203.0.113.1"}`, nil, http.StatusBadRequest, "ban_min"},
		{"ban an unknown field", http.MethodPost, "/tcp-bans", `{"source": "203.0.113.1", "ban_min": 5, "forever": true}`, nil, http.StatusBadRequest, "unknown"},
		{"ban a range", http.MethodPost, "/tcp-bans", `{"source": "203.0.113.0/24", "ban_min": 5}`,
			fmt.Errorf("%w: source must be an IP address, or an IPv6 /64; a ban is for one source", store.ErrInvalidTCPBan), http.StatusBadRequest, "one source"},
		{"ban shorter than the active one", http.MethodPost, "/tcp-bans", `{"source": "203.0.113.1", "ban_min": 5}`, store.ErrTCPBanLonger, http.StatusConflict,
			"DELETE /api/v1/services/{id}/tcp-bans?source="},
		{"too many bans", http.MethodPost, "/tcp-bans", `{"source": "203.0.113.1", "ban_min": 5}`, store.ErrTooManyTCPBans, http.StatusConflict, "4096"},
		{"ban on http", http.MethodPost, "/tcp-bans", `{"source": "203.0.113.1", "ban_min": 5}`, store.ErrServiceNotTCP, http.StatusConflict, "tcp services only"},
		{"ban another user's", http.MethodPost, "/tcp-bans", `{"source": "203.0.113.1", "ban_min": 5}`, store.ErrForbidden, http.StatusForbidden, ""},
		{"unban without a source", http.MethodDelete, "/tcp-bans", ``, nil, http.StatusBadRequest, "source"},
		{"unban what is not banned", http.MethodDelete, "/tcp-bans?source=203.0.113.1", ``, store.ErrNoTCPBan, http.StatusNotFound, "no ban of that source"},
		{"unban on an unknown service", http.MethodDelete, "/tcp-bans?source=203.0.113.1", ``, db.ErrNotFound, http.StatusNotFound, "service not found"},
		{"unban nonsense", http.MethodDelete, "/tcp-bans?source=x", ``, fmt.Errorf("%w: source must be an IP address, or an IPv6 /64", store.ErrInvalidTCPBan),
			http.StatusBadRequest, "IP address"},
		{"list another user's", http.MethodGet, "/tcp-bans", ``, store.ErrForbidden, http.StatusForbidden, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a.svcs.tcpBanErr = c.err
			defer func() { a.svcs.tcpBanErr = nil }()
			n := len(relay.bans)
			r := a.c.doRaw(t, c.method, "/api/v1/services/rdp"+c.path, c.body)
			body := readBody(t, r)
			if r.StatusCode != c.want || !strings.Contains(body, c.text) || strings.Contains(body, "store:") {
				t.Fatalf("status %d %s, want %d with %q", r.StatusCode, body, c.want, c.text)
			}
			if len(relay.bans) != n {
				t.Fatalf("a refused write told the relay: %v", relay.bans)
			}
		})
	}
}

// doRaw sends body as it is ("" sends none).
func (c *authClient) doRaw(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The relay's bans are listed with the stored ones, and lifted by a DELETE
// though the store has none of them (a write that failed).
func TestTCPBans_HeldBansAreListedAndLifted(t *testing.T) {
	until := time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)
	relay := &guardRelay{held: map[string][]server.TCPBan{"rdp": {
		{Source: "198.51.100.1", Until: until.Add(-time.Minute), Reason: "auto: 10 connections", CreatedAt: until.Add(-time.Hour)},
		{Source: "203.0.113.7", Until: until.Add(time.Minute), Reason: "auto: newer", CreatedAt: until},
	}}}
	a := newTCPAPI(t, func(d *Deps) { d.Connections = relay })
	a.svcs.tcpBans = []db.TCPBan{{ServiceID: "rdp", Source: "203.0.113.7", Until: until, Reason: "manual: stored", CreatedAt: until.Add(-time.Hour)}}
	r := a.c.get(t, "/api/v1/services/rdp/tcp-bans")
	var bans []tcpBanResp
	if err := json.NewDecoder(r.Body).Decode(&bans); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %v", r.StatusCode, err)
	}
	r.Body.Close()
	if len(bans) != 2 || bans[0].Source != "198.51.100.1" || !bans[0].Auto || bans[1].Source != "203.0.113.7" || bans[1].Reason != "manual: stored" {
		t.Fatalf("bans %+v", bans)
	}

	a.svcs.noBanRow = true
	if r := a.c.delete(t, "/api/v1/services/rdp/tcp-bans?source=198.51.100.1"); r.StatusCode != http.StatusNoContent {
		t.Fatalf("unban a held ban: %d %s", r.StatusCode, readBody(t, r))
	}
	if len(relay.held["rdp"]) != 1 {
		t.Fatalf("held %+v", relay.held["rdp"])
	}
	r = a.c.delete(t, "/api/v1/services/rdp/tcp-bans?source=198.51.100.1")
	if body := readBody(t, r); r.StatusCode != http.StatusNotFound || !strings.Contains(body, "no ban of that source") {
		t.Fatalf("second unban: %d %s", r.StatusCode, body)
	}
}

// A ban by hand never shortens a ban the relay holds (one the store may
// not have): 409 pointing to the unban, and nothing reaches the store or the
// relay. A longer one is made.
func TestTCPBans_AShorterBanThanTheRelaysIsRefused(t *testing.T) {
	now := time.Now().UTC()
	relay := &guardRelay{held: map[string][]server.TCPBan{"rdp": {
		{Source: "203.0.113.7", Until: now.Add(2 * time.Hour), Reason: "auto: 10 connections", CreatedAt: now},
	}}}
	a := newTCPAPI(t, func(d *Deps) { d.Connections = relay })
	r := a.c.post(t, "/api/v1/services/rdp/tcp-bans", map[string]any{"source": "203.0.113.7", "ban_min": 5})
	body := readBody(t, r)
	if r.StatusCode != http.StatusConflict || !strings.Contains(body, "DELETE") || !strings.Contains(body, "longer") {
		t.Fatalf("shorter: %d %s", r.StatusCode, body)
	}
	if a.svcs.lastBan != "" || len(relay.bans) != 0 {
		t.Fatalf("a refused ban went on: store %q relay %v", a.svcs.lastBan, relay.bans)
	}
	r = a.c.post(t, "/api/v1/services/rdp/tcp-bans", map[string]any{"source": "203.0.113.7", "ban_min": 180})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("longer: %d %s", r.StatusCode, readBody(t, r))
	}
	r.Body.Close()
	if len(relay.bans) != 1 {
		t.Fatalf("relay %v", relay.bans)
	}
}
