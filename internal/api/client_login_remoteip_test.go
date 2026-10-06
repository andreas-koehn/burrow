package api

import (
	"net/http"
	"testing"
)

// The address a sign-in request is stored and counted under is one spelling
// per address: an IPv4 caller on a dual-stack listener is its IPv4 address.
func TestRemoteIP_Normalised(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7:4711":          "203.0.113.7",
		"[::ffff:203.0.113.7]:4711": "203.0.113.7",
		"[2001:DB8:0:0::1]:4711":    "2001:db8::1",
		"[2001:db8::1]:4711":        "2001:db8::1",
		"[::1]:9":                   "::1",
		"203.0.113.7":               "203.0.113.7", // no port, as some middleware leaves it
		"::ffff:203.0.113.7":        "203.0.113.7",
		"not-an-address:80":         "not-an-address",
		"":                          "",
	} {
		if got := remoteIP(&http.Request{RemoteAddr: in}); got != want {
			t.Errorf("remoteIP(%q) = %q, want %q", in, got, want)
		}
	}
}
