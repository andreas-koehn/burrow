package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/store"
)

// cookieSession is a dashboard request carrying sid as burrow_session.
func cookieSession(sid string) func(*http.Request) {
	return func(req *http.Request) {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
		req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf"})
		req.Header.Set("X-CSRF-Token", "csrf")
	}
}

// Round 2: the dashboard refuses a services session as burrow_session, and
// signing out of the dashboard, or revoking a dashboard session, ends the
// services sessions it handed off. Signing out everywhere else ends them all.
func TestSessions_ServicesSessionsFollowTheirDashboardSession(t *testing.T) {
	h := newAuthzHarness(t)
	ctx := context.Background()
	newDash := func() string {
		sid, err := h.st.CreateSession(ctx, "u-admin", "ua", "192.0.2.1")
		if err != nil {
			t.Fatal(err)
		}
		h.reauth.open(sessionRef(sid))
		return sid
	}
	newSvc := func(parent string) string {
		sid, err := h.st.CreateServicesSession(ctx, "u-admin", "ua", "192.0.2.1", parent)
		if err != nil {
			t.Fatal(err)
		}
		return sid
	}
	gone := func(sid string) bool {
		_, err := h.st.ValidateServicesSession(ctx, sid)
		return errors.Is(err, store.ErrUnauthorized)
	}

	// A services session is no dashboard session.
	dash := newDash()
	svc := newSvc(dash)
	if rec := h.do(t, http.MethodGet, "/api/v1/me", nil, cookieSession(svc)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("services session as burrow_session: %d, want 401", rec.Code)
	}
	if rec := h.do(t, http.MethodGet, "/api/v1/me", nil, cookieSession(dash)); rec.Code != http.StatusOK {
		t.Fatalf("dashboard session: %d", rec.Code)
	}

	// Logout ends the services session it handed off.
	if rec := h.do(t, http.MethodPost, "/api/v1/auth/logout", nil, cookieSession(dash)); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if !gone(svc) {
		t.Fatal("the services session outlived the dashboard logout")
	}

	// Revoking a dashboard session from another one ends its services session.
	me, other := newDash(), newDash()
	otherSvc, mySvc := newSvc(other), newSvc(me)
	var list []sessionResp
	rec := h.do(t, http.MethodGet, "/api/v1/sessions", nil, cookieSession(me))
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	// The list shows the services sessions too, by the hash of their id
	// only, so that one can be revoked on its own.
	ids := map[string]bool{}
	for _, s := range list {
		ids[s.ID] = true
	}
	for _, sid := range []string{me, other, otherSvc, mySvc} {
		if !ids[sessionRef(sid)] {
			t.Errorf("session %s is not listed by its hash (listed: %v)", sessionRef(sid), ids)
		}
	}
	if raw := rec.Body.String(); strings.Contains(raw, otherSvc) || strings.Contains(raw, mySvc) {
		t.Fatalf("the list holds a raw services session id: %s", raw)
	}
	if rec := h.do(t, http.MethodDelete, "/api/v1/sessions/"+sessionRef(other), nil, cookieSession(me)); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if !gone(otherSvc) || gone(mySvc) {
		t.Fatalf("after revoking the other session: its services session gone=%v, mine gone=%v; want true, false", gone(otherSvc), gone(mySvc))
	}

	// Sign out everywhere else: every services session ends, mine too.
	third := newDash()
	thirdSvc := newSvc(third)
	if rec := h.do(t, http.MethodPost, "/api/v1/sessions/revoke-all", nil, cookieSession(me)); rec.Code != http.StatusOK {
		t.Fatalf("revoke all: %d %s", rec.Code, rec.Body)
	}
	if !gone(thirdSvc) || !gone(mySvc) {
		t.Fatalf("after revoke all: third gone=%v, mine gone=%v", gone(thirdSvc), gone(mySvc))
	}
	if rec := h.do(t, http.MethodGet, "/api/v1/me", nil, cookieSession(me)); rec.Code != http.StatusOK {
		t.Fatalf("the kept session: %d", rec.Code)
	}
}
