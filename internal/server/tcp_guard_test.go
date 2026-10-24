package server

// Automatic bans, bandwidth and daily volume of tcp services (tcp complete,
// E6 and E7).

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// fakeGuardStore is a TCPGuardStore in memory.
type fakeGuardStore struct {
	mu    sync.Mutex
	bans  map[string]map[string]TCPBan // service → source → ban
	usage map[string][2]uint64         // service|day → in, out
	err   error
	adds  int
}

func newFakeGuardStore() *fakeGuardStore {
	return &fakeGuardStore{bans: map[string]map[string]TCPBan{}, usage: map[string][2]uint64{}}
}

func (f *fakeGuardStore) ActiveTCPBans(_ context.Context, id string, now time.Time) ([]TCPBan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []TCPBan
	for _, b := range f.bans[id] {
		if now.Before(b.Until) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeGuardStore) SaveTCPBan(_ context.Context, id string, b TCPBan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bans[id] == nil {
		f.bans[id] = map[string]TCPBan{}
	}
	f.bans[id][b.Source] = b
	return nil
}

func (f *fakeGuardStore) TCPUsage(_ context.Context, id, day string) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, 0, f.err
	}
	u := f.usage[id+"|"+day]
	return u[0], u[1], nil
}

func (f *fakeGuardStore) AddTCPUsage(_ context.Context, id, day string, in, out uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	u := f.usage[id+"|"+day]
	f.usage[id+"|"+day] = [2]uint64{u[0] + in, u[1] + out}
	f.adds++
	return nil
}

func (f *fakeGuardStore) ban(id, source string) (TCPBan, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.bans[id][source]
	return b, ok
}

func (f *fakeGuardStore) used(id, day string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.usage[id+"|"+day]
	return u[0] + u[1]
}

// testClock is the time now plus an offset a test moves.
type testClock struct{ off atomic.Int64 }

func (c *testClock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *testClock) advance(d time.Duration) { c.off.Add(int64(d)) }

// newGuardRelay is a forwardRelay on port whose service "web" (svc-web) has
// limits l and is served in mode open; its home client greets with "hi "
// and echoes.
func newGuardRelay(t *testing.T, port int, st TCPGuardStore, clk *testClock, l TCPLimits) *forwardRelay {
	t.Helper()
	r := newForwardRelay(t, port, func(o *Options) {
		if st != nil {
			o.TCPGuards = st
		}
		if clk != nil {
			o.clock = clk.now
		}
	})
	r.res.mu.Lock()
	r.res.limits["svc-web"] = l
	r.res.mu.Unlock()
	r.access.set("svc-web", TCPAccess{Mode: TCPAccessOpen, Limits: l})
	if rr := r.home.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK || rr.RemotePort != port {
		t.Fatalf("register: %+v", rr)
	}
	return r
}

// dialFrom connects to the port from the loopback address src.
func dialFrom(t *testing.T, src string, port int) net.Conn {
	t.Helper()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 2 * time.Second}
	conn, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// greeted connects from src and reads the home client's greeting.
func greeted(t *testing.T, src string, port int) net.Conn {
	t.Helper()
	conn := dialFrom(t, src, port)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got := readN(t, conn, 3); got != "hi " {
		t.Fatalf("greeting %q", got)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn
}

// refusedAtOnce connects from src and checks that the relay closes the
// connection without a byte and without telling the home client.
func (r *forwardRelay) refusedAtOnce(t *testing.T, src string, port int) {
	t.Helper()
	before := r.told.Load()
	conn := dialFrom(t, src, port)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(make([]byte, 8))
	if n != 0 || err == nil {
		t.Fatalf("from %s: read %d bytes, %v; want a close without a byte", src, n, err)
	}
	settle()
	if r.told.Load() != before {
		t.Fatalf("the home client was told of a refused visitor from %s", src)
	}
}

// shortVisits makes n visits from src that end at once, each after the home
// client greeted it, and waits for their rows.
func (r *forwardRelay) shortVisits(t *testing.T, src string, port, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		before := len(r.closedFrom(src))
		conn := greeted(t, src, port)
		_ = conn.Close()
		r.waitFor("the row of a short visit", func() bool { return len(r.closedFrom(src)) > before })
	}
}

