package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// fakeBearerStore is an in-memory BearerStore for middleware tests.
type fakeBearerStore struct {
	mu        sync.Mutex
	byHash    map[string]AutomationTokenInfo
	touched   []string
	lookupErr error
}

func newFakeBearerStore() *fakeBearerStore {
	return &fakeBearerStore{byHash: map[string]AutomationTokenInfo{}}
}

func (f *fakeBearerStore) put(plaintext string, info AutomationTokenInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha256.Sum256([]byte(plaintext))
	f.byHash[hex.EncodeToString(sum[:])] = info
}

func (f *fakeBearerStore) LookupBearer(_ context.Context, hash string) (AutomationTokenInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupErr != nil {
		return AutomationTokenInfo{}, f.lookupErr
	}
	info, ok := f.byHash[hash]
	if !ok {
		return AutomationTokenInfo{}, db.ErrNotFound
	}
	return info, nil
}

func (f *fakeBearerStore) TouchBearer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
	return nil
}

// bearerUsers is a UserStore fake whose GetUserByID is configurable per-test.
type bearerUsers struct {
	fakeUsers
	users map[string]db.User
}

func (u *bearerUsers) GetUserByID(_ context.Context, id string) (db.User, error) {
	if user, ok := u.users[id]; ok {
		return user, nil
	}
	return db.User{}, db.ErrNotFound
}

// inner returns a handler that records what userID + bearerTokenID +
// bearerPerms were in ctx when it fired.
type inner struct {
	uid, tokID string
	perms      []string
	role       string
	fired      bool
}

func (i *inner) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.uid = userID(r.Context())
		i.tokID = bearerTokenID(r.Context())
		i.perms = bearerPerms(r.Context())
		i.role = callerRoleFromCtx(r.Context())
		i.fired = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequireBearerOrSessionHappyPath(t *testing.T) {
	bs := newFakeBearerStore()
	plaintext := "bua_validplaintexttoken12345"
	bs.put(plaintext, AutomationTokenInfo{
		ID:          "tok1",
		UserID:      "u1",
		Permissions: []string{"tunnels:read:any"},
	})
	us := &bearerUsers{users: map[string]db.User{"u1": {ID: "u1", Role: "admin", Status: "active"}}}

	rec := &inner{}
	h := RequireBearerOrSession(bs, us)(rec.handler())

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+plaintext)
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 got %d", rr.Code)
	}
	if !rec.fired {
		t.Fatal("handler must run on success")
	}
	if rec.uid != "u1" || rec.tokID != "tok1" || rec.role != "admin" {
		t.Fatalf("ctx not populated: %+v", rec)
	}
	if len(rec.perms) != 1 || rec.perms[0] != "tunnels:read:any" {
		t.Fatalf("bearer perms: %+v", rec.perms)
	}
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if len(bs.touched) != 1 || bs.touched[0] != "tok1" {
		t.Fatalf("touch not called: %+v", bs.touched)
	}
}

func TestRequireBearerOrSessionNoHeaderFallsThrough(t *testing.T) {
	bs := newFakeBearerStore()
	us := &bearerUsers{users: map[string]db.User{}}
	rec := &inner{}
	h := RequireBearerOrSession(bs, us)(rec.handler())
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	h.ServeHTTP(rr, r)
	if !rec.fired {
		t.Fatal("absent bearer header must fall through")
	}
	if rec.uid != "" || rec.tokID != "" {
		t.Fatalf("must not inject ctx on no-bearer path: %+v", rec)
	}
}

func TestRequireBearerOrSessionInvalidPrefix(t *testing.T) {
	bs := newFakeBearerStore()
	us := &bearerUsers{users: map[string]db.User{}}
	rec := &inner{}
	h := RequireBearerOrSession(bs, us)(rec.handler())
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer not_a_bua_token")
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	if rec.fired {
		t.Fatal("handler must NOT run on invalid prefix")
	}
}

func TestRequireBearerOrSessionUnknownHash(t *testing.T) {
	bs := newFakeBearerStore()
	us := &bearerUsers{users: map[string]db.User{}}
	rec := &inner{}
	h := RequireBearerOrSession(bs, us)(rec.handler())
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer bua_nonexistent")
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rr.Code)
	}
	if rec.fired {
		t.Fatal("handler must NOT run on unknown hash")
	}
}

func TestRequireBearerOrSessionExpired(t *testing.T) {
	bs := newFakeBearerStore()
	past := time.Now().UTC().Add(-time.Hour)
	plaintext := "bua_expiredtoken"
	bs.put(plaintext, AutomationTokenInfo{
		ID: "tok1", UserID: "u1", ExpiresAt: &past,
	})
	us := &bearerUsers{users: map[string]db.User{"u1": {ID: "u1", Role: "admin", Status: "active"}}}
	rec := &inner{}
	h := RequireBearerOrSession(bs, us)(rec.handler())

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+plaintext)
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid bearer token") {
		t.Fatalf("body: %s", rr.Body.String())
	}
}

func TestRequireBearerOrSessionMinterDeletedReturns401(t *testing.T) {
	bs := newFakeBearerStore()
	plaintext := "bua_orphaned"
	bs.put(plaintext, AutomationTokenInfo{ID: "tok1", UserID: "gone"})
	us := &bearerUsers{users: map[string]db.User{}} // no users
	rec := &inner{}
	h := RequireBearerOrSession(bs, us)(rec.handler())

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+plaintext)
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rr.Code)
	}
}

func TestRequireBearerOrSessionSuspendedReturns401(t *testing.T) {
	bs := newFakeBearerStore()
	plaintext := "bua_susp"
	bs.put(plaintext, AutomationTokenInfo{ID: "tok1", UserID: "u1"})
	us := &bearerUsers{users: map[string]db.User{"u1": {ID: "u1", Role: "admin", Status: "suspended"}}}
	rec := &inner{}
	h := RequireBearerOrSession(bs, us)(rec.handler())
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer "+plaintext)
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 got %d", rr.Code)
	}
}

// TestEffectivePermsIntersection asserts the role-and-token intersection
// rule: a token's permission only counts when the CURRENT role also grants it.
func TestEffectivePermsIntersection(t *testing.T) {
	// Cookie-authed (no bearer): role alone decides.
	if !effectivePerms(context.Background(), "admin", authz.PermTunnelsReadAny) {
		t.Fatal("admin must hold tunnels:read:any (cookie path)")
	}
	if effectivePerms(context.Background(), "user", authz.PermTunnelsReadAny) {
		t.Fatal("user must NOT hold tunnels:read:any (cookie path)")
	}

	// Bearer-authed, token declares perm AND role grants it → allow.
	ctxAdminAny := context.WithValue(context.Background(), bearerPermsKey,
		[]string{"tunnels:read:any"})
	if !effectivePerms(ctxAdminAny, "admin", authz.PermTunnelsReadAny) {
		t.Fatal("admin+bearer-declares: must hold")
	}

	// Bearer-authed, token declares perm BUT role no longer grants it (demotion).
	// → deny. This is the rule that revokes :any after role demotion.
	if effectivePerms(ctxAdminAny, "user", authz.PermTunnelsReadAny) {
		t.Fatal("intersection: demoted user must NOT hold :any even when token declares it")
	}

	// Bearer-authed, token does NOT declare perm → deny even if role grants it.
	ctxNoDecl := context.WithValue(context.Background(), bearerPermsKey, []string{})
	if effectivePerms(ctxNoDecl, "admin", authz.PermTunnelsReadAny) {
		t.Fatal("intersection: token must declare the perm")
	}
}
