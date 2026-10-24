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
		"https://localhost:3000":   "localhost:3000",
		"https://localhost":        "localhost:443",
		"h2c://localhost:50051":    "localhost:50051",
		"h2c://127.0.0.1:50051/":   "127.0.0.1:50051",
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
		"http://localhost:3000/api":  "path",
		"http://localhost:3000?x=1":  "path",
		"http://user:pw@localhost:1": "credentials",
		"ftp://localhost:21":         "http",
		"h2://localhost:3000":        "http",
		"h2c://localhost:3000/x":     "path",
		"https://localhost:3000/x":   "path",
		"h2c://u:p@localhost:3000":   "credentials",
		"localhost:3000:1":           "port",
		"local host:3000":            "host",
		"pw@localhost:3000":          "credentials",
		"user:pw@localhost:3000":     "credentials",
		"a\u00a0b:80":                "host",
		"a b:80":                     "host",
		"-h:80":                      "host",
		"+3000":                      "port",
		"03000":                      "port",
		"localhost:+80":              "port",
		"a\nb:80":                    "host",
		"a\x7fb:80":                  "host",
		"http://localhost:0":         "port",
		"http://localhost:65536":     "port",
	}
	for _, in := range []string{
		"user:s3cretpw@localhost:1", "user:s3cretpw@localhost", "http://user:s3cretpw@localhost:abc",
		"http://user:s3cr etpw@localhost:1", "http://user:s3cretpw@local host:1", "http://user:s3cretpw@[::1",
		"http://user:s3cretpw@localhost:1", "https://user:s3cretpw@localhost:1", "http://user:s3cr@etpw@localhost:1",
		"http://user:s3cr:etpw@localhost:1", "user:s3cr@etpw@localhost:3000", "user:s3cr:etpw@localhost:3000",
	} {
		_, err := ParseTarget(in)
		if err == nil {
			t.Errorf("ParseTarget(%q) accepted credentials", in)
			continue
		}
		for _, leak := range []string{"s3cretpw", "etpw", "s3cr"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("ParseTarget(%q) echoes the password (%q): %v", in, leak, err)
			}
		}
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
	// Odd ports: a non-numeric or over-long port counts as no port.
	for _, c := range []struct{ host, addr, want string }{
		{"x", "h:HTTP", "burrow"},
		{"host", "h:HTTP", "host"},
		{"host", "h:" + strings.Repeat("9", 50), "host"},
		{"a", "garbage", "burrow"},
		{"ab", "garbage", "burrow"},
		{strings.Repeat("a", 33) + "-bcdef", "127.0.0.1:65535", strings.Repeat("a", 33) + "-65535"},
	} {
		got := DefaultName(c.host, c.addr)
		if got != c.want || !slug.MatchString(got) {
			t.Errorf("DefaultName(%q, %q) = %q, want %q", c.host, c.addr, got, c.want)
		}
	}
	// Stable: the same inputs always give the same name.
	first, second := DefaultName("h", "127.0.0.1:3000"), DefaultName("h", "127.0.0.1:3000")
	if first != second {
		t.Fatal("DefaultName is not deterministic")
	}
}

func TestDefaultNameAlwaysValidSlug(t *testing.T) {
	slug := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
	hosts := []string{"", "x", "AB", "HOST", "---", "...", "!!!", "héllo-wörld", "日本語", strings.Repeat("a", 200),
		"host.", "host-", "-host", "a.b.c.d", "my host", strings.Repeat("a-", 40), "UPPER.Example.COM", "1", "a_b"}
	addrs := []string{"", "127.0.0.1:3000", "localhost:1", "[::1]:65535", "garbage", "h:", "h:HTTP", "h:" + strings.Repeat("9", 60), ":80", "h:0080", "h:-1"}
	for _, h := range hosts {
		for _, a := range addrs {
			got := DefaultName(h, a)
			if !slug.MatchString(got) {
				t.Errorf("DefaultName(%q, %q) = %q is not a valid slug", h, a, got)
			}
			if again := DefaultName(h, a); again != got {
				t.Errorf("DefaultName(%q, %q) not stable: %q vs %q", h, a, got, again)
			}
		}
	}
}

func TestParseTargetProtocol(t *testing.T) {
	for in, want := range map[string][3]string{ // addr, protocol
		"3000":                   {"127.0.0.1:3000", "", ""},
		"localhost:3000":         {"localhost:3000", "", ""},
		"http://localhost:3000":  {"localhost:3000", "", ""},
		"https://localhost:8443": {"localhost:8443", "https", ""},
		"https://localhost":      {"localhost:443", "https", ""},
		"h2c://localhost:50051":  {"localhost:50051", "h2c", ""},
		"h2c://[::1]:50051":      {"[::1]:50051", "h2c", ""},
	} {
		addr, p, err := ParseTargetProtocol(in)
		if err != nil || addr != want[0] || p != want[1] {
			t.Errorf("ParseTargetProtocol(%q) = %q, %q, %v; want %q, %q", in, addr, p, err, want[0], want[1])
		}
	}
	if _, _, err := ParseTargetProtocol("h2://localhost:1"); err == nil {
		t.Error("h2:// accepted")
	}
	// The accepted forms name the new schemes.
	_, err := ParseTarget("ftp://x:1")
	if err == nil || !strings.Contains(err.Error(), "https://") || !strings.Contains(err.Error(), "h2c://") {
		t.Errorf("error does not list https:// and h2c://: %v", err)
	}
}

func TestResolveUpstream(t *testing.T) {
	for _, c := range []struct {
		name, scheme, flag string
		grpc               bool
		want               string
		bad                string // part of the error; "" = none
	}{
		{"nothing chosen", "", "", false, "", ""},
		{"flag alone", "", "h2c", false, "h2c", ""},
		{"flag http1 is a choice", "", "http1", false, "http1", ""},
		{"plain scheme and flag", "", "h2", false, "h2", ""},
		{"https scheme", "https", "", false, "https", ""},
		{"https scheme with h2", "https", "h2", false, "h2", ""},
		{"https scheme with https", "https", "https", false, "https", ""},
		{"https scheme against h2c", "https", "h2c", false, "", "https://"},
		{"https scheme against http1", "https", "http1", false, "", "https://"},
		{"h2c scheme", "h2c", "", false, "h2c", ""},
		{"h2c scheme with h2c", "h2c", "h2c", false, "h2c", ""},
		{"h2c scheme against h2", "h2c", "h2", false, "", "h2c://"},
		{"unknown flag", "", "grpc", false, "", "http1, https, h2c or h2"},
		{"grpc bare", "", "", true, "h2c", ""},
		{"grpc https", "https", "", true, "h2", ""},
		{"grpc h2c", "h2c", "", true, "h2c", ""},
		{"grpc with h2", "", "h2", true, "h2", ""},
		{"grpc with http1", "", "http1", true, "", "h2c or h2"},
		{"grpc with https", "", "https", true, "", "h2c or h2"},
	} {
		got, err := ResolveUpstream(c.scheme, c.flag, c.grpc)
		if c.bad == "" && (err != nil || got != c.want) {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
		if c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)) {
			t.Errorf("%s: %q, %v; want an error with %q", c.name, got, err, c.bad)
		}
	}
}

// ParseTarget is ParseTargetProtocol without the protocol.
func ParseTarget(s string) (string, error) {
	addr, _, err := ParseTargetProtocol(s)
	return addr, err
}
