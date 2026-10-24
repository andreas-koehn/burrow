package server

// tcp_access.go — who may connect to the public port of a tcp service (native
// tcp, P02): the access modes, the compiled ip policy of mode ip, and the
// limits every accepted visitor of modes open and ip passes before the home
// client is told of it.
//
// A visitor is judged by the address its connection comes from
// (RemoteAddr), never by anything it sends. The one exception is the TLS
// passthrough listener (passthrough.go): there a PROXY protocol v2 header of
// a trusted proxy (BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES, never
// BURROW_TRUSTED_PROXIES) names the visitor. When
// the relay runs in Docker with the userland proxy (published IPv6 ports to
// an IPv4 container, some hairpin cases) that address is the bridge gateway
// (172.17.0.1 or similar), not the visitor's: an allow entry for the gateway
// lets everyone in. The relay logs and records the address it saw, so an
// operator can tell.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The access modes of a tcp service. Rows that an older client created are
// open.
const (
	TCPAccessOpen    = "open"
	TCPAccessIP      = "ip"
	TCPAccessPrivate = "private"
)

// ValidTCPAccessMode reports whether mode is an access mode of a tcp service.
func ValidTCPAccessMode(mode string) bool {
	switch mode {
	case TCPAccessOpen, TCPAccessIP, TCPAccessPrivate:
		return true
	}
	return false
}

// tcpAccessRank orders the tcp access modes from the most open: open < ip <
// private.
func tcpAccessRank(mode string) int {
	switch mode {
	case TCPAccessIP:
		return 1
	case TCPAccessPrivate:
		return 2
	}
	return 0
}

// Limits of the visitors of one tcp tunnel (modes open and ip).
const (
	// DefaultTCPMaxConnections is how many visitors may be connected at once
	// when the service sets no number; MaxTCPMaxConnections is the most a
	// service may set.
	DefaultTCPMaxConnections = 32
	MaxTCPMaxConnections     = 256
	// tcpMaxPending is how many visitors may wait for the home client's
	// stream at once, and tcpPairTimeout how long one waits.
	tcpMaxPending  = 16
	tcpPairTimeout = 5 * time.Second
	// Per source (an IPv4 address, or the /64 of an IPv6 one): a bucket of
	// tcpSourceBurst new connections that refills by tcpSourcePerMinute a
	// minute, and at most tcpSourceConcurrent connected at once.
	tcpSourcePerMinute  = 10
	tcpSourceBurst      = 5
	tcpSourceConcurrent = 4
	// tcpSourcesTracked bounds the sources one tunnel remembers. When it is
	// full and none can be forgotten, a new source is refused.
	tcpSourcesTracked = 4096
)

// Why a visitor of a tcp tunnel was refused: the reason of its connection
// log row and of the access.denied event. RejectedIPGeo (package observe) is
// the ip policy of mode ip.
const (
	TCPRefusedRateLimited = "rate_limited" // the source's bucket of new connections is empty
	TCPRefusedSourceLimit = "source_limit" // the source has tcpSourceConcurrent connections
	TCPRefusedTunnelFull  = "tunnel_full"  // the tunnel has its maximum of visitors
	TCPRefusedPendingFull = "pending_full" // tcpMaxPending visitors wait for the home client
	TCPRefusedNoAddress   = "no_address"   // the connection has no address the relay can read
)

// TCPGrant is a just-in-time grant: an address or a range that may connect
// until ExpiresAt.
type TCPGrant struct {
	CIDR      string
	ExpiresAt time.Time
}

// TCPAccess is what decides who may reach a tcp service: its mode and, for
// mode ip, the permanent lists (service_ip_geo) and the grants that have not
// expired. The country lists apply in modes open and ip. MaxConnections is
// the service's cap on connected visitors, 0 for the default. ProxyProtocol
// and IdleTimeout are the service's settings of the same names in Resolved;
// they are read with the access so that a change applies without a new
// registration.
type TCPAccess struct {
	Mode                   string
	AllowCIDRs, BlockCIDRs []string
	AllowCC, BlockCC       []string
	Grants                 []TCPGrant
	MaxConnections         int
	ProxyProtocol          string
	IdleTimeout            time.Duration
	// Hostname is the service's own TLS passthrough host name
	// (Resolved.TCPHostname), "" for none. HostnameOnly is
	// Resolved.HostnameOnly.
	Hostname     string
	HostnameOnly bool
	// Limits are the service's bandwidth, daily volume and automatic bans
	// (Resolved.Limits).
	Limits TCPLimits
}

