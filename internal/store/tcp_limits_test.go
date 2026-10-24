package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

func auditPayloads(sink *auditSink, action string) []string {
	var out []string
	for _, e := range sink.ev {
		if e.Action == action {
			out = append(out, string(e.Payload))
		}
	}
	return out
}

// The limits of a tcp service (E6, E7): partial updates, the bounds, the
// defaults of the ban settings, the permission rule, and one audit row per
// write with both states.
func TestServiceTCPLimits(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	other := mustCreateUser(t, st, "other@x", "user")
	sink := &auditSink{}
	st.SetAuditLogger(sink)
	web := mustGetOrCreateService(t, st, owner.ID, "web", "http")
	rdp := mustGetOrCreateService(t, st, owner.ID, "rdp", "tcp")

	got, usage, err := st.ServiceTCPLimits(ctx, owner.ID, "user", rdp.ID)
	if err != nil || got != (TCPLimits{AutoBan: DefaultTCPAutoBan()}) || usage.Day != time.Now().UTC().Format(time.DateOnly) || usage.BytesIn != 0 {
		t.Fatalf("new service: %+v %+v %v", got, usage, err)
	}
	if d := DefaultTCPAutoBan(); d.Enabled || d.ShortS != 3 || d.Count != 10 || d.WindowMin != 10 || d.BanMin != 60 {
		t.Fatalf("defaults %+v", d)
	}
	rate, daily, on := 2048, int64(5)<<30, true
	before, after, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", rdp.ID, TCPLimitsUpdate{
		RateLimitKbps: &rate, DailyBytes: &daily, AutoBan: &TCPAutoBanUpdate{Enabled: &on},
	})
	wantAB := DefaultTCPAutoBan()
	wantAB.Enabled = true
	if err != nil || before != (TCPLimits{AutoBan: DefaultTCPAutoBan()}) ||
		after != (TCPLimits{RateLimitKbps: 2048, DailyBytes: 5 << 30, AutoBan: wantAB}) {
		t.Fatalf("set: %+v %+v %v", before, after, err)
	}
	// A ban field alone keeps the others; other fields stay.
	count := 5
	if _, after, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", rdp.ID, TCPLimitsUpdate{AutoBan: &TCPAutoBanUpdate{Count: &count}}); err != nil ||
		after.AutoBan.Count != 5 || !after.AutoBan.Enabled || after.AutoBan.BanMin != 60 || after.RateLimitKbps != 2048 {
		t.Fatalf("count alone: %+v %v", after, err)
	}
	if got, _, _ := st.ServiceTCPLimits(ctx, owner.ID, "user", rdp.ID); got.AutoBan.Count != 5 || got.DailyBytes != 5<<30 {
		t.Fatalf("read back %+v", got)
	}
	// Bounds.
	ints := func(vs ...int) []int { return vs }
	for _, n := range ints(-1, 1, 63, 10_000_001) {
		if _, _, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", rdp.ID, TCPLimitsUpdate{RateLimitKbps: &n}); !errors.Is(err, ErrInvalidTCPLimits) ||
			!strings.Contains(err.Error(), "rate_limit_kbps") {
			t.Fatalf("rate %d: %v", n, err)
		}
	}
	for _, n := range ints(0, 64, 10_000_000) {
		if _, _, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", rdp.ID, TCPLimitsUpdate{RateLimitKbps: &n}); err != nil {
			t.Fatalf("rate %d: %v", n, err)
		}
	}
	for _, n := range []int64{-1, 1, 1<<20 - 1, 1<<50 + 1} {
		if _, _, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", rdp.ID, TCPLimitsUpdate{DailyBytes: &n}); !errors.Is(err, ErrInvalidTCPLimits) ||
			!strings.Contains(err.Error(), "daily_bytes") {
			t.Fatalf("daily %d: %v", n, err)
		}
	}
	for _, n := range []int64{0, 1 << 20, 1 << 50} {
		if _, _, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", rdp.ID, TCPLimitsUpdate{DailyBytes: &n}); err != nil {
			t.Fatalf("daily %d: %v", n, err)
		}
	}
	bad := []struct {
		field string
		u     TCPAutoBanUpdate
	}{
		{"short_s", TCPAutoBanUpdate{ShortS: ptrTo(0)}}, {"short_s", TCPAutoBanUpdate{ShortS: ptrTo(301)}},
		{"count", TCPAutoBanUpdate{Count: ptrTo(1)}}, {"count", TCPAutoBanUpdate{Count: ptrTo(101)}},
		{"window_min", TCPAutoBanUpdate{WindowMin: ptrTo(0)}}, {"window_min", TCPAutoBanUpdate{WindowMin: ptrTo(1441)}},
		{"ban_min", TCPAutoBanUpdate{BanMin: ptrTo(0)}}, {"ban_min", TCPAutoBanUpdate{BanMin: ptrTo(43201)}},
	}
	for _, c := range bad {
		if _, _, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", rdp.ID, TCPLimitsUpdate{AutoBan: &c.u}); !errors.Is(err, ErrInvalidTCPLimits) ||
			!strings.Contains(err.Error(), c.field) {
			t.Fatalf("%s %+v: %v", c.field, c.u, err)
		}
	}
	if _, _, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", rdp.ID, TCPLimitsUpdate{AutoBan: &TCPAutoBanUpdate{
		ShortS: ptrTo(300), Count: ptrTo(100), WindowMin: ptrTo(1440), BanMin: ptrTo(43200),
	}}); err != nil {
		t.Fatalf("upper bounds: %v", err)
	}
	if _, _, err := st.SetServiceTCPLimits(ctx, owner.ID, "user", web.ID, TCPLimitsUpdate{RateLimitKbps: &rate}); !errors.Is(err, ErrServiceNotTCP) {
		t.Fatalf("http: %v", err)
	}
	if _, _, err := st.ServiceTCPLimits(ctx, owner.ID, "user", web.ID); !errors.Is(err, ErrServiceNotTCP) {
		t.Fatalf("http read: %v", err)
	}
	if _, _, err := st.SetServiceTCPLimits(ctx, other.ID, "user", rdp.ID, TCPLimitsUpdate{RateLimitKbps: &rate}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user: %v", err)
	}
	if _, _, err := st.ServiceTCPLimits(ctx, other.ID, "user", rdp.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user reads: %v", err)
	}
	writes := auditPayloads(sink, audit.ActionServiceTCPLimitsUpdate)
	if len(writes) != 9 {
		t.Fatalf("%d audit rows:\n%s", len(writes), strings.Join(writes, "\n"))
	}
	if want := `{"from":{"rate_limit_kbps":0,"daily_bytes":0,"autoban":{"enabled":false,"short_s":3,"count":10,"window_min":10,"ban_min":60}},` +
		`"to":{"rate_limit_kbps":2048,"daily_bytes":5368709120,"autoban":{"enabled":true,"short_s":3,"count":10,"window_min":10,"ban_min":60}}}`; writes[0] != want {
		t.Fatalf("audit:\n%s\nwant\n%s", writes[0], want)
	}
	for _, a := range []string{audit.ActionServiceTCPLimitsUpdate, audit.ActionTCPBanAdd, audit.ActionTCPBanRemove} {
		if !slices.Contains(audit.AllActions, a) {
			t.Fatalf("%s is not an audit action", a)
		}
	}
	// The usage of today is read from the store.
	if err := st.q.AddTCPDailyUsage(ctx, rdp.ID, time.Now().UTC().Format(time.DateOnly), 10, 20); err != nil {
		t.Fatal(err)
	}
	if _, usage, _ := st.ServiceTCPLimits(ctx, owner.ID, "user", rdp.ID); usage.BytesIn != 10 || usage.BytesOut != 20 {
		t.Fatalf("usage %+v", usage)
	}
}

