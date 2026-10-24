package proxy

// ipgeo.go — per-service IP allow/block + country-code gating middleware.
//
// The IP-CIDR matcher and the GeoLookup interface. The country lookup is the
// MMDB reader of geo.go (BURROW_GEOIP_DB); without a database it is a no-op.
//
// The chain wiring (internal/aigw/chain.go) currently exposes a stub IPGeo
// field — Task 25 wires this engine into the chain. Task 16's job is to
// provide the engine + JSON API + DB CRUD.
//
// Decision semantics (spec Part J):
//
//   - block_cidrs match → deny first (overrides allow_cidrs).
//   - allow_cidrs non-empty AND no match → deny.
//   - allow_cidrs empty → allow by default (no allowlist).
//   - block_countries match → deny.
//   - allow_countries non-empty AND no match → deny.
//   - a country that is unknown (no record, a failed lookup, no database) → deny
//     when allow_countries is non-empty, allow otherwise. Without a database a
//     block list is therefore not enforced; the API refuses to save one and
//     /geo/status reports it.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/ankoehn/burrow/pkg/clientip"
)

// GeoLookup resolves a visitor IP to an ISO-3166-1 alpha-2 country code.
//
// Implementations:
//   - noopGeoLookup (no database configured): always returns ("", nil); the
//     engine treats this as "country unknown".
//   - MMDBGeoLookup: wraps oschwald/maxminddb-golang.
type GeoLookup interface {
	Country(ip net.IP) (string, error)
	Enabled() bool
	DBPath() string
	DBAgeSeconds() int64
}

// noopGeoLookup is the GeoLookup of a relay with no database: it reports Enabled()=false
// and returns ("", nil) for every IP. The middleware treats "" as "unknown"
// and skips country enforcement (so the country lists are accepted at the
// API for forward-compat but never used in the default build).
type noopGeoLookup struct{}

// NoopGeoLookup returns the default-build GeoLookup that disables country
// resolution. The proxy chain uses this when no geo-tagged build is in play.
func NoopGeoLookup() GeoLookup { return noopGeoLookup{} }

func (noopGeoLookup) Country(net.IP) (string, error) { return "", nil }
func (noopGeoLookup) Enabled() bool                  { return false }
func (noopGeoLookup) DBPath() string                 { return "" }
func (noopGeoLookup) DBAgeSeconds() int64            { return 0 }

// IPGeoPolicy is the resolved per-service ip-geo decision rules.
//
// AllowCIDRs / BlockCIDRs are pre-parsed []*net.IPNet to avoid per-request
// parsing on the hot path. Empty lists mean "no rule of this kind".
type IPGeoPolicy struct {
	AllowCIDRs     []*net.IPNet
	BlockCIDRs     []*net.IPNet
	AllowCountries []string // upper-case ISO-3166-1 alpha-2
	BlockCountries []string
}

// IsEmpty reports whether the policy has any active rule. An empty policy
// is a pure pass-through (the proxy can skip every check).
func (p IPGeoPolicy) IsEmpty() bool {
	return len(p.AllowCIDRs) == 0 && len(p.BlockCIDRs) == 0 &&
		len(p.AllowCountries) == 0 && len(p.BlockCountries) == 0
}

// CompileIPGeoPolicy parses string CIDR lists into IPGeoPolicy. Country lists
// are upper-cased + de-duped. Unparseable CIDRs return an error so the API
// can surface 400 — the proxy hot path must never see malformed input.
func CompileIPGeoPolicy(allowCIDRs, blockCIDRs, allowCountries, blockCountries []string) (IPGeoPolicy, error) {
	allow, err := parseCIDRs(allowCIDRs, "allow_cidrs")
	if err != nil {
		return IPGeoPolicy{}, err
	}
	block, err := parseCIDRs(blockCIDRs, "block_cidrs")
	if err != nil {
		return IPGeoPolicy{}, err
	}
	return IPGeoPolicy{
		AllowCIDRs:     allow,
		BlockCIDRs:     block,
		AllowCountries: normaliseCountries(allowCountries),
		BlockCountries: normaliseCountries(blockCountries),
	}, nil
}

// IPGeoEngine is the per-request decision engine constructed once per
// service-policy load. It owns the resolved policy + the GeoLookup the
// chain should consult.
//
// Allow(ip) returns (true, "") to pass through, (false, "ip_geo") to deny.
// Callers (the chain wiring in Task 25, or test code today) translate
// the deny code into a 403 JSON envelope.
type IPGeoEngine struct {
	policy IPGeoPolicy
	geo    GeoLookup
}

// NewIPGeoEngine wraps a compiled policy + a GeoLookup. A nil GeoLookup
// is auto-replaced with NoopGeoLookup() so callers can pass nil for no
// database.
func NewIPGeoEngine(policy IPGeoPolicy, geo GeoLookup) *IPGeoEngine {
	if geo == nil {
		geo = noopGeoLookup{}
	}
	return &IPGeoEngine{policy: policy, geo: geo}
}

