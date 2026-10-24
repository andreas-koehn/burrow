package server

// tcp_guard.go — what a tcp service is protected and bounded by beyond its
// access mode (tcp complete, E6 and E7):
//
//   - automatic bans: a source whose visitor connections keep ending at
//     once (at least Count that last less than Short within Window) is
//     banned for Ban. A banned source is closed at accept with no byte, on
//     the port listener and on the TLS passthrough listener alike, before
//     the home client is told. A source is an IPv4 address or an IPv6 /64
//     (sourceKey). Forward streams are never banned: they are signed in.
//     A source an allow entry of mode ip or a live grant lets in is trusted
//     and not counted. A ban is never shortened (only lifted).
//   - bandwidth: one token bucket per direction, shared by every connection
//     of the service, visitors and forwards. It sits in the pipe, under the
//     byte counters, so what they count is what passed.
//   - daily volume: the bytes in and out of a UTC day. Over the limit new
//     visitors are refused (reason quota) and the open ones are closed
//     (reason quota); forwards are counted and never refused or closed by
//     it (they are authorized). At 80 % the observer is told once a day.
//     Usage rows older than 90 days are pruned once a day.
//
// The state of a service lives in one tcpGuard per service, kept by the
// Server while a tunnel serves it (and until what it counted is written).
// Bans are written through to the store as they are made; the bytes are
// written once a minute. Both are read again when a tunnel of the service
// starts on a relay that does not hold the guard, so they survive a restart.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ankoehn/burrow/internal/observe"
)

// The reasons of a refused or closed tcp connection of E6 and E7.
const (
	// TCPRefusedBanned: the source is banned from the service. It is also
	// the reason of a live visitor closed when its source was banned.
	TCPRefusedBanned = "banned"
	// TCPRefusedQuota: the service has carried its daily volume. It is also
	// the reason of an open visitor closed when the volume was reached
	// (forwards are counted, never closed or refused by it).
	TCPRefusedQuota = "quota"
)

// The bounds of the settings of E6 and E7. The relay applies any value it is
// given (a test uses small ones); the store keeps to these.
const (
	MinTCPRateLimitKbps = 64
	MaxTCPRateLimitKbps = 10_000_000 // 10 Gbit/s
	MinTCPDailyBytes    = int64(1) << 20
	MaxTCPDailyBytes    = int64(1) << 50

	MinTCPAutoBanShort  = time.Second
	MaxTCPAutoBanShort  = 5 * time.Minute
	MinTCPAutoBanCount  = 2
	MaxTCPAutoBanCount  = 100
	MinTCPAutoBanWindow = time.Minute
	MaxTCPAutoBanWindow = 24 * time.Hour
	MinTCPAutoBanBan    = time.Minute
	MaxTCPAutoBanBan    = 30 * 24 * time.Hour
)

// The defaults of an automatic ban (spec E6).
const (
	DefaultTCPAutoBanShort  = 3 * time.Second
	DefaultTCPAutoBanCount  = 10
	DefaultTCPAutoBanWindow = 10 * time.Minute
	DefaultTCPAutoBanBan    = time.Hour
)

const (
	// tcpBanTracked bounds the sources one service watches for short
	// connections. When it is full the source seen least recently is
	// forgotten.
	tcpBanTracked = 4096
	// MaxTCPBans bounds the active bans of one service. An automatic ban
	// beyond it is not made (and logged); the API refuses a manual one.
	MaxTCPBans = 4096
	// defaultUsageFlush is how often the bytes counted are written.
	defaultUsageFlush = time.Minute
	// tcpQuotaWarnPercent is the share of the daily volume that is warned
	// of.
	tcpQuotaWarnPercent = 80
	// guardLoadTimeout bounds the read of a service's bans and usage.
	guardLoadTimeout = 5 * time.Second
)

// TCPAutoBan are the automatic ban settings of a tcp service.
type TCPAutoBan struct {
	Enabled bool
	// A connection that lasts less than Short is short; Count short ones
	// within Window ban their source for Ban.
	Short  time.Duration
	Count  int
	Window time.Duration
	Ban    time.Duration
}