func ptrTo[T any](v T) *T { return &v }

// The address a ban is for: an IPv4 address, or the /64 of an IPv6 one, the
// way the relay counts sources.
func TestNormalizeTCPBanSource(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":              "203.0.113.7",
		" 203.0.113.7 ":            "203.0.113.7",
		"203.0.113.7/32":           "203.0.113.7",
		"::ffff:203.0.113.7":       "203.0.113.7",
		"2001:db8:1:2:3:4:5:6":     "2001:db8:1:2::/64",
		"2001:DB8:1:2::/64":        "2001:db8:1:2::/64",
		"2001:db8:1:2:aaaa::/80":   "2001:db8:1:2::/64",
		"fe80::1%eth0":             "fe80::/64",
		"2001:db8:1:2:3:4:5:6/128": "2001:db8:1:2::/64",
		"::ffff:203.0.113.7/128":   "203.0.113.7",
		"203.0.113.0/24":           "",
		"2001:db8::/48":            "",
		"example.com":              "",
		"":                         "",
		"0.0.0.0/0":                "",
	} {
		got, ok := NormalizeTCPBanSource(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
}

// Bans by hand: validated, listed until they expire, capped, removed, and
// each audited.
func TestTCPBans(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	other := mustCreateUser(t, st, "other@x", "user")
	sink := &auditSink{}
	st.SetAuditLogger(sink)
	web := mustGetOrCreateService(t, st, owner.ID, "web", "http")
	rdp := mustGetOrCreateService(t, st, owner.ID, "rdp", "tcp")

	b, err := st.AddTCPBan(ctx, owner.ID, "user", rdp.ID, "2001:db8:1:2::99", 30, "scanner")
	if err != nil || b.Source != "2001:db8:1:2::/64" || b.Reason != "manual: scanner" || time.Until(b.Until) < 29*time.Minute {
		t.Fatalf("add: %+v %v", b, err)
	}
	if _, err := st.AddTCPBan(ctx, owner.ID, "user", rdp.ID, "203.0.113.7", 60, ""); err != nil {
		t.Fatal(err)
	}
	// An auto ban of the relay, and an expired one.
	_ = st.q.PutTCPBan(ctx, db.TCPBan{ServiceID: rdp.ID, Source: "198.51.100.1", Until: time.Now().Add(time.Hour), Reason: "auto: 10 connections"})
	_ = st.q.PutTCPBan(ctx, db.TCPBan{ServiceID: rdp.ID, Source: "198.51.100.2", Until: time.Now().Add(-time.Hour), Reason: "auto"})
	bans, err := st.ListTCPBans(ctx, owner.ID, "user", rdp.ID)
	if err != nil || len(bans) != 3 || bans[0].Source != "2001:db8:1:2::/64" {
		t.Fatalf("list: %+v %v", bans, err)
	}
	// The list is a read: the expired ban is still stored (the relay
	// prunes).
	if n, err := st.q.PruneTCPBans(ctx, rdp.ID, time.Now()); err != nil || n != 1 {
		t.Fatalf("the list pruned the expired ban: %d %v", n, err)
	}
	for _, c := range []struct {
		source string
		min    int
		note   string
		want   error
	}{
		{"203.0.113.0/24", 60, "", ErrInvalidTCPBan},
		{"not an address", 60, "", ErrInvalidTCPBan},
		{"203.0.113.9", 0, "", ErrInvalidTCPBan},
		{"203.0.113.9", 43201, "", ErrInvalidTCPBan},
		{"203.0.113.9", 60, strings.Repeat("x", 201), ErrInvalidTCPBan},
	} {
		if _, err := st.AddTCPBan(ctx, owner.ID, "user", rdp.ID, c.source, c.min, c.note); !errors.Is(err, c.want) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	if _, err := st.AddTCPBan(ctx, owner.ID, "user", web.ID, "203.0.113.9", 60, ""); !errors.Is(err, ErrServiceNotTCP) {
		t.Fatalf("http: %v", err)
	}
	if _, err := st.AddTCPBan(ctx, other.ID, "user", rdp.ID, "203.0.113.9", 60, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user: %v", err)
	}
	if _, err := st.ListTCPBans(ctx, other.ID, "user", rdp.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user lists: %v", err)
	}
	if _, err := st.RemoveTCPBan(ctx, other.ID, "user", rdp.ID, "203.0.113.7", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another user removes: %v", err)
	}

	// The relay is told before the row goes (a write of the relay under
	// way then ends before the delete).
	var told []string
	held := func(src string) bool {
		active, _ := st.q.ListTCPBans(ctx, rdp.ID, time.Now())
		stored := false
		for _, b := range active {
			stored = stored || b.Source == src
		}
		told = append(told, fmt.Sprintf("%s stored=%v", src, stored))
		return src == "192.0.2.50"
	}
	got, err := st.RemoveTCPBan(ctx, owner.ID, "user", rdp.ID, "2001:db8:1:2::1", held)
	if err != nil || got.Source != "2001:db8:1:2::/64" || len(told) != 1 || told[0] != "2001:db8:1:2::/64 stored=true" {
		t.Fatalf("remove by an address of the /64: %+v %v; told %v", got, err, told)
	}
	if _, err := st.RemoveTCPBan(ctx, owner.ID, "user", rdp.ID, "2001:db8:1:2::1", held); !errors.Is(err, ErrNoTCPBan) {
		t.Fatalf("second remove: %v", err)
	}
	// A ban only the relay holds is lifted.
	if got, err := st.RemoveTCPBan(ctx, owner.ID, "user", rdp.ID, "192.0.2.50", held); err != nil || got.Source != "192.0.2.50" {
		t.Fatalf("remove a held ban: %+v %v", got, err)
	}
	if _, err := st.RemoveTCPBan(ctx, owner.ID, "user", rdp.ID, "nonsense", held); !errors.Is(err, ErrInvalidTCPBan) {
		t.Fatalf("bad source: %v", err)
	}
	if _, err := st.RemoveTCPBan(ctx, owner.ID, "user", "no-such-service", "192.0.2.50", held); !errors.Is(err, db.ErrNotFound) || errors.Is(err, ErrNoTCPBan) {
		t.Fatalf("unknown service: %v", err)
	}

	adds, removes := auditPayloads(sink, audit.ActionTCPBanAdd), auditPayloads(sink, audit.ActionTCPBanRemove)
	if len(adds) != 2 || !strings.Contains(adds[0], `"source":"2001:db8:1:2::/64"`) || !strings.Contains(adds[0], `"reason":"manual: scanner"`) ||
		!strings.Contains(adds[0], `"ban_min":30`) {
		t.Fatalf("adds %v", adds)
	}
	if len(removes) != 2 || !strings.Contains(removes[0], `"source":"2001:db8:1:2::/64"`) || !strings.Contains(removes[1], `"source":"192.0.2.50"`) {
		t.Fatalf("removes %v", removes)
	}
}

// At most MaxTCPBans are active; renewing one that is there is not refused.
func TestTCPBans_Capped(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	rdp := mustGetOrCreateService(t, st, owner.ID, "rdp", "tcp")
	until := time.Now().Add(time.Hour)
	for i := range MaxTCPBans {
		if err := st.q.PutTCPBan(ctx, db.TCPBan{ServiceID: rdp.ID, Source: "10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256), Until: until}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddTCPBan(ctx, owner.ID, "user", rdp.ID, "203.0.113.1", 60, ""); !errors.Is(err, ErrTooManyTCPBans) {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := st.AddTCPBan(ctx, owner.ID, "user", rdp.ID, "10.0.0.1", 60, ""); err != nil {
		t.Fatalf("renew: %v", err)
	}
}
