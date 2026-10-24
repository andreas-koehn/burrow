package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// InsertCustomDomain inserts a new service_custom_domains row and returns the
// inserted row. Returns ErrDuplicateHostname (wrapping a sqlite UNIQUE error)
// when the hostname is already bound to another service.
func (x *DB) InsertCustomDomain(ctx context.Context, d ServiceCustomDomain) (ServiceCustomDomain, error) {
	return x.insertCustomDomain(ctx, d, nil)
}

// InsertCustomDomainCapped is InsertCustomDomain that also refuses an auto
// row over caps (ErrAutoDomainCap, ErrAutoDomainUserCap), counted in the
// write's transaction.
func (x *DB) InsertCustomDomainCapped(ctx context.Context, d ServiceCustomDomain, caps AutoDomainCaps) (ServiceCustomDomain, error) {
	return x.insertCustomDomain(ctx, d, &caps)
}

func (x *DB) insertCustomDomain(ctx context.Context, d ServiceCustomDomain, caps *AutoDomainCaps) (ServiceCustomDomain, error) {
	d.ID = uuid.NewString()
	var ok bool
	if d.Hostname, ok = NormalizeDomainName(d.Hostname); !ok {
		return ServiceCustomDomain{}, ErrInvalidHostname
	}
	if d.CertSource == "" {
		d.CertSource = CertSourceUploaded
	}
	if d.CertSource != CertSourceUploaded && d.CertSource != CertSourceAuto {
		return ServiceCustomDomain{}, ErrInvalidCertSource
	}
	now := time.Now().UTC()
	d.CreatedAt = now
	d.UpdatedAt = now

	hostOwnerMu.Lock()
	defer hostOwnerMu.Unlock()
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return ServiceCustomDomain{}, fmt.Errorf("insert custom domain: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := refuseTCPNameUnder(ctx, tx, d.Hostname); err != nil {
		return ServiceCustomDomain{}, err
	}
	if err := refuseWildcardOverlap(ctx, tx, d); err != nil {
		return ServiceCustomDomain{}, err
	}
	if err := refuseOverAutoCaps(ctx, tx, d, caps); err != nil {
		return ServiceCustomDomain{}, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO service_custom_domains
		   (id, service_id, hostname, cert_pem, key_pem, cert_sha256, not_before, not_after, created_at, updated_at, cert_source)
		   VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.ServiceID, d.Hostname, d.CertPEM, d.KeyPEM, d.CertSHA256,
		d.NotBefore, d.NotAfter, d.CreatedAt, d.UpdatedAt, d.CertSource,
	)
	if err != nil {
		if isDuplicateError(err) {
			return ServiceCustomDomain{}, ErrDuplicateHostname
		}
		return ServiceCustomDomain{}, fmt.Errorf("insert custom domain: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ServiceCustomDomain{}, fmt.Errorf("insert custom domain: commit: %w", err)
	}
	return d, nil
}

// UpdateCustomDomain replaces the cert/key/sha256/not_before/not_after columns
// for an existing row. Returns ErrNotFound when no row matches (id, service_id).
// Returns ErrDuplicateHostname when the new hostname conflicts.
func (x *DB) UpdateCustomDomain(ctx context.Context, d ServiceCustomDomain) error {
	return x.updateCustomDomain(ctx, d, nil)
}

// UpdateCustomDomainCapped is UpdateCustomDomain with the caps of
// InsertCustomDomainCapped; the row itself is not counted.
func (x *DB) UpdateCustomDomainCapped(ctx context.Context, d ServiceCustomDomain, caps AutoDomainCaps) error {
	return x.updateCustomDomain(ctx, d, &caps)
}

func (x *DB) updateCustomDomain(ctx context.Context, d ServiceCustomDomain, caps *AutoDomainCaps) error {
	var ok bool
	if d.Hostname, ok = NormalizeDomainName(d.Hostname); !ok {
		return ErrInvalidHostname
	}
	if d.CertSource == "" {
		d.CertSource = CertSourceUploaded
	}
	if d.CertSource != CertSourceUploaded && d.CertSource != CertSourceAuto {
		return ErrInvalidCertSource
	}
	now := time.Now().UTC()

	hostOwnerMu.Lock()
	defer hostOwnerMu.Unlock()
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update custom domain: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := refuseTCPNameUnder(ctx, tx, d.Hostname); err != nil {
		return err
	}
	if err := refuseWildcardOverlap(ctx, tx, d); err != nil {
		return err
	}
	if err := refuseOverAutoCaps(ctx, tx, d, caps); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE service_custom_domains
		    SET hostname=?, cert_pem=?, key_pem=?, cert_sha256=?,
		        not_before=?, not_after=?, updated_at=?, cert_source=?
		  WHERE id=? AND service_id=?`,
		d.Hostname, d.CertPEM, d.KeyPEM, d.CertSHA256,
		d.NotBefore, d.NotAfter, now, d.CertSource,
		d.ID, d.ServiceID,
	)
	if err != nil {
		if isDuplicateError(err) {
			return ErrDuplicateHostname
		}
		return fmt.Errorf("update custom domain: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update custom domain: commit: %w", err)
	}
	return nil
}

// GetCustomDomain returns the domain row matching (id, service_id), or
// ErrNotFound when no row matches.
func (x *DB) GetCustomDomain(ctx context.Context, serviceID, id string) (ServiceCustomDomain, error) {
	var d ServiceCustomDomain
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT id, service_id, hostname, cert_pem, key_pem, cert_sha256,
		        not_before, not_after, created_at, updated_at,
		        status, status_updated_at, cert_source
		   FROM service_custom_domains WHERE id=? AND service_id=?`,
		id, serviceID,
	).Scan(&d.ID, &d.ServiceID, &d.Hostname, &d.CertPEM, &d.KeyPEM, &d.CertSHA256,
		&d.NotBefore, &d.NotAfter, &d.CreatedAt, &d.UpdatedAt,
		&d.Status, &d.StatusUpdatedAt, &d.CertSource)
	if err == sql.ErrNoRows {
		return ServiceCustomDomain{}, ErrNotFound
	}
	if err != nil {
		return ServiceCustomDomain{}, fmt.Errorf("get custom domain: %w", err)
	}
	return d, nil
}