// TCPAccessLoader reads the access of a tcp service. The relay reads it when
// a tunnel of the service registers in mode ip, and again when it is told
// that the service changed (Server.RefreshTCPService); never per visitor.
type TCPAccessLoader interface {
	TCPAccess(ctx context.Context, serviceID string) (TCPAccess, error)
}

// tcpPolicy is the compiled ip policy of a tunnel in mode ip. It is never
// changed; a change of the service swaps the tunnel's pointer.
type tcpPolicy struct {
	ipMode       bool // the CIDR lists and grants apply (mode ip)
	allow, block []netip.Prefix
	grants       []tcpGrant
	// Country rules (modes open and ip), upper case.
	allowCC, blockCC []string
}

// hasCountries reports whether the policy has a country rule.
func (p *tcpPolicy) hasCountries() bool {
	return p != nil && (len(p.allowCC) > 0 || len(p.blockCC) > 0)
}

func upperCodes(in []string) []string {
	var out []string
	for _, c := range in {
		if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
			out = append(out, c)
		}
	}
	return out
}

func hasCode(list []string, cc string) bool {
	for _, c := range list {
		if c == cc {
			return true
		}
	}
	return false
}

type tcpGrant struct {
	prefix  netip.Prefix
	expires time.Time
}

// ParseTCPPrefix reads an entry of an ip policy: a CIDR or a bare address
// (one host). The prefix is masked, and an IPv4 address written as IPv6
// (::ffff:a.b.c.d) is the IPv4 one, as the address of a visitor is.
func ParseTCPPrefix(s string) (netip.Prefix, error) {
	var p netip.Prefix
	if a, err := netip.ParseAddr(s); err == nil {
		a = a.WithZone("")
		p = netip.PrefixFrom(a, a.BitLen())
	} else {
		pp, perr := netip.ParsePrefix(s)
		if perr != nil {
			return netip.Prefix{}, fmt.Errorf("%q is neither an address nor a CIDR", s)
		}
		p = pp
	}
	if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	return p.Masked(), nil
}

// compileTCPPolicy compiles the ip policy of a. An entry that cannot be read
// fails the whole policy: a block entry left out would let in what it
// blocks.
func compileTCPPolicy(a TCPAccess) (*tcpPolicy, error) {
	p := &tcpPolicy{ipMode: a.Mode != TCPAccessOpen, allowCC: upperCodes(a.AllowCC), blockCC: upperCodes(a.BlockCC)}
	if !p.ipMode {
		// Mode open: only the country rules narrow it.
		if !p.hasCountries() {
			return nil, nil
		}
		return p, nil
	}
	for _, s := range a.AllowCIDRs {
		pr, err := ParseTCPPrefix(s)
		if err != nil {
			return nil, fmt.Errorf("allow list: %w", err)
		}
		p.allow = append(p.allow, pr)
	}
	for _, s := range a.BlockCIDRs {
		pr, err := ParseTCPPrefix(s)
		if err != nil {
			return nil, fmt.Errorf("block list: %w", err)
		}
		p.block = append(p.block, pr)
	}
	for _, g := range a.Grants {
		pr, err := ParseTCPPrefix(g.CIDR)
		if err != nil {
			return nil, fmt.Errorf("grant: %w", err)
		}
		p.grants = append(p.grants, tcpGrant{prefix: pr, expires: g.ExpiresAt})
	}
	return p, nil
}