// closedFrom are the rows of visitors from src that were carried (not
// refused).
func (r *forwardRelay) closedFrom(src string) []ConnLogEntry {
	var out []ConnLogEntry
	for _, e := range r.rows(observe.TrafficTCP) {
		if e.SourceIP == src && e.Status != observe.OutcomeRejected {
			out = append(out, e)
		}
	}
	return out
}

func autoBan(count int, ban time.Duration) TCPLimits {
	return TCPLimits{AutoBan: TCPAutoBan{Enabled: true, Short: 2 * time.Second, Count: count, Window: time.Minute, Ban: ban}}
}

// A source whose connections keep ending at once is banned: its live
// visitors are closed (reason banned), it is closed at accept with no byte
// and a sampled row, the ban is stored and told once. Another source is
// not. The ban expires.
func TestTCPBan_ShortConnectionsBanTheSource(t *testing.T) {
	st, clk := newFakeGuardStore(), &testClock{}
	r := newGuardRelay(t, 19650, st, clk, autoBan(3, time.Hour))

	long := greeted(t, "127.0.0.2", 19650)
	r.shortVisits(t, "127.0.0.2", 19650, 2)
	if n := len(r.sessionEvents(observe.TCPBanned)); n != 0 {
		t.Fatalf("banned after 2 short visits: %d events", n)
	}
	r.shortVisits(t, "127.0.0.2", 19650, 1)

	r.waitFor("the ban event", func() bool { return len(r.sessionEvents(observe.TCPBanned)) == 1 })
	ev := r.sessionEvents(observe.TCPBanned)[0]
	if ev.ServiceID != "svc-web" || ev.SourceIP != "127.0.0.2" || ev.OwnerID != "u1" || ev.Until.Sub(clk.now()) < 59*time.Minute {
		t.Fatalf("event %+v", ev)
	}
	// The live visitor of the source is closed.
	_ = long.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(long); err != nil {
		t.Fatalf("the long visitor was not closed: %v", err)
	}
	r.rowFor(t, observe.TrafficTCP, func(e ConnLogEntry) bool { return e.SourceIP == "127.0.0.2" && e.Reason == TCPRefusedBanned })
	r.waitFor("the stored ban", func() bool { _, ok := st.ban("svc-web", "127.0.0.2"); return ok })

	r.refusedAtOnce(t, "127.0.0.2", 19650)
	r.refusedAtOnce(t, "127.0.0.2", 19650)
	if rows := r.refusedRows(TCPRefusedBanned); len(rows) != 1 || rows[0].SourceIP != "127.0.0.2" {
		t.Fatalf("refused rows (sampled): %+v", rows)
	}
	if n := r.rejectedTraffic(TCPRefusedBanned); n != 2 {
		t.Fatalf("%d refusals told", n)
	}
	// Another source is not banned.
	greeted(t, "127.0.0.3", 19650)

	// The ban expires.
	clk.advance(61 * time.Minute)
	greeted(t, "127.0.0.2", 19650)
	if n := len(r.sessionEvents(observe.TCPBanned)); n != 1 {
		t.Fatalf("%d ban events", n)
	}
}

// Short connections older than the window do not count; a connection that
// lasted the short time or longer, or one the relay ended, never counts.
func TestTCPBan_OnlyShortConnectionsWithinTheWindowCount(t *testing.T) {
	clk := &testClock{}
	g := newTCPGuard(&Server{}, "svc", clk.now())
	g.apply(TCPLimits{AutoBan: TCPAutoBan{Enabled: true, Short: time.Second, Count: 3, Window: time.Minute, Ban: time.Hour}}, clk.now())
	end := func() bool {
		_, banned, _ := g.shortEnd("192.0.2.1", clk.now())
		return banned
	}
	first, second := end(), end()
	if first || second {
		t.Fatal("banned too early")
	}
	clk.advance(61 * time.Second)
	first, second = end(), end()
	if first || second {
		t.Fatal("ends outside the window counted")
	}
	if !end() {
		t.Fatal("three within the window did not ban")
	}
	if !g.banned("192.0.2.1", clk.now()) || g.banned("192.0.2.2", clk.now()) {
		t.Fatal("ban state")
	}
	clk.advance(time.Hour)
	if g.banned("192.0.2.1", clk.now()) {
		t.Fatal("the ban did not expire")
	}

	// Off: nothing counts.
	g.apply(TCPLimits{}, clk.now())
	for range 5 {
		if end() {
			t.Fatal("banned while off")
		}
	}
}

