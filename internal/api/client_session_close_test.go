package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// closedSessions records which client tokens were asked to be closed.
type closedSessions struct{ ids []string }

func (c *closedSessions) CloseByToken(id string) int {
	c.ids = append(c.ids, id)
	return 1
}

// closeHarness is the harness with a recording session closer and two client
// tokens of the plain user. It returns the ids of the tokens, sorted.
func closeHarness(t *testing.T) (*authzHarness, *closedSessions, []string) {
	t.Helper()
	closer := &closedSessions{}
	h := newAuthzHarness(t, func(d *Deps) { d.ClientSessions = closer })
	ctx := context.Background()
	for _, name := range []string{"laptop", "server"} {
		if _, err := h.st.IssueClientToken(ctx, "u-user", name); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := h.st.ListClientTokens(ctx, "u-user")
	if err != nil || len(tokens) != 2 {
		t.Fatalf("tokens: %v, %v", tokens, err)
	}
	ids := []string{tokens[0].ID, tokens[1].ID}
	sort.Strings(ids)
	return h, closer, ids
}

func TestRevokingAClientTokenClosesItsSessions(t *testing.T) {
	h, closer, ids := closeHarness(t)

	// Somebody else's token is not found, and nothing is closed for it.
	if rec := h.do(t, http.MethodDelete, "/api/v1/tokens/"+ids[0], nil, h.session(t, "admin")); rec.Code != http.StatusNotFound {
		t.Fatalf("revoking another user's token: %d", rec.Code)
	}
	if len(closer.ids) != 0 {
		t.Fatalf("closed %v for a token that was not revoked", closer.ids)
	}

	if rec := h.do(t, http.MethodDelete, "/api/v1/tokens/"+ids[0], nil, h.session(t, "user")); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if !slices.Equal(closer.ids, []string{ids[0]}) {
		t.Fatalf("closed %v, want the revoked token %s only", closer.ids, ids[0])
	}
	// The same through an automation token of the owner.
	if rec := h.do(t, http.MethodDelete, "/api/v1/tokens/"+ids[1], nil, h.token(t, "user", "tokens:manage:own")); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke with an automation token: %d %s", rec.Code, rec.Body)
	}
	if !slices.Equal(closer.ids, ids) {
		t.Fatalf("closed %v, want %v", closer.ids, ids)
	}
}

func TestSuspendingAUserClosesTheSessionsOfItsTokens(t *testing.T) {
	h, closer, ids := closeHarness(t)
	admin := h.session(t, "admin")
	patch := func(status string) int {
		return h.do(t, http.MethodPatch, "/api/v1/users/u-user", map[string]string{"status": status}, admin).Code
	}
	if code := patch("suspended"); code != http.StatusNoContent {
		t.Fatalf("suspend: %d", code)
	}
	got := slices.Clone(closer.ids)
	sort.Strings(got)
	if !slices.Equal(got, ids) {
		t.Fatalf("closed %v, want both tokens %v", got, ids)
	}
	// The tokens are still there, and they no longer open a session.
	closer.ids = nil
	if code := patch("active"); code != http.StatusNoContent || len(closer.ids) != 0 {
		t.Fatalf("reactivating: %d, closed %v; want 204 and nothing closed", code, closer.ids)
	}
	// A change of the role alone closes nothing.
	if rec := h.do(t, http.MethodPatch, "/api/v1/users/u-user", map[string]string{"role": "admin"}, admin); rec.Code != http.StatusNoContent || len(closer.ids) != 0 {
		t.Fatalf("role change: %d, closed %v", rec.Code, closer.ids)
	}
}

func TestDeletingAUserClosesTheSessionsOfItsTokens(t *testing.T) {
	h, closer, ids := closeHarness(t)
	admin := h.session(t, "admin")
	if rec := h.do(t, http.MethodDelete, "/api/v1/users/nope", nil, admin); rec.Code != http.StatusNotFound || len(closer.ids) != 0 {
		t.Fatalf("deleting nobody: %d, closed %v", rec.Code, closer.ids)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/users/u-user", nil, admin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	got := slices.Clone(closer.ids)
	sort.Strings(got)
	if !slices.Equal(got, ids) {
		t.Fatalf("closed %v, want both tokens %v (they are read before the user is deleted)", got, ids)
	}
}

// failingTokenList is the user store with a client token list that fails.
type failingTokenList struct{ UserStore }

func (failingTokenList) ListClientTokens(context.Context, string) ([]db.ClientToken, error) {
	return nil, errors.New("list failed")
}

// A suspension reads the user's tokens before it changes anything: when the
// list fails the answer is 500, the user stays active and nothing is closed.
func TestSuspendingAUserWithAFailingTokenListChangesNothing(t *testing.T) {
	closer := &closedSessions{}
	h := newAuthzHarness(t, func(d *Deps) {
		d.ClientSessions = closer
		d.Users = failingTokenList{d.Users}
	})
	rec := h.do(t, http.MethodPatch, "/api/v1/users/u-user", map[string]string{"status": "suspended"}, h.session(t, "admin"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("suspend with a failing list: %d, want 500", rec.Code)
	}
	u, err := h.st.GetUserByID(context.Background(), "u-user")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status == "suspended" {
		t.Error("the user was suspended although the answer was 500")
	}
	if len(closer.ids) != 0 {
		t.Errorf("closed %v, want nothing", closer.ids)
	}
}

// Without a closer (a relay that runs no control server) the three routes
// work as before.
func TestSessionCloserIsOptional(t *testing.T) {
	h := newAuthzHarness(t)
	if _, err := h.st.IssueClientToken(context.Background(), "u-user", "laptop"); err != nil {
		t.Fatal(err)
	}
	tokens, _ := h.st.ListClientTokens(context.Background(), "u-user")
	if rec := h.do(t, http.MethodDelete, "/api/v1/tokens/"+tokens[0].ID, nil, h.session(t, "user")); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/users/u-user", nil, h.session(t, "admin")); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
}
