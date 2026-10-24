package server

// tcp_geo.go — country rules of a tcp service (E5): the lookup the relay
// resolves a visitor's country with, and a small cache of its answers.

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// GeoCountry resolves an address to its ISO-3166-1 alpha-2 country. Enabled
// is false when no database is loaded. proxy.GeoLookup satisfies it.
type GeoCountry interface {
	Country(ip net.IP) (string, error)
	Enabled() bool
}

type geoBox struct{ g GeoCountry }

// SetGeo gives the relay the database its tcp services' country rules are
// checked against. Call it before Serve; nil (the default) is the same as no
// database: an allow list of countries lets nobody in by country, and a
// block list cannot be enforced.
func (s *Server) SetGeo(g GeoCountry) { s.geo.Store(&geoBox{g}) }

const (
	// geoCacheTTL is how long the country of a source is remembered; the
	// lookup itself is cheap, the cache keeps a scanner's burst off the
	// reader. geoCacheMax bounds it: when full it starts again.
	geoCacheTTL = 5 * time.Minute
	geoCacheMax = 8192
)

type geoCacheEntry struct {
	cc      string
	expires time.Time
}

// geoCache remembers the country of each source address for geoCacheTTL.
type geoCache struct {
	mu sync.Mutex
	m  map[netip.Addr]geoCacheEntry
}

// country is the upper-case code of ip, or "" when it is unknown: no
// database, no record, or a failed lookup. A reload of the database is seen
// when the entry has expired.
func (c *geoCache) country(g GeoCountry, ip netip.Addr, now time.Time) string {
	if g == nil || !g.Enabled() {
		return ""
	}
	c.mu.Lock()
	if e, ok := c.m[ip]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.cc
	}
	c.mu.Unlock()
	cc, err := g.Country(net.IP(ip.AsSlice()))
	if err != nil {
		return "" // unknown, and not remembered
	}
	cc = strings.ToUpper(strings.TrimSpace(cc))
	c.mu.Lock()
	if c.m == nil || len(c.m) >= geoCacheMax {
		c.m = map[netip.Addr]geoCacheEntry{}
	}
	c.m[ip] = geoCacheEntry{cc: cc, expires: now.Add(geoCacheTTL)}
	c.mu.Unlock()
	return cc
}

// reset forgets every country (a database that was swapped).
func (c *geoCache) reset() {
	c.mu.Lock()
	c.m = nil
	c.mu.Unlock()
}

func (s *Server) geoLookup() GeoCountry {
	if b := s.geo.Load(); b != nil {
		return b.g
	}
	return nil
}