// TCPLimits are the settings of E6 and E7 of a tcp service, read with its
// access (Resolved, TCPAccess).
type TCPLimits struct {
	// RateLimitKbps is the bandwidth of each direction, shared by every
	// connection of the service, in kilobits (1000 bits) a second; 0 none.
	RateLimitKbps int
	// DailyBytes is the volume in and out a UTC day may carry; 0 none.
	DailyBytes int64
	AutoBan    TCPAutoBan
}

// TCPBan is a source banned from a tcp service until Until.
type TCPBan struct {
	Source    string // an IPv4 address or an IPv6 /64 (as sourceKey writes it)
	Until     time.Time
	Reason    string
	CreatedAt time.Time
}

// TCPBanReasonAuto is the reason of a ban the relay made itself.
const TCPBanReasonAuto = "auto"

// TCPGuardStore keeps the bans and the daily usage of tcp services. The
// relay reads them when a tunnel of a service starts (and the relay does not
// hold them yet), writes a ban as it makes one, and adds the bytes counted
// once a minute.
type TCPGuardStore interface {
	// ActiveTCPBans are the bans of the service that have not expired at
	// now.
	ActiveTCPBans(ctx context.Context, serviceID string, now time.Time) ([]TCPBan, error)
	// SaveTCPBan adds the ban, or replaces the one of the same source.
	SaveTCPBan(ctx context.Context, serviceID string, b TCPBan) error
	// TCPUsage is what the service carried on day (YYYY-MM-DD, UTC).
	TCPUsage(ctx context.Context, serviceID, day string) (in, out uint64, err error)
	// AddTCPUsage adds in and out to the service's day.
	AddTCPUsage(ctx context.Context, serviceID, day string, in, out uint64) error
	// PruneTCPBans deletes the bans of the service that expired at or
	// before now.
	PruneTCPBans(ctx context.Context, serviceID string, now time.Time) error
}

// ErrTCPServiceGone is what AddTCPUsage answers (wrapped) when the service is
// no longer in the store: the bytes are dropped, not tried again.
var ErrTCPServiceGone = errors.New("server: the tcp service is gone")

// TCPUsagePruner is a TCPGuardStore that can delete old usage rows. The
// relay calls it once per UTC day while it holds tcp services.
type TCPUsagePruner interface {
	// PruneTCPUsage deletes the usage of every service for the days before
	// before (YYYY-MM-DD).
	PruneTCPUsage(ctx context.Context, before string) error
}

// tcpUsageKeep is how long tcp_daily_usage rows are kept.
const tcpUsageKeepDays = 90

// utcDay is the UTC day of t as tcp_daily_usage keeps it.
func utcDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// nextUTCDay is the start of the UTC day after t.
func nextUTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Bandwidth
// ---------------------------------------------------------------------------

// rateBucket is a token bucket of bytes a second, shared by the connections
// that take from it. A take may go into debt: it waits for its own share,
// so the bucket's rate holds however many connections take at once.
type rateBucket struct {
	mu     sync.Mutex
	rate   float64 // bytes a second; 0 is no limit
	burst  float64
	tokens float64
	last   time.Time
}

// set changes the rate; 0 lifts the limit. The burst is a tenth of a second,
// at least 2 KiB, which is also the most one take moves at once.
func (b *rateBucket) set(bytesPerSecond float64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytesPerSecond == b.rate {
		return
	}
	b.rate = max(bytesPerSecond, 0)
	b.burst = max(b.rate/10, 2048)
	if b.last.IsZero() || b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
}

// chunk is the most bytes one take should ask for; 0 without a limit.
func (b *rateBucket) chunk() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate == 0 {
		return 0
	}
	return int(b.burst)
}

