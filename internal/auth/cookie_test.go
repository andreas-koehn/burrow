package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
