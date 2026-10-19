package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

type fakeSessions struct {
	list    []db.Session
	revoked []string
	others  int64
}

func (f *fakeSessions) ListSessions(_ context.Context, uid string) ([]db.Session, error) {
	return f.list, nil
}
func (f *fakeSessions) RevokeSession(_ context.Context, id, uid string) error {
	if id == "missing" {
		return db.ErrNotFound
	}
	f.revoked = append(f.revoked, id)
	return nil
}
func (f *fakeSessions) RevokeOtherSessions(_ context.Context, uid, keep string) (int64, error) {
	return f.others, nil
}

func TestSessionEndpoints(t *testing.T) {
	fs := &fakeSessions{
		list:   []db.Session{{ID: "s1", UserID: "u1", IP: "1.1.1.1", UserAgent: "UA", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}},
		others: 3,
	}
	d := Deps{Log: discardLog(), Sessions: fs, Users: &fakeUserStore{role: "user"}}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/sessions")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("list sessions status=%d", r.StatusCode)
	}
	raw := readBody(t, r)
	var ls []sessionResp
	if err := json.Unmarshal([]byte(raw), &ls); err != nil {
		t.Fatal(err)
	}
	// The id is a hash of the session id, never the session id (the cookie
	// value) itself.
	if len(ls) != 1 || ls[0].ID != sessionRef("s1") || ls[0].ID == "s1" || strings.Contains(raw, `"s1"`) {
		t.Fatalf("sessions: %s", raw)
	}

	if r := c.delete(t, "/api/v1/sessions/"+ls[0].ID); r.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status=%d", r.StatusCode)
	}
	if len(fs.revoked) != 1 || fs.revoked[0] != "s1" {
		t.Fatalf("revoked %v, want the session behind the hash", fs.revoked)
	}
	// The raw id names nothing any more; neither does an unknown hash.
	for _, id := range []string{"s1", "missing", sessionRef("s2")} {
		if r := c.delete(t, "/api/v1/sessions/"+id); r.StatusCode != http.StatusNotFound {
			t.Fatalf("revoke %q status=%d want 404", id, r.StatusCode)
		}
	}

	r = c.post(t, "/api/v1/sessions/revoke-all", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("revoke-all status=%d", r.StatusCode)
	}
	var ra struct {
		Revoked int64 `json:"revoked"`
	}
	json.NewDecoder(r.Body).Decode(&ra)
	r.Body.Close()
	if ra.Revoked != 3 {
		t.Fatalf("revoke-all count=%d want 3", ra.Revoked)
	}
}