// The state is bounded: the sources watched and the bans per service.
func TestTCPBan_StateIsBounded(t *testing.T) {
	clk := &testClock{}
	g := newTCPGuard(&Server{}, "svc", clk.now())
	g.apply(TCPLimits{AutoBan: TCPAutoBan{Enabled: true, Short: time.Second, Count: 2, Window: time.Hour, Ban: time.Hour}}, clk.now())
	for i := range tcpBanTracked + 500 {
		g.shortEnd(fmt.Sprintf("10.%d.%d.1", i/256, i%256), clk.now())
		clk.advance(time.Millisecond)
	}
	if n := len(g.shorts); n > tcpBanTracked {
		t.Fatalf("%d sources watched", n)
	}
	for i := range MaxTCPBans {
		if !g.addBan(TCPBan{Source: fmt.Sprintf("b%d", i), Until: clk.now().Add(time.Hour)}, clk.now()) {
			t.Fatalf("ban %d refused", i)
		}
	}
	if g.addBan(TCPBan{Source: "one-more", Until: clk.now().Add(time.Hour)}, clk.now()) {
		t.Fatal("a ban beyond the maximum was kept")
	}
	if !g.addBan(TCPBan{Source: "b1", Until: clk.now().Add(2 * time.Hour)}, clk.now()) {
		t.Fatal("renewing a ban was refused")
	}
	g.shortEnd("198.51.100.9", clk.now())
	if _, banned, full := g.shortEnd("198.51.100.9", clk.now()); banned || !full {
		t.Fatalf("an automatic ban beyond the maximum: banned %v, full %v", banned, full)
	}
	clk.advance(2 * time.Hour)
	if !g.addBan(TCPBan{Source: "after-expiry", Until: clk.now().Add(time.Hour)}, clk.now()) {
		t.Fatal("expired bans were not made room for")
	}
}

// Forwards are never banned: short forward streams ban nobody, and a forward
// from a banned address goes through.
func TestTCPBan_ForwardsAreNeverBanned(t *testing.T) {
	r := newGuardRelay(t, 19651, newFakeGuardStore(), nil, autoBan(2, time.Hour))
	f := r.forwarder("secret")
	for range 4 {
		st, resp, err := f.open(t, "web")
		if err != nil || !resp.OK {
			t.Fatalf("forward: %+v %v", resp, err)
		}
		readN(t, st, 3)
		_ = st.Close()
	}
	r.waitFor("the forward rows", func() bool { return len(r.rows(observe.TrafficTCPForward)) == 4 })
	settle()
	if n := len(r.sessionEvents(observe.TCPBanned)); n != 0 {
		t.Fatalf("short forwards banned: %d", n)
	}
	// The forwarder's address banned by hand: visitors from it are refused,
	// its forward is not.
	r.srv.BanTCPSource("svc-web", "127.0.0.1", time.Now().Add(time.Hour), "manual")
	r.refusedAtOnce(t, "127.0.0.1", 19651)
	st, resp, err := f.open(t, "web")
	if err != nil || !resp.OK {
		t.Fatalf("forward from a banned address: %+v %v", resp, err)
	}
	if got := readN(t, st, 3); got != "hi " {
		t.Fatalf("forward got %q", got)
	}
}

