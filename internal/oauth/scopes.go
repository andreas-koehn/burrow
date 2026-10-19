package oauth

import (
	"slices"
	"strings"

	"github.com/ankoehn/burrow/internal/authz"
)

// parseScope splits a scope parameter into its distinct values, in order.
// offline_access is dropped: refresh tokens are always issued, and an MCP
// server should not list it (MCP authorization, scope selection).
func parseScope(raw string) []string {
	out := []string{}
	for _, s := range strings.Fields(raw) {
		if s == "offline_access" || slices.Contains(out, s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// defaultScopes is what a request without a scope parameter asks for.
func defaultScopes(kind string) []string {
	if kind == ResourceBurrow {
		return []string{ScopeBurrowRead}
	}
	return []string{ScopeMCPTools}
}

// validScopes reports whether every scope is one the resource kind knows:
// burrow:read, burrow:write or a permission string at the management server,
// mcp:tools at a gateway resource.
func validScopes(kind string, scopes []string) bool {
	if len(scopes) == 0 {
		return false
	}
	for _, s := range scopes {
		if kind != ResourceBurrow {
			if s != ScopeMCPTools {
				return false
			}
			continue
		}
		if s == ScopeBurrowRead || s == ScopeBurrowWrite {
			continue
		}
		if !slices.Contains(authz.AllPermissions(), authz.Permission(s)) {
			return false
		}
	}
	return true
}

// ReadPermissions are the permissions burrow:read stands for (before the
// role's own are intersected): those that only read.
func ReadPermissions() []authz.Permission {
	var out []authz.Permission
	for _, p := range authz.AllPermissions() {
		if strings.Contains(string(p), ":read") {
			out = append(out, p)
		}
	}
	return out
}

// ScopePermissions expands the scopes of a /mcp/burrow grant into the
// permissions they stand for: burrow:write every permission, burrow:read
// ReadPermissions, a permission string itself. The effective set of a token
// is this intersected with what the user's role grants now (as for an
// automation token); the resource server does that intersection.
func ScopePermissions(scopes []string) []authz.Permission {
	var out []authz.Permission
	add := func(p authz.Permission) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, s := range scopes {
		switch s {
		case ScopeBurrowWrite:
			for _, p := range authz.AllPermissions() {
				add(p)
			}
		case ScopeBurrowRead:
			for _, p := range ReadPermissions() {
				add(p)
			}
		default:
			if slices.Contains(authz.AllPermissions(), authz.Permission(s)) {
				add(authz.Permission(s))
			}
		}
	}
	return out
}