// allows reports whether a visitor from ip may connect at now.
//
// Mode ip: no block entry holds it and no block country is its country, and
// then a permanent allow entry, a grant that has not expired at now, or a
// country of the allow list does. Nothing allowed is nothing allowed: unlike
// the http ip-geo engine, an empty allow list lets nobody in. An expired
// grant stops new connections only; one that is connected stays.
//
// Mode open: the country rules only. A block country refuses, and a
// non-empty allow list of countries refuses every other country.
//
// The country of ip is unknown without a database, without a record for it
// and on a failed lookup. An allow list of countries never matches an
// unknown country, so without a database it lets nobody in by country; a
// block list never matches one either, so it lets it through.
func (p *tcpPolicy) allows(ip netip.Addr, now time.Time, geo GeoCountry, cache *geoCache) bool {
	if p == nil || !ip.IsValid() {
		return false
	}
	ip = ip.Unmap().WithZone("")
	if p.ipMode {
		for _, b := range p.block {
			if b.Contains(ip) {
				return false
			}
		}
	}
	cc := ""
	if p.hasCountries() {
		cc = cache.country(geo, ip, now)
		if cc != "" && hasCode(p.blockCC, cc) {
			return false
		}
	}
	if !p.ipMode {
		return len(p.allowCC) == 0 || (cc != "" && hasCode(p.allowCC, cc))
	}
	for _, a := range p.allow {
		if a.Contains(ip) {
			return true
		}
	}
	for _, g := range p.grants {
		if now.Before(g.expires) && g.prefix.Contains(ip) {
			return true
		}
	}
	return cc != "" && hasCode(p.allowCC, cc)
}

// trusts reports whether ip is let in by name in mode ip: a permanent allow
// entry, or a grant that has not expired at now, and no block entry. Such a
// source (a monitor, an uptime probe, the owner) is never banned
// automatically; one let in by its country only is not trusted.
func (p *tcpPolicy) trusts(ip netip.Addr, now time.Time) bool {
	if p == nil || !p.ipMode || !ip.IsValid() {
		return false
	}
	ip = ip.Unmap().WithZone("")
	for _, b := range p.block {
		if b.Contains(ip) {
			return false
		}
	}
	for _, a := range p.allow {
		if a.Contains(ip) {
			return true
		}
	}
	for _, g := range p.grants {
		if now.Before(g.expires) && g.prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// visitorAddr is the address a connection comes from, IPv4 unmapped; the
// zero Addr when it cannot be read.
func visitorAddr(c net.Conn) netip.Addr {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		if a, ok := netip.AddrFromSlice(ta.IP); ok {
			return a.Unmap()
		}
	}
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	return netip.Addr{}
}

// sourceKey is what the limits count a visitor by: its IPv4 address, or the
// /64 of its IPv6 address (one host usually has a whole /64).
func sourceKey(a netip.Addr) string {
	if a.Is6() && !a.Is4In6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.Unmap().String()
}

// TCPSourceKey is the source a visitor from a is counted, limited and
// banned as (sourceKey): the store writes a ban by hand in the same form.
func TCPSourceKey(a netip.Addr) string { return sourceKey(a.Unmap().WithZone("")) }

// tcpLimits are the limits of one tcp tunnel's visitors. admit and done are
// the only ways in and out; one mutex keeps the counts consistent.
type tcpLimits struct {
	mu       sync.Mutex
	max      int // connected visitors at most
	active   int // admitted and not done, waiting ones included
	pending  int // admitted and waiting for the home client's stream
	sources  map[string]*tcpSource
	now      func() time.Time
	refillAt time.Duration // how long one new connection takes to refill
	// owners are the owner's forward streams (they count against no cap
	// but tcpOwnerForwards); perUser the open forward streams of each other
	// user.
	owners  int
	perUser map[string]int
}

type tcpSource struct {
	tokens float64
	last   time.Time
	active int
}

func newTCPLimits(max int) *tcpLimits {
	return &tcpLimits{
		max: clampTCPMax(max), sources: map[string]*tcpSource{}, now: time.Now,
		refillAt: time.Minute / tcpSourcePerMinute,
	}
}

// clampTCPMax is a service's cap on connected visitors as the relay applies
// it: the default when it sets none, never more than MaxTCPMaxConnections.
func clampTCPMax(n int) int {
	switch {
	case n <= 0:
		return DefaultTCPMaxConnections
	case n > MaxTCPMaxConnections:
		return MaxTCPMaxConnections
	}
	return n
}

// setMax changes the cap; visitors connected beyond it stay.
func (l *tcpLimits) setMax(n int) {
	l.mu.Lock()
	l.max = clampTCPMax(n)
	l.mu.Unlock()
}

// admit lets a visitor from source in, or names why not ("" when it is
// in). An admitted visitor is pending until paired is called, and counted
// until done is.
func (l *tcpLimits) admit(source string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= l.max {
		return TCPRefusedTunnelFull
	}
	if l.pending >= tcpMaxPending {
		return TCPRefusedPendingFull
	}
	now := l.now()
	src, ok := l.sources[source]
	if !ok {
		if len(l.sources) >= tcpSourcesTracked && !l.forgetIdle(now) {
			return TCPRefusedRateLimited
		}
		src = &tcpSource{tokens: tcpSourceBurst, last: now}
		l.sources[source] = src
	}
	l.refill(src, now)
	if src.active >= tcpSourceConcurrent {
		return TCPRefusedSourceLimit
	}
	if src.tokens < 1 {
		return TCPRefusedRateLimited
	}
	src.tokens--
	src.active++
	l.active++
	l.pending++
	return ""
}

// tcpOwnerForwards is how many forward streams the owner of a service may
// have open to it, outside its cap: the users it allows (and the visitors of
// its port) can never lock the owner out.
const tcpOwnerForwards = 4

// TCPRefusedUserLimit: a user who is not the owner has forwardPerUser
// streams open to the service.
const TCPRefusedUserLimit = "user_limit"

// admitForward lets a forward stream of user in, or names why not. The
// owner's streams count only against tcpOwnerForwards. Another user's count
// against the tunnel's cap and pending pairings like a visitor, and against
// forwardPerUser for that user, never against a source (a forward is also
// limited per session, see forwardLimits). It ends with pairedForward and
// doneForward.
func (l *tcpLimits) admitForward(user string, owner bool) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if owner {
		if l.owners >= tcpOwnerForwards {
			return TCPRefusedTunnelFull
		}
		l.owners++
		return ""
	}
	if l.active >= l.max {
		return TCPRefusedTunnelFull
	}
	if l.pending >= tcpMaxPending {
		return TCPRefusedPendingFull
	}
	if l.perUser[user] >= forwardPerUser {
		return TCPRefusedUserLimit
	}
	if l.perUser == nil {
		l.perUser = map[string]int{}
	}
	l.perUser[user]++
	l.active++
	l.pending++
	return ""
}