// A ban by hand closes the source's live visitors and refuses it; an unban
// lets it in again.
func TestTCPBan_ManualBanAndUnban(t *testing.T) {
	r := newGuardRelay(t, 19652, newFakeGuardStore(), nil, TCPLimits{})
	live := greeted(t, "127.0.0.4", 19652)
	other := greeted(t, "127.0.0.5", 19652)
	r.srv.BanTCPSource("svc-web", "127.0.0.4", time.Now().Add(time.Hour), "manual")
	_ = live.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(live); err != nil {
		t.Fatalf("live visitor not closed: %v", err)
	}
	r.refusedAtOnce(t, "127.0.0.4", 19652)
	// The other source's visitor stays.
	if _, err := other.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, other, 1); got != "x" {
		t.Fatalf("other got %q", got)
	}
	r.srv.UnbanTCPSource("svc-web", "127.0.0.4")
	greeted(t, "127.0.0.4", 19652)
	// No guard on another relay's service: nothing happens.
	r.srv.BanTCPSource("svc-none", "127.0.0.4", time.Now().Add(time.Hour), "manual")
	r.srv.UnbanTCPSource("svc-none", "127.0.0.4")
}

// Bans are stored and read again by a relay that starts serving the
// service; an expired ban in the store is not.
func TestTCPBan_BansSurviveARestart(t *testing.T) {
	st := newFakeGuardStore()
	a := newGuardRelay(t, 19653, st, nil, autoBan(2, time.Hour))
	a.shortVisits(t, "127.0.0.6", 19653, 2)
	a.waitFor("the stored ban", func() bool { _, ok := st.ban("svc-web", "127.0.0.6"); return ok })
	_ = st.SaveTCPBan(context.Background(), "svc-web", TCPBan{Source: "127.0.0.7", Until: time.Now().Add(-time.Minute)})

	b := newGuardRelay(t, 19654, st, nil, TCPLimits{})
	b.refusedAtOnce(t, "127.0.0.6", 19654)
	greeted(t, "127.0.0.7", 19654)
	greeted(t, "127.0.0.8", 19654)
}

// A store that cannot be read refuses the registration: a ban that was not
// read would let its source in.
func TestTCPBan_AnUnreadableStoreRefusesTheRegistration(t *testing.T) {
	st := newFakeGuardStore()
	st.err = fmt.Errorf("db down")
	r := newForwardRelay(t, 19655, func(o *Options) { o.TCPGuards = st })
	rr := r.home.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"})
	if rr.OK || rr.Code != proto.CodeInternal {
		t.Fatalf("register: %+v", rr)
	}
	st.mu.Lock()
	st.err = nil
	st.mu.Unlock()
	if rr := r.home.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register again: %+v", rr)
	}
}

// echoFor writes n bytes and reads the n echoed, and reports how long it
// took.
func echoFor(t *testing.T, conn io.ReadWriter, n int) time.Duration {
	t.Helper()
	start := time.Now()
	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 8<<10)
		left := n
		for left > 0 {
			k := min(left, len(buf))
			if _, err := conn.Write(buf[:k]); err != nil {
				errc <- err
				return
			}
			left -= k
		}
		errc <- nil
	}()
	if _, err := io.ReadFull(conn, make([]byte, n)); err != nil {
		t.Fatalf("read the echo: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("write: %v", err)
	}
	return time.Since(start)
}

// within checks that n bytes in d are within 20 % of rate bytes a second.
func within(t *testing.T, what string, n int, d time.Duration, rate float64) {
	t.Helper()
	got := float64(n) / d.Seconds()
	if got < rate*0.8 || got > rate*1.2 {
		t.Fatalf("%s: %.0f bytes/s (%d in %v), want %.0f ± 20 %%", what, got, n, d, rate)
	}
}

