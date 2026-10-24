package main

// tcp complete (E6, E7): the limits of a tcp service reach the relay at
// registration and on every refresh; its bans and daily usage are kept in
// the database; the relay's bans and quota warnings become webhooks and
// audit rows.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// The relay and the store agree on the bounds.
func TestTCPLimits_OneRange(t *testing.T) {
	type pair struct {
		name         string
		store, relay int64
	}
	for _, p := range []pair{
		{"rate min", store.MinTCPRateLimitKbps, server.MinTCPRateLimitKbps},
		{"rate max", store.MaxTCPRateLimitKbps, server.MaxTCPRateLimitKbps},
		{"daily min", store.MinTCPDailyBytes, server.MinTCPDailyBytes},
		{"daily max", store.MaxTCPDailyBytes, server.MaxTCPDailyBytes},
		{"short min", int64(time.Duration(store.MinTCPAutoBanShortS) * time.Second), int64(server.MinTCPAutoBanShort)},
		{"short max", int64(time.Duration(store.MaxTCPAutoBanShortS) * time.Second), int64(server.MaxTCPAutoBanShort)},
		{"count min", store.MinTCPAutoBanCount, server.MinTCPAutoBanCount},
		{"count max", store.MaxTCPAutoBanCount, server.MaxTCPAutoBanCount},
		{"window min", int64(time.Duration(store.MinTCPAutoBanWindowMin) * time.Minute), int64(server.MinTCPAutoBanWindow)},
		{"window max", int64(time.Duration(store.MaxTCPAutoBanWindowMin) * time.Minute), int64(server.MaxTCPAutoBanWindow)},
		{"ban min", int64(time.Duration(store.MinTCPAutoBanBanMin) * time.Minute), int64(server.MinTCPAutoBanBan)},
		{"ban max", int64(time.Duration(store.MaxTCPAutoBanBanMin) * time.Minute), int64(server.MaxTCPAutoBanBan)},
		{"bans", store.MaxTCPBans, server.MaxTCPBans},
	} {
		if p.store != p.relay {
			t.Errorf("%s: store %d, relay %d", p.name, p.store, p.relay)
		}
	}
	d := store.DefaultTCPAutoBan()
	if time.Duration(d.ShortS)*time.Second != server.DefaultTCPAutoBanShort || d.Count != server.DefaultTCPAutoBanCount ||
		time.Duration(d.WindowMin)*time.Minute != server.DefaultTCPAutoBanWindow || time.Duration(d.BanMin)*time.Minute != server.DefaultTCPAutoBanBan {
		t.Errorf("defaults: store %+v", d)
	}
}

// A ban by hand is written in the form the relay counts sources by.
func TestTCPBanSource_OneForm(t *testing.T) {
	for _, s := range []string{"203.0.113.7", "::ffff:203.0.113.7", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::", "fe80::1%eth0", "::1", "10.0.0.1"} {
		a := netip.MustParseAddr(s)
		got, ok := store.NormalizeTCPBanSource(s)
		if !ok || got != server.TCPSourceKey(a) {
			t.Errorf("%s: store %q, relay %q", s, got, server.TCPSourceKey(a))
		}
	}
}