// take takes n bytes and says how long the taker must wait before it moves
// them.
func (b *rateBucket) take(n int, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rate == 0 || n <= 0 {
		return 0
	}
	if el := now.Sub(b.last); el > 0 {
		b.tokens = min(b.tokens+el.Seconds()*b.rate, b.burst)
	}
	b.last = now
	b.tokens -= float64(n)
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// guardStream is the home side of a tcp connection as the guard sees it:
// what is written to it (visitor or forwarder → home client) and what is
// read from it pass the service's buckets and are counted against its day.
// It closes nothing itself; the pipe does.
type guardStream struct {
	net.Conn
	g    *tcpGuard
	c    *liveConn
	done chan struct{}
	once sync.Once
}

func newGuardStream(conn net.Conn, g *tcpGuard, c *liveConn) *guardStream {
	g.live.Add(1)
	return &guardStream{Conn: conn, g: g, c: c, done: make(chan struct{})}
}

// interrupt wakes a take that waits for its share of the bandwidth.
func (s *guardStream) interrupt() { s.once.Do(func() { close(s.done) }) }

// release ends what the stream holds of the guard; call it once, after the
// pipe returned.
func (s *guardStream) release() {
	s.interrupt()
	s.g.live.Add(-1)
}

func (s *guardStream) Close() error {
	s.interrupt()
	return s.Conn.Close()
}

func (s *guardStream) wait(d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if s.c != nil {
		// Waiting for the bucket is not idle time (sweepIdle).
		s.c.throttled.Add(1)
		defer s.c.throttled.Add(-1)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-s.done:
		return net.ErrClosed
	}
}

func (s *guardStream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := len(p)
		if ch := s.g.in.chunk(); ch > 0 && n > ch {
			n = ch
		}
		if err := s.wait(s.g.in.take(n, time.Now())); err != nil {
			return written, err
		}
		m, err := s.Conn.Write(p[:n])
		written += m
		if m > 0 {
			s.g.srv.countGuard(s.g, s.c, uint64(m), 0)
		}
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

func (s *guardStream) Read(p []byte) (int, error) {
	if ch := s.g.out.chunk(); ch > 0 && len(p) > ch {
		p = p[:ch]
	}
	n, err := s.Conn.Read(p)
	if n > 0 {
		s.g.srv.countGuard(s.g, s.c, 0, uint64(n))
		if werr := s.wait(s.g.out.take(n, time.Now())); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}

// ---------------------------------------------------------------------------
// The guard of one service
// ---------------------------------------------------------------------------

// tcpGuard is the state of E6 and E7 of one tcp service.
type tcpGuard struct {
	srv       *Server
	serviceID string

	in, out rateBucket

	// The daily volume: limit (0 none), what the day carried as stored
	// (base) and what was counted since (pend). over: the day carried its
	// limit; warned: the observer was told of 80 %. dayEnd is when day
	// ends (unix nanoseconds).
	limit           atomic.Int64
	baseIn, baseOut atomic.Uint64
	pendIn, pendOut atomic.Uint64
	over, warned    atomic.Bool
	dayEnd          atomic.Int64
	live            atomic.Int32 // guardStreams not released
	mu              sync.Mutex   // what follows
	day             string
	carries         []usageCarry // counted for a day that has ended, not written yet
	applied         bool
	autoban         TCPAutoBan
	bans            map[string]TCPBan
	shorts          map[string]*shortEnds
	// banStore orders the write of an automatic ban with an unban: a ban
	// is written only while the guard still holds it, and an unban waits
	// for a write under way, so a ban lifted before it was written is not
	// written after.
	banStore sync.Mutex
}

// subU64 takes n from a.
func subU64(a *atomic.Uint64, n uint64) { a.Add(^(n - 1)) }

// usageCarry is what a day counted that is still to be written.
type usageCarry struct {
	day     string
	in, out uint64
}

// shortEnds are the ends of the short connections of one source, oldest
// first, at most the ban count of them.
type shortEnds struct {
	at   []time.Time
	last time.Time
}

func newTCPGuard(s *Server, serviceID string, now time.Time) *tcpGuard {
	g := &tcpGuard{srv: s, serviceID: serviceID, day: utcDay(now), bans: map[string]TCPBan{}, shorts: map[string]*shortEnds{}}
	g.dayEnd.Store(nextUTCDay(now).UnixNano())
	return g
}

// used is what the day carried so far.
func (g *tcpGuard) used() uint64 {
	return g.baseIn.Load() + g.baseOut.Load() + g.pendIn.Load() + g.pendOut.Load()
}

// apply takes the service's settings as they are now. A daily volume that
// the day has carried already refuses from now on and closes the open
// connections; one that it has not carried lets in again.
func (g *tcpGuard) apply(l TCPLimits, now time.Time) {
	g.in.set(float64(l.RateLimitKbps)*125, now)
	g.out.set(float64(l.RateLimitKbps)*125, now)
	g.mu.Lock()
	g.rolloverLocked(now)
	first := !g.applied
	g.applied = true
	g.autoban = l.AutoBan
	if !l.AutoBan.Enabled {
		clear(g.shorts)
	}
	g.limit.Store(max(l.DailyBytes, 0))
	used := g.used()
	lim := uint64(max(l.DailyBytes, 0))
	if first && lim > 0 && used*100 >= lim*tcpQuotaWarnPercent {
		// The day was warned of before the relay read it (a restart).
		g.warned.Store(true)
	}
	reached := lim > 0 && used >= lim
	was := g.over.Swap(reached)
	g.mu.Unlock()
	if reached && !was {
		g.srv.quotaReached(g)
	}
	if !first && lim > 0 && !reached && used*100 >= lim*tcpQuotaWarnPercent && g.warned.CompareAndSwap(false, true) {
		g.srv.quotaWarning(g, used, lim)
	}
}

// admitQuota reports whether the service may take a new connection: its
// day has not carried its volume.
func (g *tcpGuard) admitQuota(now time.Time) bool {
	if now.UnixNano() >= g.dayEnd.Load() {
		g.mu.Lock()
		g.rolloverLocked(now)
		g.mu.Unlock()
	}
	return !g.over.Load()
}

// rolloverLocked starts the day of now when it is a new one: what the old
// day counted is kept to be written, and the new day starts empty.
func (g *tcpGuard) rolloverLocked(now time.Time) {
	d := utcDay(now)
	if d == g.day {
		return
	}
	pi, po := g.pendIn.Swap(0), g.pendOut.Swap(0)
	if pi+po > 0 {
		g.carries = append(g.carries, usageCarry{day: g.day, in: pi, out: po})
	}
	g.baseIn.Store(0)
	g.baseOut.Store(0)
	g.day = d
	g.dayEnd.Store(nextUTCDay(now).UnixNano())
	g.warned.Store(false)
	g.over.Store(false)
}

// banned reports whether source is banned at now; an expired ban is
// forgotten.
func (g *tcpGuard) banned(source string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.bans[source]
	if !ok {
		return false
	}
	if !now.Before(b.Until) {
		delete(g.bans, source)
		return false
	}
	return true
}

// addBan bans the source; false when the service has MaxTCPBans active bans
// and the source has none.
func (g *tcpGuard) addBan(b TCPBan, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.addBanLocked(b, now)
}

func (g *tcpGuard) addBanLocked(b TCPBan, now time.Time) bool {
	cur, ok := g.bans[b.Source]
	if ok && now.Before(cur.Until) && cur.Until.After(b.Until) {
		// A ban is never shortened: the one that ends later holds.
		delete(g.shorts, b.Source)
		return true
	}
	if !ok && len(g.bans) >= MaxTCPBans {
		g.expireLocked(now)
		if len(g.bans) >= MaxTCPBans {
			return false
		}
	}
	g.bans[b.Source] = b
	delete(g.shorts, b.Source)
	return true
}

func (g *tcpGuard) removeBan(source string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.bans[source]
	delete(g.bans, source)
	delete(g.shorts, source)
	return ok
}

func (g *tcpGuard) expireLocked(now time.Time) {
	for k, b := range g.bans {
		if !now.Before(b.Until) {
			delete(g.bans, k)
		}
	}
}

// shortEnd counts a visitor connection of source that lasted less than the
// short time, at now. When it is the one that makes the count within the
// window, the source is banned and the ban returned; full says that it would
// have been and the service has its maximum of bans.
func (g *tcpGuard) shortEnd(source string, now time.Time) (b TCPBan, banned, full bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ab := g.autoban
	if !ab.Enabled || ab.Count < 1 {
		return TCPBan{}, false, false
	}
	if cur, ok := g.bans[source]; ok && now.Before(cur.Until) {
		return TCPBan{}, false, false
	}
	se, ok := g.shorts[source]
	if !ok {
		if len(g.shorts) >= tcpBanTracked {
			g.forgetShortsLocked(now, ab.Window)
		}
		se = &shortEnds{}
		g.shorts[source] = se
	}
	keep := se.at[:0]
	for _, t := range se.at {
		if now.Sub(t) < ab.Window {
			keep = append(keep, t)
		}
	}
	se.at = append(keep, now)
	if len(se.at) > ab.Count {
		se.at = se.at[len(se.at)-ab.Count:]
	}
	se.last = now
	if len(se.at) < ab.Count {
		return TCPBan{}, false, false
	}
	b = TCPBan{
		Source: source, Until: now.Add(ab.Ban), CreatedAt: now,
		Reason: fmt.Sprintf("%s: %d connections shorter than %s within %s", TCPBanReasonAuto, ab.Count, ab.Short, ab.Window),
	}
	if !g.addBanLocked(b, now) {
		delete(g.shorts, source)
		return TCPBan{}, false, true
	}
	return b, true, false
}

// forgetShortsLocked makes room in the table of short connections: the
// sources with none left in the window go, and when that is not enough the
// one seen least recently.
func (g *tcpGuard) forgetShortsLocked(now time.Time, window time.Duration) {
	var oldest string
	var oldestAt time.Time
	for k, se := range g.shorts {
		if now.Sub(se.last) >= window {
			delete(g.shorts, k)
			continue
		}
		if oldest == "" || se.last.Before(oldestAt) {
			oldest, oldestAt = k, se.last
		}
	}
	if len(g.shorts) >= tcpBanTracked && oldest != "" {
		delete(g.shorts, oldest)
	}
}

// ---------------------------------------------------------------------------
// The Server's side
// ---------------------------------------------------------------------------

// guardFor returns the guard of a service, reading its bans and the usage of
// its day from the store when the relay does not hold it yet. An error of
// the store refuses the registration: a ban that was not read would let its
// source in.
//
// The store is read without guardMu, so a slow one holds up only this
// service; two loads of one service at once keep the guard put first.
func (s *Server) guardFor(serviceID string) (*tcpGuard, error) {
	if g := s.guard(serviceID); g != nil {
		return g, nil
	}
	now := s.now()
	g := newTCPGuard(s, serviceID, now)
	if st := s.opts.TCPGuards; st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), guardLoadTimeout)
		defer cancel()
		bans, err := st.ActiveTCPBans(ctx, serviceID, now)
		if err != nil {
			return nil, fmt.Errorf("tcp bans: %w", err)
		}
		in, out, err := st.TCPUsage(ctx, serviceID, g.day)
		if err != nil {
			return nil, fmt.Errorf("tcp usage: %w", err)
		}
		for _, b := range bans {
			if now.Before(b.Until) {
				g.bans[b.Source] = b
			}
		}
		g.baseIn.Store(in)
		g.baseOut.Store(out)
	}
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	if have, ok := s.guards[serviceID]; ok {
		return have, nil
	}
	if s.guards == nil {
		s.guards = map[string]*tcpGuard{}
	}
	s.guards[serviceID] = g
	if !s.guardFlushing {
		s.guardFlushing = true
		go s.flushGuards()
	}
	return g, nil
}