// ListCustomDomains returns all domain rows for the given serviceID.
func (x *DB) ListCustomDomains(ctx context.Context, serviceID string) ([]ServiceCustomDomain, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, service_id, hostname, cert_pem, key_pem, cert_sha256,
		        not_before, not_after, created_at, updated_at,
		        status, status_updated_at, cert_source
		   FROM service_custom_domains WHERE service_id=?
		   ORDER BY created_at`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list custom domains: %w", err)
	}
	defer rows.Close()

	var out []ServiceCustomDomain
	for rows.Next() {
		var d ServiceCustomDomain
		if err := rows.Scan(&d.ID, &d.ServiceID, &d.Hostname, &d.CertPEM, &d.KeyPEM, &d.CertSHA256,
			&d.NotBefore, &d.NotAfter, &d.CreatedAt, &d.UpdatedAt,
			&d.Status, &d.StatusUpdatedAt, &d.CertSource); err != nil {
			return nil, fmt.Errorf("scan custom domain: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteCustomDomain removes the row matching (id, service_id). It is not an
// error if no row matched (idempotent delete).
func (x *DB) DeleteCustomDomain(ctx context.Context, serviceID, id string) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM service_custom_domains WHERE id=? AND service_id=?`,
		id, serviceID,
	)
	if err != nil {
		return fmt.Errorf("delete custom domain: %w", err)
	}
	return nil
}

