// database_handlers.go — database-backend status endpoint (spec G.1 / Task 15).
//
// GET /api/v1/database → 200 {driver, postgres_alpha, url_redacted}
//
// Permission: metrics:read, as for /metrics and /openapi/viewer (routeAccess
// in access.go).
// url_redacted is the database URL with user:password replaced by ****:****;
// for SQLite it is the on-disk file path (no credentials to redact).
package api

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// DBInfo carries the database backend metadata surfaced by
// GET /api/v1/database. It is populated by cmd/server at startup and stored
// on Deps — all fields are plain values (no behaviour), so there is no
// interface to nil-guard in the handler.
type DBInfo struct {
	// Driver is "sqlite" or "postgres".
	Driver string
	// URLRedacted is the connection string with user:password replaced by
	// ****:**** (for SQLite, this is the database file path).
	URLRedacted string
	// Alpha is true when the Postgres backend is in alpha-experimental mode
	// (cfg.ExperimentalPostgres == true && Driver == "postgres").
	Alpha bool
}

// dbStatusResp is the JSON body of GET /api/v1/database.
type dbStatusResp struct {
	Driver        string `json:"driver"`
	PostgresAlpha bool   `json:"postgres_alpha"`
	URLRedacted   string `json:"url_redacted"`
}

// GetDatabaseStatus handles GET /api/v1/database.
//
// Returns the active database driver, whether Postgres alpha is in effect,
// and a credential-redacted form of the connection string. The handler is
// pure-read (no I/O, no DB query) — it renders the Deps.DBInfo value that
// cmd/server populated at startup.
func (d Deps) GetDatabaseStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, dbStatusResp{
		Driver:        d.Database.Driver,
		PostgresAlpha: d.Database.Alpha,
		URLRedacted:   d.Database.URLRedacted,
	})
}

// RedactDatabaseURL strips user:password from a Postgres DSN for safe
// display. If the raw string is not a recognisable URL (no scheme/host),
// a regex fallback replaces the user:pass segment. For SQLite paths (no
// "://" present) the input is returned unchanged.
//
// This function is exported so cmd/server can use it when populating DBInfo,
// and so it can be unit-tested independently of the handler.
//
// Examples:
//
//	postgres://user:s3cr3t@host/db?sslmode=verify-full
//	→ postgres://****:****@host/db?sslmode=verify-full
//
//	/var/lib/burrow/burrow.db  → /var/lib/burrow/burrow.db  (unchanged)
func RedactDatabaseURL(raw string) string {
	// Fast path: no "://" → treat as a file path (SQLite), return unchanged.
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Unparseable URL with "://" — use regex to scrub credentials.
		return pgCredRe.ReplaceAllString(raw, "${scheme}****:****@")
	}
	if u.User != nil {
		// Replace the userinfo segment with literal ****:**** by rebuilding
		// the URL string manually to avoid net/url's percent-encoding of *.
		// Format: scheme://****:****@host/path?query
		hostPath := u.Host
		if u.Path != "" {
			hostPath += u.Path
		}
		if u.RawQuery != "" {
			hostPath += "?" + u.RawQuery
		}
		if u.Fragment != "" {
			hostPath += "#" + u.Fragment
		}
		return u.Scheme + "://****:****@" + hostPath
	}
	return raw
}

// pgCredRe matches the user:pass@ part of a Postgres URL as a fallback when
// url.Parse does not populate User (e.g. malformed but partially parseable URLs).
var pgCredRe = regexp.MustCompile(`(?P<scheme>[a-z][a-z0-9+\-.]*://)[^:@/]+:[^@/]*@`)
