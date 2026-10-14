// bearer_middleware.go — automation API bearer auth (spec Part M).
//
// RequireBearerOrSession runs BEFORE the existing RequireSession+RequireCSRF
// pair. When the request carries Authorization: Bearer bua_<token>, the
// middleware sha256-hashes the plaintext, looks the token up via BearerStore,
// and on success injects the token's user id, the token id, the closed
// permission set declared at mint, and the user's CURRENT role into the
// context. RequireSession then sees the userID already set and skips its
// cookie check; RequireCSRF sees bearerTokenID and skips the double-submit
// header check (the bearer secret IS the CSRF defense — it cannot be sent
// from a third-party origin).
//
// When the Authorization header is absent the middleware is a no-op: the
// request falls through to RequireSession, which enforces the cookie flow
// (and rejects 401 if the cookie is missing).
//
// Permission enforcement on bearer-authed requests is the intersection of
// the bearer's declared permission set and the user's CURRENT role — so
// demoting the minter user immediately revokes the token's :any reach.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// AutomationTokenInfo is the narrow view the middleware needs back from the
// bearer store. *store.Store's AutomationTokenView satisfies it via adapter.
type AutomationTokenInfo struct {
	ID          string
	UserID      string
	Permissions []string
	ExpiresAt   *time.Time
}

// BearerStore is the lookup + touch surface consumed by the bearer
// middleware. *store.Store satisfies it (the API package adapter just
// translates AutomationTokenView → AutomationTokenInfo). Tests provide
// a tiny in-memory fake.
type BearerStore interface {
	// LookupBearer returns the row matching the sha256-hex hash, or
	// db.ErrNotFound if no token has that hash. Expiry is NOT enforced
	// here — the middleware checks ExpiresAt itself so 401 messaging
	// stays consistent with not-found.
	LookupBearer(ctx context.Context, hash string) (AutomationTokenInfo, error)
	// TouchBearer updates last_used. Best-effort; the middleware logs
	// but does not fail the request on error.
	TouchBearer(ctx context.Context, id string) error
}

// storeBackedBearerStore adapts *store.Store's AutomationTokenView return
// to the narrower AutomationTokenInfo the middleware consumes. cmd/server
// uses this adapter so wiring stays one-line; tests need not.
type storeBackedBearerStore struct{ s *store.Store }

// NewStoreBearerStore wraps a *store.Store so it satisfies BearerStore.
func NewStoreBearerStore(s *store.Store) BearerStore { return storeBackedBearerStore{s: s} }

func (a storeBackedBearerStore) LookupBearer(ctx context.Context, hash string) (AutomationTokenInfo, error) {
	v, err := a.s.LookupBearer(ctx, hash)
	if err != nil {
		return AutomationTokenInfo{}, err
	}
	return AutomationTokenInfo{
		ID:          v.ID,
		UserID:      v.UserID,
		Permissions: v.Permissions,
		ExpiresAt:   v.ExpiresAt,
	}, nil
}

func (a storeBackedBearerStore) TouchBearer(ctx context.Context, id string) error {
	return a.s.TouchBearer(ctx, id)
}