// The bandwidth holds for one connection, is shared by several, applies to
// forwards as well, and goes with a refresh.
func TestTCPRate_TheBandwidthIsHeldAndShared(t *testing.T) {
	const kbps, rate = 800, 100_000.0 // 800 kbit/s: 100 000 bytes a second each way
	r := newGuardRelay(t, 19656, nil, nil, TCPLimits{RateLimitKbps: kbps})

	one := greeted(t, "127.0.0.1", 19656)
	within(t, "one visitor", 200_000, echoFor(t, one, 200_000), rate)

	a, b := greeted(t, "127.0.0.2", 19656), greeted(t, "127.0.0.3", 19656)
	start := time.Now()
	var wg sync.WaitGroup
	for _, c := range []net.Conn{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); echoFor(t, c, 100_000) }()
	}
	wg.Wait()
	within(t, "two visitors together", 200_000, time.Since(start), rate)

	f := r.forwarder("secret")
	st, resp, err := f.open(t, "web")
	if err != nil || !resp.OK {
		t.Fatalf("forward: %+v %v", resp, err)
	}
	readN(t, st, 3)
	within(t, "a forward", 150_000, echoFor(t, st, 150_000), rate)

	// The counters count what passed.
	live := r.srv.ServiceConnections("svc-web")
	var in uint64
	for _, c := range live {
		in += c.BytesIn
	}
	if in < 450_000 {
		t.Fatalf("live bytes in %d", in)
	}

	// Lifted: a megabyte passes in well under its time at the old rate.
	r.access.set("svc-web", TCPAccess{Mode: TCPAccessOpen})
	if err := r.srv.RefreshTCPService(context.Background(), "svc-web"); err != nil {
		t.Fatal(err)
	}
	if d := echoFor(t, one, 1_000_000); d > 3*time.Second {
		t.Fatalf("a megabyte took %v without a limit", d)
	}
}

func TestRateBucket(t *testing.T) {
	var b rateBucket
	now := time.Unix(1000, 0)
	if d := b.take(1<<20, now); d != 0 || b.chunk() != 0 {
		t.Fatal("an unset bucket limits")
	}
	b.set(10_000, now) // burst 2048 (a tenth is less)
	if b.chunk() != 2048 {
		t.Fatalf("chunk %d", b.chunk())
	}
	if d := b.take(2048, now); d != 0 {
		t.Fatalf("the burst waited %v", d)
	}
	if d := b.take(1000, now); d != 100*time.Millisecond {
		t.Fatalf("debt of 1000 at 10 000/s: %v", d)
	}
	if d := b.take(1000, now); d != 200*time.Millisecond {
		t.Fatalf("second debt: %v", d)
	}
	// Refilled after the debt is paid and the burst again.
	now = now.Add(time.Second)
	if d := b.take(2048, now); d != 0 {
		t.Fatalf("after a second: %v", d)
	}
	b.set(1_000_000, now)
	if b.chunk() != 100_000 {
		t.Fatalf("chunk at 1 MB/s: %d", b.chunk())
	}
	b.set(0, now)
	if d := b.take(1<<30, now); d != 0 {
		t.Fatal("lifted bucket limits")
	}
}

// writeEcho writes p and reads its echo.
func writeEcho(t *testing.T, conn net.Conn, n int) {
	t.Helper()
	if _, err := conn.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	readN(t, conn, n)
}

