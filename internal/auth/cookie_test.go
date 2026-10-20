package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCookieName(t *testing.T) {
	if got := CookieName(SessionCookie, true); got != "__Host-burrow_session" {
		t.Errorf("secure: %q", got)
	}
	if got := CookieName(SessionCookie, false); got != "burrow_session" {
		t.Errorf("plain: %q", got)
	}
}

func cookieReq(header string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if header != "" {
		r.Header.Set("Cookie", header)
	}
	return r
}

// The __Host- cookie wins over a bare one of the same base name: a bare
// cookie may come from a sibling host (Domain=), a __Host- one cannot.
func TestReadCookie_HostPrefixWins(t *testing.T) {
	cases := []struct {
		header, value, name string
	}{
		{"burrow_session=planted; __Host-burrow_session=real", "real", "__Host-burrow_session"},
		{"__Host-burrow_session=real; burrow_session=planted", "real", "__Host-burrow_session"},
		{"burrow_session=old", "old", "burrow_session"}, // accepted for one release
		{"__Host-burrow_session=; burrow_session=old", "old", "burrow_session"},
		{"other=1", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		v, name := ReadCookie(cookieReq(c.header), SessionCookie)
		if v != c.value || name != c.name {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", c.header, v, name, c.value, c.name)
		}
	}
}

// The CSRF cookie is read under the name family of the session cookie that
// authenticated the request: a bare (plantable) CSRF cookie never pairs with
// a __Host- session.
func TestReadCookieLike(t *testing.T) {
	r := cookieReq("__Host-burrow_session=s; burrow_csrf=planted")
	if v := ReadCookieLike(r, CSRFCookie, "__Host-burrow_session"); v != "" {
		t.Errorf("a bare CSRF cookie paired with a __Host- session: %q", v)
	}
	r = cookieReq("__Host-burrow_session=s; burrow_csrf=planted; __Host-burrow_csrf=real")
	if v := ReadCookieLike(r, CSRFCookie, "__Host-burrow_session"); v != "real" {
		t.Errorf("got %q, want real", v)
	}
	r = cookieReq("burrow_session=s; burrow_csrf=old")
	if v := ReadCookieLike(r, CSRFCookie, "burrow_session"); v != "old" {
		t.Errorf("legacy pair: got %q, want old", v)
	}
}

func TestIsHostCookie(t *testing.T) {
	if !IsHostCookie("__Host-burrow_session") || IsHostCookie("burrow_session") {
		t.Fatal("IsHostCookie")
	}
	if BaseCookieName("__Host-burrow_svc_nonce_ab") != "burrow_svc_nonce_ab" || BaseCookieName("x") != "x" {
		t.Fatal("BaseCookieName")
	}
}

type memSettings map[string]string

func (m memSettings) GetSettings(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

func (m memSettings) SaveSettings(_ context.Context, kv map[string]string) error {
	for k, v := range kv {
		m[k] = v
	}
	return nil
}

// The bare-cookie cutoff is minted at the first start and kept: later starts
// read the stored one.
func TestEnsureBareCookieCutoff(t *testing.T) {
	ctx := context.Background()
	ss := memSettings{}
	before := time.Now().Add(-time.Second)
	c1, err := EnsureBareCookieCutoff(ctx, ss)
	if err != nil {
		t.Fatal(err)
	}
	if c1.Before(before) || c1.After(time.Now().Add(time.Second)) {
		t.Fatalf("cutoff %v is not now", c1)
	}
	if ss[BareCookieCutoffKey] == "" {
		t.Fatalf("not stored: %v", ss)
	}
	ss[BareCookieCutoffKey] = "2026-10-01T12:00:00Z"
	c2, err := EnsureBareCookieCutoff(ctx, ss)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC); !c2.Equal(want) {
		t.Fatalf("stored cutoff not kept: %v", c2)
	}
	ss[BareCookieCutoffKey] = "garbage"
	if _, err := EnsureBareCookieCutoff(ctx, ss); err == nil {
		t.Fatal("a broken stored cutoff was accepted")
	}
}