// guard is the guard the relay holds for a service, nil when none.
func (s *Server) guard(serviceID string) *tcpGuard {
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	return s.guards[serviceID]
}

// countGuard counts bytes of a connection c against its service's day. The
// call that reaches 80 % tells the observer; one at or over the volume closes
// c, and the first closes every visitor of the service. A forward is counted
// and never closed: it is authorized (the owner, or a role the policy
// names), and anonymous visitors must not lock it out.
func (s *Server) countGuard(g *tcpGuard, c *liveConn, in, out uint64) {
	if in > 0 {
		g.pendIn.Add(in)
	}
	if out > 0 {
		g.pendOut.Add(out)
	}
	lim := g.limit.Load()
	if lim <= 0 {
		return
	}
	used, ulim := g.used(), uint64(lim)
	if used*100 >= ulim*tcpQuotaWarnPercent && !g.warned.Load() && g.warned.CompareAndSwap(false, true) {
		s.quotaWarning(g, used, ulim)
	}
	if used < ulim {
		return
	}
	if g.over.CompareAndSwap(false, true) {
		s.quotaReached(g)
	}
	if c != nil && c.kind != ConnKindForward {
		c.closeWith(TCPRefusedQuota)
	}
}

// quotaReached closes every live visitor of the service: its day carried
// its volume. Forwards stay (see countGuard).
func (s *Server) quotaReached(g *tcpGuard) {
	s.log.Info("tcp service reached its daily volume", "service_id", g.serviceID, "bytes", g.used(), "limit", g.limit.Load())
	for _, tn := range s.tunnelsOfService(g.serviceID) {
		for _, c := range tn.liveConns() {
			if c.kind != ConnKindForward {
				c.closeWith(TCPRefusedQuota)
			}
		}
	}
}