// The daily volume: 80 % is told once, the connection that carries the
// volume is closed with reason quota, new visitors and forwards are refused
// with reason quota, the usage is written and read again after a restart,
// and a new UTC day lets in again.
func TestTCPQuota_RefusesClosesWarnsOnceAndSurvivesARestart(t *testing.T) {
	st, clk := newFakeGuardStore(), &testClock{}
	l := TCPLimits{DailyBytes: 2000}
	r := newGuardRelay(t, 19657, st, clk, l)
	day := utcDay(clk.now())

	v := greeted(t, "127.0.0.1", 19657) // 3
	other := greeted(t, "127.0.0.2", 19657)
	writeEcho(t, v, 700) // 1403: 70 %
	settle()
	if n := len(r.sessionEvents(observe.TCPQuotaWarning)); n != 0 {
		t.Fatalf("warned at 70 %%: %d", n)
	}
	writeEcho(t, v, 100) // 1603
	r.waitFor("the warning", func() bool { return len(r.sessionEvents(observe.TCPQuotaWarning)) == 1 })
	w := r.sessionEvents(observe.TCPQuotaWarning)[0]
	if w.ServiceID != "svc-web" || w.Day != day || w.Limit != 2000 || w.Bytes < 1600 || w.OwnerID != "u1" {
		t.Fatalf("warning %+v", w)
	}
	writeEcho(t, v, 100) // 1803
	if _, err := v.Write(make([]byte, 300)); err != nil {
		t.Fatal(err)
	}
	// Both open connections are closed.
	for _, c := range []net.Conn{v, other} {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadAll(c); err != nil {
			t.Fatalf("not closed: %v", err)
		}
	}
	r.waitFor("the quota rows", func() bool {
		n := 0
		for _, e := range r.rows(observe.TrafficTCP) {
			if e.Reason == TCPRefusedQuota && e.Status != observe.OutcomeRejected {
				n++
			}
		}
		return n == 2
	})
	if !r.srv.TCPQuotaReached("svc-web") {
		t.Fatal("not reached")
	}
	r.refusedAtOnce(t, "127.0.0.3", 19657)
	if rows := r.refusedRows(TCPRefusedQuota); len(rows) != 1 {
		t.Fatalf("refused rows %+v", rows)
	}
	f := r.forwarder("secret")
	if _, resp, err := f.open(t, "web"); err != nil || resp.OK || resp.Code != proto.CodeRateLimited {
		t.Fatalf("forward over the volume: %+v %v", resp, err)
	}
	r.rowFor(t, observe.TrafficTCPForward, func(e ConnLogEntry) bool {
		return e.Status == observe.OutcomeRejected && e.Reason == TCPRefusedQuota
	})
	if n := len(r.sessionEvents(observe.TCPQuotaWarning)); n != 1 {
		t.Fatalf("%d warnings", n)
	}

	d, in, out, ok := r.srv.TCPUsageToday("svc-web")
	if !ok || d != day || in+out < 2000 {
		t.Fatalf("usage today %s %d+%d %v", d, in, out, ok)
	}
	r.srv.FlushTCPUsage(context.Background())
	if got := st.used("svc-web", day); got != in+out {
		t.Fatalf("stored %d, counted %d", got, in+out)
	}

	// Another relay reads the day: refused at once, and not warned again.
	clkB := &testClock{}
	b := newGuardRelay(t, 19658, st, clkB, l)
	b.refusedAtOnce(t, "127.0.0.4", 19658)
	settle()
	if n := len(b.sessionEvents(observe.TCPQuotaWarning)); n != 0 {
		t.Fatalf("warned again after the restart: %d", n)
	}

	// A new UTC day.
	clkB.advance(24 * time.Hour)
	nv := greeted(t, "127.0.0.5", 19658)
	if d, in, out, _ := b.srv.TCPUsageToday("svc-web"); d != utcDay(clkB.now()) || in+out > 10 {
		t.Fatalf("new day %s %d+%d", d, in, out)
	}
	// A limit set below what the day carried closes the open connection,
	// and lifting it lets in again.
	b.access.set("svc-web", TCPAccess{Mode: TCPAccessOpen, Limits: TCPLimits{DailyBytes: 2}})
	if err := b.srv.RefreshTCPService(context.Background(), "svc-web"); err != nil {
		t.Fatal(err)
	}
	_ = nv.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(nv); err != nil {
		t.Fatalf("not closed by the lower limit: %v", err)
	}
	b.access.set("svc-web", TCPAccess{Mode: TCPAccessOpen})
	if err := b.srv.RefreshTCPService(context.Background(), "svc-web"); err != nil {
		t.Fatal(err)
	}
	greeted(t, "127.0.0.6", 19658)
}

// The usage is written every interval; once no tunnel serves the service
// and everything is written, the relay lets go of it and the writer ends.
func TestTCPQuota_UsageIsWrittenAndTheGuardLetGo(t *testing.T) {
	st := newFakeGuardStore()
	r := newForwardRelay(t, 19659, func(o *Options) { o.TCPGuards = st; o.usageFlush = 50 * time.Millisecond })
	if rr := r.home.register(t, proto.TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", Access: "open"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	day := utcDay(time.Now())
	v := greeted(t, "127.0.0.1", 19659)
	writeEcho(t, v, 1000)
	r.waitFor("the usage written", func() bool { return st.used("svc-web", day) >= 2003 })
	_ = v.Close()
	r.home.close()
	r.waitFor("the guard let go", func() bool {
		r.srv.guardMu.Lock()
		defer r.srv.guardMu.Unlock()
		return len(r.srv.guards) == 0 && !r.srv.guardFlushing
	})
	if got := st.used("svc-web", day); got != 2003 {
		t.Fatalf("stored %d", got)
	}
}
