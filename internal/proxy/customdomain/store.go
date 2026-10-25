// Package customdomain provides the per-domain TLS certificate store and the
// GetCertificate callback factory for the HTTPS reverse-proxy listener.
//
// # Architecture
//
// The Store caches parsed *tls.Certificate objects keyed by lowercase hostname.
// On INSERT / UPDATE / DELETE (via InvalidateHostname) the cache entry is
// evicted so the next SNI handshake re-fetches from the database.
//
// The GetCertificate function returned by CertCallback is designed to be
// assigned to tls.Config.GetCertificate. On every TLS handshake it checks the
// SNI against the cache; falls back to the supplied wildcard *tls.Certificate
// when no per-domain entry is found. A nil error is always returned on lookup
// misses — a miss must not break unrelated vhosts (spec D.4).
//
// cmd/server wiring: deferred to Task 17 (see carry-over notes in
// custom_domain_handlers.go). The store + callback are fully testable without
// cmd/server involvement.
package customdomain

import (
	"context"
	"crypto/tls"
	"strings"
	"sync"
	"time"
)

// DBStore is the narrow database surface the Store consumes.
// The concrete *db.DB + a small adapter function satisfies it; tests provide
// an in-memory stub.
type DBStore interface {
	// LookupCustomDomainByHostname returns the domain row for the given
	// (lower-cased) hostname, or (DomainRow{}, false, nil) when not found.
	// A non-nil error indicates a real I/O failure.
	LookupCustomDomainByHostname(ctx context.Context, hostname string) (DomainRow, bool, error)
}

// DomainRow is the data the Store needs from the database per domain. The API
// layer fills in a concrete db.ServiceCustomDomain and projects it here.
type DomainRow struct {
	ID        string
	ServiceID string
	Hostname  string
	CertPEM   string
	KeyPEM    string
	NotAfter  time.Time
}

// Cert is a parsed per-domain certificate together with its metadata.
type Cert struct {
	// Cert is ready for use in *tls.Config; it is pre-parsed and cached.
	// nil for a row without a certificate (cert_source auto, H10): the
	// domain is bound, and its certificate comes from elsewhere.
	Cert      *tls.Certificate
	Hostname  string
	ServiceID string
	NotAfter  time.Time
}

// Store is the process-wide per-domain cert cache.
// The zero value is not usable; call New.
type Store struct {
	db  DBStore
	mu  sync.RWMutex
	mem map[string]*Cert // lowercase hostname → parsed cert
	// neg remembers names that are no custom domain until the time given,
	// so that host dispatch (every Host header the relay does not own) and
	// handshakes for unknown names do not each cost a database query. Any
	// write clears it (InvalidateHostname); a failed lookup is not kept.
	neg map[string]time.Time
	gen uint64 // bumped by every write: a miss read before it is not kept
	now func() time.Time
}

// negativeTTL is how long a miss is remembered, negativeMax how many.
const (
	negativeTTL = 5 * time.Second
	negativeMax = 4096
)

// New constructs a Store backed by db.
func New(db DBStore) *Store {
	return &Store{
		db:  db,
		mem: make(map[string]*Cert),
		neg: make(map[string]time.Time),
		now: time.Now,
	}
}

// LookupBySNI returns the per-domain Cert for the given SNI, consulting the
// in-memory cache first. Returns ok=false when no entry exists; err is non-nil
// only for DB or parse failures.
func (s *Store) LookupBySNI(ctx context.Context, sni string) (Cert, bool, error) {
	sni = strings.ToLower(sni)

	s.mu.RLock()
	c, ok := s.mem[sni]
	until, missed := s.neg[sni]
	gen := s.gen
	s.mu.RUnlock()
	if ok {
		return *c, true, nil
	}
	if missed && s.now().Before(until) {
		return Cert{}, false, nil
	}

	// Cache miss: fetch from DB.
	row, found, err := s.db.LookupCustomDomainByHostname(ctx, sni)
	if err != nil {
		return Cert{}, false, err
	}
	if !found {
		s.rememberMiss(sni, gen)
		return Cert{}, false, nil
	}

	// A row without a certificate (cert_source auto) is bound all the
	// same: routing needs its service, the handshake another source.
	var parsed *tls.Certificate
	if row.CertPEM != "" || row.KeyPEM != "" {
		tlsCert, err := tls.X509KeyPair([]byte(row.CertPEM), []byte(row.KeyPEM))
		if err != nil {
			return Cert{}, false, err
		}
		parsed = &tlsCert
	}

	entry := &Cert{
		Cert:      parsed,
		Hostname:  row.Hostname,
		ServiceID: row.ServiceID,
		NotAfter:  row.NotAfter,
	}
	s.mu.Lock()
	s.mem[sni] = entry
	s.mu.Unlock()

	return *entry, true, nil
}

// InvalidateHostname evicts the cache entry for the given hostname so the next
// lookup re-reads from the database. Call this from INSERT / UPDATE / DELETE
// API handlers.
//
// It also forgets every remembered miss: the name may be bound now, and a
// wildcard domain covers names other than its own.
func (s *Store) InvalidateHostname(hostname string) {
	hostname = strings.ToLower(hostname)
	s.mu.Lock()
	delete(s.mem, hostname)
	clear(s.neg)
	s.gen++
	s.mu.Unlock()
}

// rememberMiss notes that name is no custom domain for negativeTTL. When the
// map is full, expired entries go first, and all of them if none expired. A
// miss read before a write (gen moved on) is not kept: the write may have
// bound the name.
func (s *Store) rememberMiss(name string, gen uint64) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	if len(s.neg) >= negativeMax {
		for k, until := range s.neg {
			if !now.Before(until) {
				delete(s.neg, k)
			}
		}
		if len(s.neg) >= negativeMax {
			clear(s.neg)
		}
	}
	s.neg[name] = now.Add(negativeTTL)
}

// CertCallback returns a tls.Config.GetCertificate function. On every TLS
// handshake it checks the SNI against the Store; returns the per-domain cert
// on a hit and the wildcard fallback on a miss. A nil wildcard is valid — the
// stdlib then uses Certificates[0] (or returns a handshake error if none are
// configured).
//
// The returned function always returns (cert, nil) — lookup errors are
// silently swallowed so that a DB hiccup during a TLS handshake doesn't
// return an error to the client for an unrelated vhost. A row without a
// certificate (cert_source auto) is skipped like a miss (H10).
func CertCallback(s *Store, wildcard *tls.Certificate) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello == nil || hello.ServerName == "" {
			return wildcard, nil
		}
		ctx := hello.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		c, ok, _ := s.LookupBySNI(ctx, hello.ServerName)
		if ok && c.Cert != nil {
			return c.Cert, nil
		}
		return wildcard, nil
	}
}
