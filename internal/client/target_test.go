package client

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	ok := map[string]string{
		"3000":                     "127.0.0.1:3000",
		" 3000 ":                   "127.0.0.1:3000",
		"localhost:3000":           "localhost:3000",
		"192.168.1.20:8080":        "192.168.1.20:8080",
		"my-host.lan:80":           "my-host.lan:80",
		"[::1]:3000":               "[::1]:3000",
		"http://localhost:3000":    "localhost:3000",
		"http://localhost:3000/":   "localhost:3000",
		"http://192.168.1.20:8080": "192.168.1.20:8080",
		"http://localhost":         "localhost:80",
		"1":                        "127.0.0.1:1",
		"65535":                    "127.0.0.1:65535",
		"http://[::1]:3000":        "[::1]:3000",
	}
	for in, want := range ok {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("ParseTarget(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	bad := map[string]string{ // input → a word the reason must contain
		"":                           "port",
		"0":                          "port",
		"65536":                      "port",
		"-1":                         "port",
		"abc":                        "port",
		"localhost":                  "port",
		"localhost:":                 "port",
		"localhost:abc":              "port",
		":3000":                      "host",
		"https://localhost:3000":     "HTTPS",
		"http://localhost:3000/api":  "path",
		"http://localhost:3000?x=1":  "path",
		"http://user:pw@localhost:1": "credentials",
		"ftp://localhost:21":         "http",
		"localhost:3000:1":           "port",
		"local host:3000":            "host",
		"+3000":                      "port",
		"03000":                      "port",
		"localhost:+80":              "port",
		"a\nb:80":                    "host",
		"a\x7fb:80":                  "host",
		"http://localhost:0":         "port",
		"http://localhost:65536":     "port",
	}
	_, err := ParseTarget("http://user:pw@localhost:1")
	if err == nil || strings.Contains(err.Error(), "pw") {
		t.Errorf("credentials error must not echo the password: %v", err)
	}
	for in, word := range bad {
		_, err := ParseTarget(in)
		var te *TargetError
		if !errors.As(err, &te) {
			t.Errorf("ParseTarget(%q) err = %v, want *TargetError", in, err)
			continue
		}
		if !strings.Contains(strings.ToLower(te.Error()), strings.ToLower(word)) {
			t.Errorf("ParseTarget(%q) message %q does not mention %q", in, te.Error(), word)
		}
		// The message lists the accepted forms so the user can fix the command without the docs.
		if !strings.Contains(te.Error(), "3000") || !strings.Contains(te.Error(), "localhost:3000") {
			t.Errorf("ParseTarget(%q) message lacks the accepted forms: %q", in, te.Error())
		}
	}
}

func TestDefaultName(t *testing.T) {
	cases := []struct{ host, addr, want string }{
		{"kohns-laptop", "127.0.0.1:3000", "kohns-laptop-3000"},
		{"Kohns-Laptop.local", "localhost:8080", "kohns-laptop-8080"}, // lower-cased, domain part dropped
		{"my_box", "127.0.0.1:1", "my-box-1"},                         // characters outside a-z0-9- become "-"
		{"", "127.0.0.1:3000", "burrow-3000"},                         // no hostname available
		{"---", "127.0.0.1:3000", "burrow-3000"},
		{strings.Repeat("a", 80), "127.0.0.1:3000", strings.Repeat("a", 35) + "-3000"}, // whole name capped at 40
		{"host", "garbage", "host"},                                                    // no port to append
	}
	for _, c := range cases {
		if got := DefaultName(c.host, c.addr); got != c.want {
			t.Errorf("DefaultName(%q, %q) = %q, want %q", c.host, c.addr, got, c.want)
		}
	}
	// The result always satisfies the slug rule, even for a long host and a 5-digit port.
	slug := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
	for _, h := range []string{strings.Repeat("a", 80), strings.Repeat("ab-", 30), "x"} {
		if got := DefaultName(h, "127.0.0.1:65535"); !slug.MatchString(got) {
			t.Errorf("DefaultName(%q) = %q does not satisfy the slug rule", h, got)
		}
	}
	// Stable: the same inputs always give the same name.
	if DefaultName("h", "127.0.0.1:3000") != DefaultName("h", "127.0.0.1:3000") {
		t.Fatal("DefaultName is not deterministic")
	}
}