// sha256Hex is the canonical token-hash function used everywhere in the
// auth surface (matches auth.HashToken's algorithm). Kept private to this
// file so the bearer flow doesn't accidentally depend on the client-token
// helper's plaintext-vs-hash signature.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// RequireBearerOrSession is the front-of-chain auth middleware for the
// automation API. When Authorization: Bearer bua_<token> is present, it
// authenticates the token and ctx-injects userID + bearerTokenID +
// bearerPerms + callerRole. When the header is absent, the middleware is a
// no-op and the request flows through to the existing cookie-session
// middlewares.
//
// Failure modes on bearer requests:
//   - missing/invalid prefix     → 401 "invalid bearer token"
//   - unknown hash               → 401 "invalid bearer token"
//   - expired                    → 401 "invalid bearer token"
//   - minter user deleted        → 401 "invalid bearer token"
//   - minter user suspended      → 401 "invalid bearer token"
//   - infra error on lookup      → 500 "internal error"
//
// (Every failure rendering is JSON via writeErr; matches the chain shape.)
func RequireBearerOrSession(bs BearerStore, us UserStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				next.ServeHTTP(w, r)
				return
			}
			tok := strings.TrimPrefix(auth, "Bearer ")
			if !strings.HasPrefix(tok, "bua_") || bs == nil {
				writeErr(w, http.StatusUnauthorized, "invalid bearer token")
				return
			}
			info, err := bs.LookupBearer(r.Context(), sha256Hex(tok))
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					writeErr(w, http.StatusUnauthorized, "invalid bearer token")
					return
				}
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
			if info.ExpiresAt != nil && info.ExpiresAt.Before(time.Now().UTC()) {
				writeErr(w, http.StatusUnauthorized, "invalid bearer token")
				return
			}
			if us == nil {
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
			u, err := us.GetUserByID(r.Context(), info.UserID)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					writeErr(w, http.StatusUnauthorized, "invalid bearer token")
					return
				}
				writeErr(w, http.StatusInternalServerError, "internal error")
				return
			}
			if u.Status == "suspended" {
				writeErr(w, http.StatusUnauthorized, "invalid bearer token")
				return
			}
			// Best-effort touch (do not fail the request on error).
			_ = bs.TouchBearer(r.Context(), info.ID)

			ctx := context.WithValue(r.Context(), userIDKey, u.ID)
			ctx = context.WithValue(ctx, bearerTokenIDKey, info.ID)
			ctx = context.WithValue(ctx, bearerPermsKey, info.Permissions)
			ctx = context.WithValue(ctx, callerRoleKey, u.Role)
			if info.ExpiresAt != nil {
				ctx = context.WithValue(ctx, bearerExpiresKey, *info.ExpiresAt)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// effectivePerms reports whether the request's caller is allowed to exercise
// the named permission. For cookie-authed requests this is just authz.Can on
// the role. For bearer-authed requests it is the INTERSECTION of the token's
// declared permission set (snapshot at mint) and the user's CURRENT role:
// the token must declare p AND the role must still grant p. This is the rule
// that makes role demotion immediately revoke a token's :any reach.
//
// A request is a token's when it carries a token id, whatever the token
// declares: one that declares nothing (or whose store hands back no list at
// all) is still a token and holds nothing.
func effectivePerms(ctx context.Context, role string, p authz.Permission) bool {
	if bearerTokenID(ctx) == "" {
		return authz.Can(role, p)
	}
	return slices.Contains(bearerPerms(ctx), string(p)) && authz.Can(role, p)
}

// firstNotHeld returns the first of perms that the caller does not hold (a
// session: its role; a token: declared and granted, see effectivePerms), or ""
// when it holds them all. A key that is no permission at all is skipped: the
// store answers that one.
func firstNotHeld(ctx context.Context, role string, perms []string) string {
	known := authz.AllPermissions()
	for _, p := range perms {
		if slices.Contains(known, authz.Permission(p)) && !effectivePerms(ctx, role, authz.Permission(p)) {
			return p
		}
	}
	return ""
}

// actingRole is the role a handler hands to a store that scopes by role: the
// caller's own rows, or everyone's. own is the :own permission that store is
// going to check. A dashboard session acts with its user's role. An
// automation token acts with it only where it has one of anyPerms (the :any
// half of what the route needs, by effectivePerms); otherwise it acts as the
// built-in "user", whose reach ends at the caller's own rows, but only when
// the caller's real role grants own. A token never gets more than its user's
// role, so a role without own acts as "", which holds nothing. An admin's
// token that declares services:configure:own does not configure the services
// of other users.
func (d Deps) actingRole(r *http.Request, own authz.Permission, anyPerms ...authz.Permission) (string, error) {
	role, err := d.callerRoleForAuth(r)
	if err != nil || bearerTokenID(r.Context()) == "" {
		return role, err
	}
	for _, p := range anyPerms {
		if effectivePerms(r.Context(), role, p) {
			return role, nil
		}
	}
	if authz.Can(role, own) {
		return "user", nil
	}
	return "", nil
}