// pairedForward ends the wait of a forward stream admitted for a user who
// is not the owner (the owner's do not wait in the pending count).
func (l *tcpLimits) pairedForward(owner bool) {
	if !owner {
		l.paired()
	}
}

// doneForward ends a forward stream admitted by admitForward.
func (l *tcpLimits) doneForward(user string, owner bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if owner {
		if l.owners > 0 {
			l.owners--
		}
		return
	}
	if l.active > 0 {
		l.active--
	}
	if n := l.perUser[user]; n <= 1 {
		delete(l.perUser, user)
	} else {
		l.perUser[user] = n - 1
	}
}

func (l *tcpLimits) refill(src *tcpSource, now time.Time) {
	if el := now.Sub(src.last); el > 0 {
		src.tokens += float64(el) / float64(l.refillAt)
		if src.tokens > tcpSourceBurst {
			src.tokens = tcpSourceBurst
		}
	}
	src.last = now
}

// forgetIdle drops the sources that have nothing connected and a full
// bucket again: remembering them changes nothing. It reports whether there
// is room now.
func (l *tcpLimits) forgetIdle(now time.Time) bool {
	for k, s := range l.sources {
		if s.active == 0 && now.Sub(s.last) >= l.refillAt*tcpSourceBurst {
			delete(l.sources, k)
		}
	}
	return len(l.sources) < tcpSourcesTracked
}

// paired ends the wait of an admitted visitor (the stream came, or did not).
func (l *tcpLimits) paired() {
	l.mu.Lock()
	if l.pending > 0 {
		l.pending--
	}
	l.mu.Unlock()
}

// done ends an admitted visitor of source.
func (l *tcpLimits) done(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active > 0 {
		l.active--
	}
	if s, ok := l.sources[source]; ok && s.active > 0 {
		s.active--
	}
}

// counts reports the connected and the waiting visitors (tests).
func (l *tcpLimits) counts() (active, pending int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.pending
}

// refusalSampler counts the refused visitors of tcp tunnels per (service,
// reason, address) and minute. The first refusal of a key in a minute is
// written to the connection log; the others are only counted, and a summary
// per service and reason is logged when the minute is over. A port that
// draws scanners writes at most refusalKeysPerMinute rows a minute.
type refusalSampler struct {
	mu      sync.Mutex
	start   time.Time
	seen    map[refusalKey]int
	dropped int // refusals of keys beyond refusalKeysPerMinute
	log     *slog.Logger
}