// quotaWarning tells the observer that the service carried 80 % of its day.
func (s *Server) quotaWarning(g *tcpGuard, used, limit uint64) {
	g.mu.Lock()
	day := g.day
	g.mu.Unlock()
	owner := ""
	if tn, ok := s.LookupTunnelByServiceID(g.serviceID); ok {
		owner = tn.sess.UserID
	}
	s.log.Info("tcp service used most of its daily volume", "service_id", g.serviceID, "bytes", used, "limit", limit, "day", day)
	s.reg.observer().ObserveSession(observe.Session{
		Kind: observe.TCPQuotaWarning, ServiceID: g.serviceID, UserID: owner, OwnerID: owner, TunnelType: "tcp",
		Day: day, Bytes: int64(used), Limit: int64(limit),
	})
}

// shortVisit counts a visitor connection that ended by itself; one that
// lasted less than the short time may ban its source.
func (s *Server) shortVisit(tn *Tunnel, source string, lasted time.Duration) {
	g := tn.guard
	if g == nil || source == "" {
		return
	}
	g.mu.Lock()
	ab := g.autoban
	g.mu.Unlock()
	if !ab.Enabled || lasted >= ab.Short {
		return
	}
	b, ok, full := g.shortEnd(source, s.now())
	if full {
		s.log.Warn("tcp service has its maximum of bans; a source is not banned", "service_id", tn.ServiceID, "max", MaxTCPBans)
	}
	if !ok {
		return
	}
	s.log.Info("tcp source banned", "service_id", tn.ServiceID, "source", source, "until", b.Until.UTC().Format(time.RFC3339), "reason", b.Reason)
	s.closeBanned(tn, source)
	go s.persistBan(tn.ServiceID, g, b)
	s.reg.observer().ObserveSession(observe.Session{
		Kind: observe.TCPBanned, ServiceID: tn.ServiceID, TunnelID: tn.ID, TunnelType: "tcp",
		UserID: tn.sess.UserID, OwnerID: tn.sess.UserID, SessionID: tn.sess.SessionID,
		SourceIP: source, Until: b.Until, Reason: b.Reason,
	})
}

