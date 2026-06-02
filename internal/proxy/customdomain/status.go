package customdomain

import "time"

// Status enum values for service_custom_domains.status (v0.5.2 Task 10).
//
// The closed enum:
//   - pending       — reserved for future ACME / auto-renew flows (rows created
//     without a cert). Never returned by ComputeStatus.
//   - active        — cert is valid and more than ExpiryWarnWindow away from
//     not_after.
//   - cert_expiring — cert is still valid but within ExpiryWarnWindow of
//     not_after.
//   - cert_expired  — cert's not_after <= now.
//
// The enum is enforced application-side on SQLite via ComputeStatus and on
// Postgres additionally via a CHECK constraint in migration 0019.
const (
	StatusPending      = "pending"
	StatusActive       = "active"
	StatusCertExpiring = "cert_expiring"
	StatusCertExpired  = "cert_expired"
)

// ExpiryWarnWindow is the duration before not_after at which a cert
// transitions from `active` to `cert_expiring`. The daily-tick fires the
// custom_domain.cert.expiring webhook exactly once on this edge.
const ExpiryWarnWindow = 14 * 24 * time.Hour

// ComputeStatus returns the status a domain should hold given its cert
// notAfter and the current time. Pending is never returned by this fn
// (it is set on INSERT for future ACME / auto-renew flows and cleared
// when a cert is provided); ComputeStatus is for cert-bearing rows only.
func ComputeStatus(notAfter, now time.Time) string {
	if !notAfter.After(now) {
		return StatusCertExpired
	}
	if notAfter.Sub(now) <= ExpiryWarnWindow {
		return StatusCertExpiring
	}
	return StatusActive
}