// Policy returns the compiled policy. Useful for tests.
func (e *IPGeoEngine) Policy() IPGeoPolicy { return e.policy }

// Allow runs the decision rules for one visitor IP.
//
//   - returns (true, "") when the IP is allowed.
//   - returns (false, "ip_geo") when the IP is blocked by any rule.
//
// The reason code matches spec Part J's 403 body:
//
//	{"error":"forbidden","reason":"ip_geo"}
func (e *IPGeoEngine) Allow(ip net.IP) (bool, string) {
	if e.policy.IsEmpty() {
		return true, ""
	}
	if ip == nil {
		// Unresolvable visitor IP → only deny if there is an allowlist that
		// would have required a match. Pure pass-through otherwise.
		if len(e.policy.AllowCIDRs) > 0 || len(e.policy.AllowCountries) > 0 {
			return false, "ip_geo"
		}
		return true, ""
	}

	// 1. block_cidrs takes priority — explicit block overrides every allow.
	for _, n := range e.policy.BlockCIDRs {
		if n.Contains(ip) {
			return false, "ip_geo"
		}
	}
	// 2. allow_cidrs: non-empty and no match → deny.
	if len(e.policy.AllowCIDRs) > 0 {
		matched := false
		for _, n := range e.policy.AllowCIDRs {
			if n.Contains(ip) {
				matched = true
				break
			}
		}
		if !matched {
			return false, "ip_geo"
		}
	}

	// 3. Country rules. A country the database does not know (no record, a
	//    failed lookup, or no database at all) follows the list: an allow
	//    list denies it, a block list lets it through. Without a database
	//    an allow list therefore denies everyone it has not already let in
	//    by address; a block list cannot be enforced (the API says so and
	//    refuses to save one, and /geo/status reports it).
	if len(e.policy.AllowCountries) == 0 && len(e.policy.BlockCountries) == 0 {
		return true, ""
	}
	country := ""
	if e.geo.Enabled() {
		if c, err := e.geo.Country(ip); err == nil {
			country = strings.ToUpper(strings.TrimSpace(c))
		}
	}
	if country != "" {
		for _, c := range e.policy.BlockCountries {
			if c == country {
				return false, "ip_geo"
			}
		}
	}
	if len(e.policy.AllowCountries) > 0 {
		for _, c := range e.policy.AllowCountries {
			if c == country {
				return true, ""
			}
		}
		return false, "ip_geo"
	}
	return true, ""
}

// AllowRequest is the http.Handler-friendly wrapper. It extracts the visitor
// IP from r (X-Forwarded-For honored only when peer ∈ trustedProxies), runs
// Allow, and on deny writes a 403 JSON envelope with reason:ip_geo.
//
// On allow, the function returns true and the caller should proceed to the
// next handler. The trustedProxies argument MUST be the same []*net.IPNet
// used elsewhere in the proxy (cmd/server passes them via WithTrustedProxies).
func (e *IPGeoEngine) AllowRequest(w http.ResponseWriter, r *http.Request, trustedProxies []*net.IPNet) bool {
	ipStr := clientip.Resolve(
		r.RemoteAddr,
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-IP"),
		trustedProxies,
	)
	ip := net.ParseIP(ipStr)
	ok, reason := e.Allow(ip)
	if ok {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `{"error":"forbidden","reason":%q}`, reason)
	return false
}

// parseCIDRs parses a list of CIDR strings into []*net.IPNet. The label
// argument is included in error messages so the API layer can return
// {"error":"invalid <label>: <entry>"} on 400.
func parseCIDRs(in []string, label string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err == nil {
			out = append(out, n)
			continue
		}
		// Bare IP → host CIDR.
		if ip := net.ParseIP(s); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			ip4 := ip.To4()
			if ip4 != nil {
				ip = ip4
			}
			out = append(out, &net.IPNet{
				IP:   ip.Mask(net.CIDRMask(bits, bits)),
				Mask: net.CIDRMask(bits, bits),
			})
			continue
		}
		return nil, fmt.Errorf("invalid %s: %q", label, s)
	}
	return out, nil
}

// normaliseCountries upper-cases + trims + de-duplicates the input. Empty
// strings are dropped.
func normaliseCountries(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

// ValidateCountryCode reports whether c is a syntactically valid ISO-3166-1
// alpha-2 code (two upper-case letters). Used by the API to 400 on bad input
// before the row is written.
func ValidateCountryCode(c string) error {
	c = strings.TrimSpace(c)
	if len(c) != 2 {
		return errors.New("country code must be 2 letters")
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			// Try the upper-case form before rejecting.
			if r >= 'a' && r <= 'z' {
				continue
			}
			return errors.New("country code must be ASCII letters")
		}
	}
	return nil
}