// persistBan writes the automatic ban b to the store, unless the guard no
// longer holds it (it was lifted or replaced meanwhile).
func (s *Server) persistBan(serviceID string, g *tcpGuard, b TCPBan) {
	st := s.opts.TCPGuards
	if st == nil {
		return
	}
	g.banStore.Lock()
	defer g.banStore.Unlock()
	g.mu.Lock()
	cur, ok := g.bans[b.Source]
	g.mu.Unlock()
	if !ok || cur != b {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), guardLoadTimeout)
	defer cancel()
	if err := st.SaveTCPBan(ctx, serviceID, b); err != nil {
		s.log.Warn("persist tcp ban failed; it holds until the relay restarts", "service_id", serviceID, "source", b.Source, "err", err)
	}
}

// closeBanned closes the live visitors of the tunnel that come from source
// (never a forward).
func (s *Server) closeBanned(tn *Tunnel, source string) {
	for _, c := range tn.liveConns() {
		if c.kind != ConnKindVisitor {
			continue
		}
		if a, err := netip.ParseAddr(c.sourceIP); err == nil && sourceKey(a.Unmap().WithZone("")) == source {
			c.closeWith(TCPRefusedBanned)
		}
	}
}

// BanTCPSource bans source (an address, or an IPv6 /64, as
// NormalizeTCPBanSource writes it) from the tcp service until until, on the
// relay that serves it, and closes its live visitors. The API has written
// the ban already; a relay that does not hold the service reads it when a
// tunnel of the service starts.
func (s *Server) BanTCPSource(serviceID, source string, until time.Time, reason string) {
	g := s.guard(serviceID)
	if g == nil {
		return
	}
	now := s.now()
	if !g.addBan(TCPBan{Source: source, Until: until, Reason: reason, CreatedAt: now}, now) {
		s.log.Warn("tcp service has its maximum of bans in memory; the ban applies once the relay reads it again", "service_id", serviceID)
		return
	}
	for _, tn := range s.tunnelsOfService(serviceID) {
		s.closeBanned(tn, source)
	}
}