type refusalKey struct{ service, reason, ip string }

const refusalKeysPerMinute = 1024

// sample counts one refusal and reports whether it is the one of its key
// and minute that is written down.
func (r *refusalSampler) sample(service, reason, ip string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil || now.Sub(r.start) >= time.Minute || now.Before(r.start) {
		r.flushLocked()
		r.start, r.seen, r.dropped = now, map[refusalKey]int{}, 0
	}
	k := refusalKey{service, reason, ip}
	if n, ok := r.seen[k]; ok {
		r.seen[k] = n + 1
		return false
	}
	if len(r.seen) >= refusalKeysPerMinute {
		r.dropped++
		return false
	}
	r.seen[k] = 1
	return true
}

// flushLocked logs what the minute that ends refused, per service and
// reason, when anything was refused more than once.
func (r *refusalSampler) flushLocked() {
	if r.log == nil || len(r.seen) == 0 {
		return
	}
	type sr struct{ service, reason string }
	total, sources := map[sr]int{}, map[sr]int{}
	repeated := false
	for k, n := range r.seen {
		total[sr{k.service, k.reason}] += n
		sources[sr{k.service, k.reason}]++
		repeated = repeated || n > 1
	}
	if !repeated && r.dropped == 0 {
		return
	}
	for k, n := range total {
		r.log.Info("tcp connections refused", "service_id", k.service, "reason", k.reason,
			"count", n, "sources", sources[k], "since", r.start.UTC().Format(time.RFC3339))
	}
	if r.dropped > 0 {
		r.log.Warn("tcp connections refused from more sources than are recorded", "unrecorded", r.dropped)
	}
}

// authFailures limits the failed auth_requests on the control port per
// source: a bucket of authFailBurst failures that refills by one every
// authFailRefill. While it is empty a token from that source that is not
// valid is answered with proto.CodeRateLimited instead of invalid_token; every
// token is still checked, and a valid one gets in. It is a signal, not a gate.
type authFailures struct {
	mu      sync.Mutex
	sources map[string]*authBucket
	now     func() time.Time // read under mu
}

type authBucket struct {
	tokens float64
	last   time.Time
}

const (
	authFailBurst  = 10
	authFailRefill = 30 * time.Second
	// authFailTracked bounds the sources remembered. When it is full and
	// none can be forgotten a new source is not tracked: an address is
	// never refused for what other addresses did.
	authFailTracked = 16384
)

func newAuthFailures() *authFailures {
	return &authFailures{sources: map[string]*authBucket{}, now: time.Now}
}

func (a *authFailures) bucket(source string, now time.Time, create bool) *authBucket {
	b, ok := a.sources[source]
	if !ok {
		if !create {
			return nil
		}
		if len(a.sources) >= authFailTracked {
			for k, x := range a.sources {
				if now.Sub(x.last) >= authFailRefill*authFailBurst {
					delete(a.sources, k)
				}
			}
			if len(a.sources) >= authFailTracked {
				return nil
			}
		}
		b = &authBucket{tokens: authFailBurst, last: now}
		a.sources[source] = b
	}
	if el := now.Sub(b.last); el > 0 {
		b.tokens += float64(el) / float64(authFailRefill)
		if b.tokens > authFailBurst {
			b.tokens = authFailBurst
		}
	}
	b.last = now
	return b
}

// limited reports whether source has used up its failures.
func (a *authFailures) limited(source string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.bucket(source, a.now(), false)
	return b != nil && b.tokens < 1
}

// failed counts one failed auth_request of source.
func (a *authFailures) failed(source string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.bucket(source, a.now(), true); b != nil && b.tokens >= 1 {
		b.tokens--
	}
}

// errAuthFailed marks a handshake that failed for what the client sent: a
// wrong token, or a first message that is no auth_request. The control port
// counts these per source (authFailures).
var errAuthFailed = errors.New("auth failed")

// authFailure wraps the error of such a handshake and keeps its text.
type authFailure struct{ err error }

func (f authFailure) Error() string { return f.err.Error() }
func (f authFailure) Unwrap() []error {
	return []error{f.err, errAuthFailed}
}