// ListCustomDomainsOfUser returns the custom domains of every service the
// user owns: what a user delete takes with it (its cache entries and ACME
// orders end with them).
func (x *DB) ListCustomDomainsOfUser(ctx context.Context, userID string) ([]ServiceCustomDomain, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT d.id, d.service_id, d.hostname, d.cert_pem, d.key_pem, d.cert_sha256,
		        d.not_before, d.not_after, d.created_at, d.updated_at,
		        d.status, d.status_updated_at, d.cert_source
		   FROM service_custom_domains d JOIN services s ON s.id = d.service_id
		  WHERE s.user_id = ? ORDER BY d.hostname`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list custom domains of user: %w", err)
	}
	defer rows.Close()
	var out []ServiceCustomDomain
	for rows.Next() {
		var d ServiceCustomDomain
		if err := rows.Scan(&d.ID, &d.ServiceID, &d.Hostname, &d.CertPEM, &d.KeyPEM, &d.CertSHA256,
			&d.NotBefore, &d.NotAfter, &d.CreatedAt, &d.UpdatedAt,
			&d.Status, &d.StatusUpdatedAt, &d.CertSource); err != nil {
			return nil, fmt.Errorf("scan custom domain: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LookupCustomDomainByHostname returns the domain row matching the given
// (lower-cased) hostname, or ErrNotFound.
func (x *DB) LookupCustomDomainByHostname(ctx context.Context, hostname string) (ServiceCustomDomain, error) {
	hostname = strings.ToLower(hostname)
	var d ServiceCustomDomain
	err := x.sqlDB.QueryRowContext(ctx,
		`SELECT id, service_id, hostname, cert_pem, key_pem, cert_sha256,
		        not_before, not_after, created_at, updated_at,
		        status, status_updated_at, cert_source
		   FROM service_custom_domains WHERE hostname=?`,
		hostname,
	).Scan(&d.ID, &d.ServiceID, &d.Hostname, &d.CertPEM, &d.KeyPEM, &d.CertSHA256,
		&d.NotBefore, &d.NotAfter, &d.CreatedAt, &d.UpdatedAt,
		&d.Status, &d.StatusUpdatedAt, &d.CertSource)
	if err == sql.ErrNoRows {
		return ServiceCustomDomain{}, ErrNotFound
	}
	if err != nil {
		return ServiceCustomDomain{}, fmt.Errorf("lookup custom domain by hostname: %w", err)
	}
	return d, nil
}

// ListExpiringCustomDomains returns all rows where not_after < cutoff,
// for the cert-expiry ticker.
func (x *DB) ListExpiringCustomDomains(ctx context.Context, cutoff time.Time) ([]ServiceCustomDomain, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, service_id, hostname, cert_pem, key_pem, cert_sha256,
		        not_before, not_after, created_at, updated_at,
		        status, status_updated_at, cert_source
		   FROM service_custom_domains WHERE not_after < ?
		   ORDER BY not_after`,
		cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("list expiring custom domains: %w", err)
	}
	defer rows.Close()

	var out []ServiceCustomDomain
	for rows.Next() {
		var d ServiceCustomDomain
		if err := rows.Scan(&d.ID, &d.ServiceID, &d.Hostname, &d.CertPEM, &d.KeyPEM, &d.CertSHA256,
			&d.NotBefore, &d.NotAfter, &d.CreatedAt, &d.UpdatedAt,
			&d.Status, &d.StatusUpdatedAt, &d.CertSource); err != nil {
			return nil, fmt.Errorf("scan expiring custom domain: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListAllCustomDomains returns every row in service_custom_domains. Used by
// the cert-expiry gauge tick to update metrics for all domains.
func (x *DB) ListAllCustomDomains(ctx context.Context) ([]ServiceCustomDomain, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT id, service_id, hostname, cert_pem, key_pem, cert_sha256,
		        not_before, not_after, created_at, updated_at,
		        status, status_updated_at, cert_source
		   FROM service_custom_domains ORDER BY hostname`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all custom domains: %w", err)
	}
	defer rows.Close()

	var out []ServiceCustomDomain
	for rows.Next() {
		var d ServiceCustomDomain
		if err := rows.Scan(&d.ID, &d.ServiceID, &d.Hostname, &d.CertPEM, &d.KeyPEM, &d.CertSHA256,
			&d.NotBefore, &d.NotAfter, &d.CreatedAt, &d.UpdatedAt,
			&d.Status, &d.StatusUpdatedAt, &d.CertSource); err != nil {
			return nil, fmt.Errorf("scan custom domain: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateCustomDomainStatus persists a status transition for the row matching
// id. Status must be one of customdomain.StatusPending / StatusActive /
// StatusCertExpiring / StatusCertExpired (enforced application-side; the
// Postgres twin additionally enforces via CHECK). The status_updated_at
// column is bumped to the current UTC timestamp. Returns ErrNotFound when
// no row matches.
func (x *DB) UpdateCustomDomainStatus(ctx context.Context, id, status string) error {
	now := time.Now().UTC()
	res, err := x.sqlDB.ExecContext(ctx,
		`UPDATE service_custom_domains SET status=?, status_updated_at=? WHERE id=?`,
		status, now, id,
	)
	if err != nil {
		return fmt.Errorf("update custom domain status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// isDuplicateError reports whether err is a SQLite UNIQUE constraint violation.
func isDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "unique constraint")
}

// ErrInvalidCertSource is returned by InsertCustomDomain and
// UpdateCustomDomain for a cert_source other than CertSourceUploaded and
// CertSourceAuto.
var ErrInvalidCertSource = errors.New("db: cert_source must be uploaded or auto")

// ErrDuplicateHostname is returned when a hostname INSERT conflicts with an
// existing row (UNIQUE constraint on service_custom_domains.hostname).
var ErrDuplicateHostname = fmt.Errorf("hostname already bound")

// ErrInvalidHostname is what InsertCustomDomain and UpdateCustomDomain
// return for a host name NormalizeDomainName refuses.
var ErrInvalidHostname = errors.New("db: hostname is not a DNS name")

var domainLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeDomainName is the form a custom domain is stored and compared
// in: trimmed, lower case, one trailing dot dropped. It must be a DNS name
// of at least two labels of ASCII letters, digits and '-' (an international
// name in its punycode form), whose last label is not all digits (no
// address), with at most one leading "*." for a wildcard. ok is false for
// anything else.
func NormalizeDomainName(s string) (string, bool) {
	n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	base := strings.TrimPrefix(n, "*.")
	if len(base) > 253 {
		return "", false
	}
	labels := strings.Split(base, ".")
	if len(labels) < 2 {
		return "", false
	}
	for _, l := range labels {
		if !domainLabelRE.MatchString(l) {
			return "", false
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", false
	}
	return n, true
}

// hostOwnerMu serialises every write that gives a host name an owner: a
// custom domain (InsertCustomDomain, UpdateCustomDomain) and a tcp
// service's host name (SetServiceTCPHostname). Each runs its check and its
// write in one transaction under it, so a custom domain and a tcp_hostname
// for one name never both win (H12). The relay is one process; the lock is
// per process like ipGrantMu. It is what keeps two writers apart on
// Postgres (READ COMMITTED: each would read the other's state before either
// commits); SQLite has one connection and serialises anyway. A relay run
// as several instances on one Postgres would need
// pg_advisory_xact_lock(<key>) at the start of each transaction instead.
var hostOwnerMu sync.Mutex

// ErrHostIsTCPName is what InsertCustomDomain and UpdateCustomDomain return
// for a host name that is, lies under, or is a wildcard over the
// tcp_hostname of a tcp service (H12).
var ErrHostIsTCPName = errors.New("db: hostname is a tcp host name")

// ErrTCPHostnameCovered is what SetServiceTCPHostname returns for a name a
// custom domain covers (CustomDomainCovers) (H12).
var ErrTCPHostnameCovered = errors.New("db: tcp hostname is covered by a custom domain")

// queryer is the read half of *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// domainCoversName is CustomDomainCovers for one custom domain: it is name,
// a parent of name, or "*.<parent>" over name. Both are lower case.
func domainCoversName(domain, name string) bool {
	if domain == name {
		return true
	}
	if p, ok := strings.CutPrefix(domain, "*."); ok {
		return strings.HasSuffix(name, "."+p)
	}
	return strings.HasSuffix(name, "."+domain)
}

// refuseTCPNameUnder returns ErrHostIsTCPName when the custom domain
// hostname covers the tcp_hostname of any tcp service: the names a custom
// domain covers are the http owner's (H12).
func refuseTCPNameUnder(ctx context.Context, q queryer, hostname string) error {
	rows, err := q.QueryContext(ctx, `SELECT tcp_hostname FROM services WHERE tcp_hostname IS NOT NULL AND tcp_hostname <> ''`)
	if err != nil {
		return fmt.Errorf("custom domain: tcp host names: %w", err)
	}
	defer rows.Close()
	hostname = strings.ToLower(hostname)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return fmt.Errorf("custom domain: scan tcp host name: %w", err)
		}
		if domainCoversName(hostname, strings.ToLower(n)) {
			return ErrHostIsTCPName
		}
	}
	return rows.Err()
}

// ErrHostOverlapsWildcard is what InsertCustomDomain and UpdateCustomDomain
// return for a custom domain that lies under another service's wildcard
// custom domain, or a wildcard that covers another service's custom domain
// (R09 review C1): one service could otherwise take the names of another.
var ErrHostOverlapsWildcard = errors.New("db: hostname overlaps a wildcard custom domain of another service")

// wildcardOverlap reports whether the custom domains a and b (lower case,
// "*.<parent>" for a wildcard, covering every name under the parent) share
// a name while one is a wildcard. Two equal plain names are the UNIQUE
// constraint's.
func wildcardOverlap(a, b string) bool {
	pa, wa := strings.CutPrefix(a, "*.")
	pb, wb := strings.CutPrefix(b, "*.")
	switch {
	case wa && wb:
		return pa == pb || strings.HasSuffix(pa, "."+pb) || strings.HasSuffix(pb, "."+pa)
	case wa:
		return strings.HasSuffix(b, "."+pa)
	case wb:
		return strings.HasSuffix(a, "."+pb)
	}
	return false
}

// refuseWildcardOverlap returns ErrHostOverlapsWildcard when d overlaps a
// wildcard custom domain of another service (wildcardOverlap).
func refuseWildcardOverlap(ctx context.Context, q queryer, d ServiceCustomDomain) error {
	rows, err := q.QueryContext(ctx, `SELECT hostname FROM service_custom_domains WHERE service_id <> ?`, d.ServiceID)
	if err != nil {
		return fmt.Errorf("custom domain: other services' names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return fmt.Errorf("custom domain: scan name: %w", err)
		}
		if wildcardOverlap(d.Hostname, strings.ToLower(n)) {
			return ErrHostOverlapsWildcard
		}
	}
	return rows.Err()
}

// AutoDomainCaps bound the custom domains with cert_source auto
// (BURROW_ACME_CUSTOM_MAX, BURROW_ACME_CUSTOM_PER_USER): Max in all,
// PerUser for the owner of the service. 0 allows none.
type AutoDomainCaps struct{ Max, PerUser int }

// ErrAutoDomainCap and ErrAutoDomainUserCap refuse an auto row over the
// relay's cap and over its owner's.
var (
	ErrAutoDomainCap     = errors.New("db: the relay has as many automatic certificates as it may")
	ErrAutoDomainUserCap = errors.New("db: the user has as many automatic certificates as they may")
)

// refuseOverAutoCaps counts the auto rows other than d's own and refuses
// d when it is auto and one more would pass a cap. nil caps: no limit.
func refuseOverAutoCaps(ctx context.Context, q queryer, d ServiceCustomDomain, caps *AutoDomainCaps) error {
	if caps == nil || d.CertSource != CertSourceAuto {
		return nil
	}
	var all int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM service_custom_domains WHERE cert_source = 'auto' AND id <> ?`, d.ID,
	).Scan(&all); err != nil {
		return fmt.Errorf("custom domain: count auto: %w", err)
	}
	if all+1 > caps.Max {
		return ErrAutoDomainCap
	}
	var mine int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM service_custom_domains d JOIN services s ON s.id = d.service_id
		  WHERE d.cert_source = 'auto' AND d.id <> ?
		    AND s.user_id = (SELECT user_id FROM services WHERE id = ?)`, d.ID, d.ServiceID,
	).Scan(&mine); err != nil {
		return fmt.Errorf("custom domain: count auto of the user: %w", err)
	}
	if mine+1 > caps.PerUser {
		return ErrAutoDomainUserCap
	}
	return nil
}

// CustomDomainCovers reports whether a custom domain (service_custom_domains)
// is name, a parent of name, or a wildcard "*.<parent>" over it: a TLS
// passthrough host name there would take a name an http service is reached
// by. name is lower case.
func (x *DB) CustomDomainCovers(ctx context.Context, name string) (bool, error) {
	return customDomainCovers(ctx, x.sqlDB, name)
}

func customDomainCovers(ctx context.Context, q queryer, name string) (bool, error) {
	cands := []any{name}
	for rest := name; ; {
		_, parent, ok := strings.Cut(rest, ".")
		if !ok || parent == "" {
			break
		}
		cands = append(cands, parent, "*."+parent)
		rest = parent
	}
	qs := `SELECT COUNT(*) FROM service_custom_domains WHERE hostname IN (?` + strings.Repeat(",?", len(cands)-1) + `)`
	var n int
	if err := q.QueryRowContext(ctx, qs, cands...).Scan(&n); err != nil {
		return false, fmt.Errorf("custom domain covers: %w", err)
	}
	return n > 0, nil
}