// UnbanTCPSource lifts a ban on the relay that holds the service, and
// forgets the source's short connections; it reports whether the relay held
// the ban. A write of the ban under way ends first, so a caller that deletes
// the stored ban after this deletes that write too, and a later write is not
// made.
func (s *Server) UnbanTCPSource(serviceID, source string) bool {
	g := s.guard(serviceID)
	if g == nil {
		return false
	}
	g.banStore.Lock()
	defer g.banStore.Unlock()
	return g.removeBan(source)
}

// TCPBans are the active bans the relay holds for the service, the first to
// expire first; nil when it does not hold the service. They include bans
// the store does not have (a write that failed).
func (s *Server) TCPBans(serviceID string) []TCPBan {
	g := s.guard(serviceID)
	if g == nil {
		return nil
	}
	now := s.now()
	g.mu.Lock()
	out := make([]TCPBan, 0, len(g.bans))
	for _, b := range g.bans {
		if now.Before(b.Until) {
			out = append(out, b)
		}
	}
	g.mu.Unlock()
	slices.SortFunc(out, func(a, b TCPBan) int {
		return cmp.Or(a.Until.Compare(b.Until), cmp.Compare(a.Source, b.Source))
	})
	return out
}

// TCPUsageToday is what the tcp service carried on the UTC day day as the
// relay counts it, written or not; ok is false when the relay does not hold
// the service (the store has it then).
func (s *Server) TCPUsageToday(serviceID string) (day string, in, out uint64, ok bool) {
	g := s.guard(serviceID)
	if g == nil {
		return "", 0, 0, false
	}
	now := s.now()
	g.mu.Lock()
	g.rolloverLocked(now)
	day = g.day
	g.mu.Unlock()
	return day, g.baseIn.Load() + g.pendIn.Load(), g.baseOut.Load() + g.pendOut.Load(), true
}

// TCPQuotaReached reports whether the relay refuses the tcp service's new
// connections because its day carried its volume.
func (s *Server) TCPQuotaReached(serviceID string) bool {
	g := s.guard(serviceID)
	return g != nil && !g.admitQuota(s.now())
}

// flushGuards writes what the guards counted every usageFlush, starts a new
// day when one begins, and lets go of the guards of services no tunnel serves
// any more once all they counted is written. It ends when there are none.
func (s *Server) flushGuards() {
	t := time.NewTicker(s.usageFlush)
	defer t.Stop()
	for range t.C {
		s.guardMu.Lock()
		gs := make([]*tcpGuard, 0, len(s.guards))
		for _, g := range s.guards {
			gs = append(gs, g)
		}
		s.guardMu.Unlock()
		for _, g := range gs {
			ctx, cancel := context.WithTimeout(context.Background(), guardLoadTimeout)
			s.flushGuard(ctx, g)
			cancel()
		}
		s.pruneUsage(s.now())
		s.guardMu.Lock()
		for id, g := range s.guards {
			if !s.servingTCP(id) && g.live.Load() == 0 && g.idle() {
				delete(s.guards, id)
			}
		}
		if len(s.guards) == 0 {
			s.guardFlushing = false
			s.guardMu.Unlock()
			return
		}
		s.guardMu.Unlock()
	}
}