func TestTCPLimits_ResolverAndLoaderReadThem(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	res := serviceResolverAdapter{db: x}
	r, err := res.ResolveTCP(ctx, uid, "web", server.ResolveOptions{Access: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Limits != (server.TCPLimits{AutoBan: server.TCPAutoBan{Short: 3 * time.Second, Count: 10, Window: 10 * time.Minute, Ban: time.Hour}}) {
		t.Fatalf("new service: %+v", r.Limits)
	}
	rate, daily, ab := 512, int64(1)<<30, `{"enabled":true,"short_s":5,"count":4,"window_min":2,"ban_min":30}`
	if err := x.SetServiceTCPLimits(ctx, r.ServiceID, &rate, &daily, &ab); err != nil {
		t.Fatal(err)
	}
	want := server.TCPLimits{RateLimitKbps: 512, DailyBytes: 1 << 30,
		AutoBan: server.TCPAutoBan{Enabled: true, Short: 5 * time.Second, Count: 4, Window: 2 * time.Minute, Ban: 30 * time.Minute}}
	again, err := res.ResolveTCP(ctx, uid, "web", server.ResolveOptions{})
	if err != nil || again.Limits != want {
		t.Fatalf("resolved %+v %v", again.Limits, err)
	}
	acc, err := tcpAccessAdapter{db: x}.TCPAccess(ctx, r.ServiceID)
	if err != nil || acc.Limits != want {
		t.Fatalf("access %+v %v", acc.Limits, err)
	}
	// Private: the limits are read as well (forwards are limited too).
	if err := x.SetServiceAccessMode(ctx, r.ServiceID, "private", ""); err != nil {
		t.Fatal(err)
	}
	if acc, err := (tcpAccessAdapter{db: x}).TCPAccess(ctx, r.ServiceID); err != nil || acc.Limits != want {
		t.Fatalf("private access %+v %v", acc.Limits, err)
	}
}

// The relay's bans and usage round trip through the database.
func TestTCPGuardStore(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	r, err := serviceResolverAdapter{db: x}.ResolveTCP(ctx, uid, "web", server.ResolveOptions{Access: "open"})
	if err != nil {
		t.Fatal(err)
	}
	st := tcpGuardStore{db: x}
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.SaveTCPBan(ctx, r.ServiceID, server.TCPBan{Source: "203.0.113.7", Until: now.Add(time.Hour), Reason: "auto: x", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	_ = st.SaveTCPBan(ctx, r.ServiceID, server.TCPBan{Source: "203.0.113.8", Until: now.Add(-time.Second), Reason: "auto: old", CreatedAt: now})
	bans, err := st.ActiveTCPBans(ctx, r.ServiceID, now)
	if err != nil || len(bans) != 1 || bans[0].Source != "203.0.113.7" || !bans[0].Until.Equal(now.Add(time.Hour)) || bans[0].Reason != "auto: x" {
		t.Fatalf("bans %+v %v", bans, err)
	}
	for range 3 {
		if err := st.AddTCPUsage(ctx, r.ServiceID, "2026-10-24", 10, 1<<33); err != nil {
			t.Fatal(err)
		}
	}
	if in, out, err := st.TCPUsage(ctx, r.ServiceID, "2026-10-24"); err != nil || in != 30 || out != 3<<33 {
		t.Fatalf("usage %d %d %v", in, out, err)
	}
}

// A ban of the relay is a webhook (limited per service and minute) and an
// audit row (every one); a quota warning is a webhook.
func TestObserver_TCPBannedAndQuotaWarning(t *testing.T) {
	o, _, em, au := newTestObserver(t)
	until := time.Now().Add(time.Hour)
	for i := range tcpBanEventsPerMinute + 5 {
		o.ObserveSession(observe.Session{Kind: observe.TCPBanned, ServiceID: "svc-rdp", OwnerID: "u1", UserID: "u1",
			SourceIP: "203.0.113." + strconv.Itoa(i), Until: until, Reason: "auto: 10 connections"})
	}
	got := em.named("tcp.banned")
	if len(got) != tcpBanEventsPerMinute || got[0]["service_id"] != "svc-rdp" || got[0]["source"] != "203.0.113.0" ||
		got[0]["ban_min"] != 60 || got[0]["reason"] != "auto: 10 connections" {
		t.Fatalf("tcp.banned: %d %v", len(got), got[:min(len(got), 1)])
	}
	fieldsOf(t, "tcp.banned", got[0])
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range au.actions() {
		if a == audit.ActionTCPBanAdd+":svc-rdp" {
			n++
		}
	}
	if n != tcpBanEventsPerMinute+5 {
		t.Fatalf("%d audit rows", n)
	}
	o.ObserveSession(observe.Session{Kind: observe.TCPQuotaWarning, ServiceID: "svc-rdp", Day: "2026-10-24", Bytes: 850, Limit: 1000})
	w := em.named("tcp.quota.warning")
	if len(w) != 1 || w[0]["percent"] != 85 || w[0]["day"] != "2026-10-24" || w[0]["bytes"] != int64(850) {
		t.Fatalf("tcp.quota.warning %v", w)
	}
	fieldsOf(t, "tcp.quota.warning", w[0])
}

// The whole relay: limits set through the API reach the live tunnel; short
// visits ban the source (listed, audited, announced), a lifted ban lets it
// in, a ban by hand refuses it, and the daily volume closes and refuses with
// reason quota and is shown in the limits.
func TestServe_TCPLimits(t *testing.T) {
	r := bootObsRelay(t)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	var token struct {
		Token string `json:"token"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "tcp-limits"}, http.StatusCreated, &token)
	stop := r.connectWith(token.Token, client.TunnelSpec{Name: "web", Type: "tcp", LocalAddr: target.Addr().String()})
	defer stop()
	var id string
	r.eventually("the service is served", func() bool {
		var rows []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Connected bool   `json:"connected"`
		}
		r.mustAdmin(http.MethodGet, "/api/v1/services", nil, http.StatusOK, &rows)
		for _, s := range rows {
			if s.Name == "web" && s.Connected {
				id = s.ID
			}
		}
		return id != ""
	})
	tunnel := r.tunnels()["web"].ID
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.tcpPort))
	// echo sends n bytes and reads them back; err when the relay closed it.
	echo := func(n int) error {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		go func() { _, _ = conn.Write(make([]byte, n)) }()
		_, err = io.ReadFull(conn, make([]byte, n))
		return err
	}
	type limits struct {
		RateLimitKbps int   `json:"rate_limit_kbps"`
		DailyBytes    int64 `json:"daily_bytes"`
		AutoBan       struct {
			Enabled bool `json:"enabled"`
			Count   int  `json:"count"`
		} `json:"autoban"`
		UsageToday struct {
			Bytes int64 `json:"bytes"`
		} `json:"usage_today"`
		QuotaReached bool `json:"quota_reached"`
	}
	var got limits
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-limits", map[string]any{"autoban": map[string]any{"enabled": true, "count": 2}}, http.StatusOK, &got)
	if !got.AutoBan.Enabled || got.AutoBan.Count != 2 {
		t.Fatalf("PUT %+v", got)
	}
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-limits", map[string]any{"autoban": map[string]any{"count": 1}}, http.StatusBadRequest, nil)

	type ban struct {
		Source string `json:"source"`
		Auto   bool   `json:"auto"`
	}
	bans := func() []ban {
		var out []ban
		r.mustAdmin(http.MethodGet, "/api/v1/services/"+id+"/tcp-bans", nil, http.StatusOK, &out)
		return out
	}
	// Two visits that end at once: banned.
	for range 2 {
		if err := echo(1); err != nil {
			t.Fatalf("visit: %v", err)
		}
	}
	r.eventually("the automatic ban", func() bool {
		b := bans()
		return len(b) == 1 && b[0].Source == "127.0.0.1" && b[0].Auto
	})
	if err := echo(1); err == nil {
		t.Fatal("a banned source was carried")
	}
	r.eventually("the ban's audit row and webhook", func() bool {
		_, body := r.admin(http.MethodGet, "/api/v1/audit/events?action=tcp.ban.add", nil)
		return strings.Contains(string(body), "tcp.ban.add") && r.hook.seen("tcp.banned")
	})
	// Lifted (the bans off, so that the next visits do not ban again).
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-limits", map[string]any{"autoban": map[string]any{"enabled": false}}, http.StatusOK, nil)
	r.mustAdmin(http.MethodDelete, "/api/v1/services/"+id+"/tcp-bans?source=127.0.0.1", nil, http.StatusNoContent, nil)
	r.mustAdmin(http.MethodDelete, "/api/v1/services/"+id+"/tcp-bans?source=127.0.0.1", nil, http.StatusNotFound, nil)
	if err := echo(1); err != nil {
		t.Fatalf("after the unban: %v", err)
	}
	// By hand.
	var b ban
	r.mustAdmin(http.MethodPost, "/api/v1/services/"+id+"/tcp-bans", map[string]any{"source": "127.0.0.1", "ban_min": 5}, http.StatusCreated, &b)
	if b.Source != "127.0.0.1" || b.Auto {
		t.Fatalf("ban %+v", b)
	}
	if err := echo(1); err == nil {
		t.Fatal("a source banned by hand was carried")
	}
	r.mustAdmin(http.MethodDelete, "/api/v1/services/"+id+"/tcp-bans?source=127.0.0.1", nil, http.StatusNoContent, nil)

	// The daily volume: 1 MiB.
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-limits", map[string]any{"daily_bytes": 1 << 20}, http.StatusOK, &got)
	if got.DailyBytes != 1<<20 || got.QuotaReached {
		t.Fatalf("PUT daily %+v", got)
	}
	if err := echo(600_000); err == nil {
		t.Fatal("a connection over the daily volume was carried to its end")
	}
	r.eventually("the quota", func() bool {
		r.mustAdmin(http.MethodGet, "/api/v1/services/"+id+"/tcp-limits", nil, http.StatusOK, &got)
		return got.QuotaReached && got.UsageToday.Bytes >= 1<<20
	})
	if err := echo(1); err == nil {
		t.Fatal("a new connection over the daily volume was carried")
	}
	r.eventually("the quota warning and the rows", func() bool {
		var rows []struct {
			Reason string `json:"reason"`
			Status string `json:"status"`
		}
		r.mustAdmin(http.MethodGet, "/api/v1/connection-logs?kind=tcp_proxy&service_id="+id, nil, http.StatusOK, &rows)
		closed, refused, banned := 0, 0, 0
		for _, row := range rows {
			switch {
			case row.Reason == "quota" && row.Status == "rejected":
				refused++
			case row.Reason == "quota":
				closed++
			case row.Reason == "banned" && row.Status == "rejected":
				banned++
			}
		}
		return closed == 1 && refused == 1 && banned >= 1 && r.hook.seen("tcp.quota.warning")
	})
	if tn := r.tunnels()["web"].ID; tn != tunnel {
		t.Fatalf("the client registered again (%s → %s)", tunnel, tn)
	}
	_, body := r.admin(http.MethodGet, "/api/v1/audit/events?action=service.tcp_limits.update", nil)
	if n := strings.Count(string(body), `"service.tcp_limits.update"`); n < 3 {
		var pretty any
		_ = json.Unmarshal(body, &pretty)
		t.Fatalf("%d limits audit rows: %v", n, pretty)
	}
}