// pruneUsage deletes the stored usage older than tcpUsageKeepDays, once per
// UTC day of now, when the store can.
func (s *Server) pruneUsage(now time.Time) {
	p, ok := s.opts.TCPGuards.(TCPUsagePruner)
	if !ok {
		return
	}
	day := utcDay(now)
	if s.usagePrunedDay == day {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), guardLoadTimeout)
	defer cancel()
	if err := p.PruneTCPUsage(ctx, utcDay(now.UTC().AddDate(0, 0, -tcpUsageKeepDays))); err != nil {
		s.log.Warn("prune tcp usage failed", "err", err)
		return
	}
	s.usagePrunedDay = day
}

// idle reports whether everything the guard counted is written.
func (g *tcpGuard) idle() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.carries) == 0 && g.pendIn.Load() == 0 && g.pendOut.Load() == 0
}

// flushGuard writes what g counted and has not written, and prunes the
// service's expired bans from the store. Without a store it only moves the
// count into what is kept for the day. What cannot be written is kept for
// the next time; a write whose context ended while it ran may have been
// made, so it is not tried again (the count may then miss up to a minute of
// bytes, never count them twice).
func (s *Server) flushGuard(ctx context.Context, g *tcpGuard) {
	now := s.now()
	g.mu.Lock()
	g.rolloverLocked(now)
	g.expireLocked(now)
	carries := g.carries
	g.carries = nil
	day := g.day
	pi, po := g.pendIn.Load(), g.pendOut.Load()
	// The count moves to base before it is written: the day's total never
	// dips meanwhile.
	g.baseIn.Add(pi)
	subU64(&g.pendIn, pi)
	g.baseOut.Add(po)
	subU64(&g.pendOut, po)
	g.mu.Unlock()
	st := s.opts.TCPGuards
	if st == nil {
		return
	}
	if pi+po > 0 {
		carries = append(carries, usageCarry{day: day, in: pi, out: po})
	}
	var failed []usageCarry
	for _, c := range carries {
		if !s.addUsage(ctx, st, g.serviceID, c) {
			failed = append(failed, c)
		}
	}
	if ctx.Err() == nil {
		if err := st.PruneTCPBans(ctx, g.serviceID, now); err != nil {
			s.log.Warn("prune tcp bans failed", "service_id", g.serviceID, "err", err)
		}
	}
	if len(failed) == 0 {
		return
	}
	g.mu.Lock()
	for _, c := range failed {
		if c.day == g.day {
			// Not written: it is pending again, not stored.
			subU64(&g.baseIn, c.in)
			g.pendIn.Add(c.in)
			subU64(&g.baseOut, c.out)
			g.pendOut.Add(c.out)
			continue
		}
		g.carries = append(g.carries, c)
	}
	g.mu.Unlock()
}

// addUsage writes c; false when it was not written and is to be tried
// again. One whose context ended while it ran is not: it may have been
// written.
func (s *Server) addUsage(ctx context.Context, st TCPGuardStore, serviceID string, c usageCarry) bool {
	if ctx.Err() != nil {
		return false
	}
	err := st.AddTCPUsage(ctx, serviceID, c.day, c.in, c.out)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrTCPServiceGone):
		s.log.Debug("tcp service gone; its usage is dropped", "service_id", serviceID, "day", c.day, "bytes", c.in+c.out)
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		s.log.Warn("write tcp usage timed out; it may not have been written and is not tried again",
			"service_id", serviceID, "day", c.day, "bytes", c.in+c.out, "err", err)
		return true
	default:
		s.log.Warn("write tcp usage failed; trying again", "service_id", serviceID, "day", c.day, "err", err)
		return false
	}
}

// FlushTCPUsage writes what every guard counted and has not written yet.
// The relay's shutdown calls it after the sessions ended.
func (s *Server) FlushTCPUsage(ctx context.Context) {
	s.guardMu.Lock()
	gs := make([]*tcpGuard, 0, len(s.guards))
	for _, g := range s.guards {
		gs = append(gs, g)
	}
	s.guardMu.Unlock()
	for _, g := range gs {
		s.flushGuard(ctx, g)
	}
}
