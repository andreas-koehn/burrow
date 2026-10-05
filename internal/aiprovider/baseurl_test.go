package aiprovider

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestValidateBaseURL(t *testing.T) {
	good := []string{
		"https://openrouter.ai/api/v1",
		"https://api.z.ai/api/coding/paas/v4",
		"https://api.z.ai/api/paas/v4/",
		"https://example.com",
		"https://example.com:8443/v1",
	}
	bad := []string{
		"", "openrouter.ai/api/v1", "http://openrouter.ai/api/v1", "ftp://x/y",
		"https://", "https://user:pw@example.com/v1", "https://example.com/v1?x=1",
		"https://example.com/v1#frag", "https://exa mple.com", "//example.com/v1",
		"https://user@example.com/v1", "https://example.com/v1?", "HTTP://example.com/v1",
		"https://example.com/a/../b", "https://example.com/v1/.", "https://example.com/%2e%2e/b",
		"https://example.com/v1 ", " https://example.com/v1", "https://example.com/v1\t", "https://example.com/v1%20x",
		"https://example.com:0/v1", "https://example.com:99999/v1", "https://example.com:/v1",
		"https://example.com/v1#",
	}
	for _, s := range good {
		if _, err := ValidateBaseURL(s); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v, want ok", s, err)
		}
	}
	for _, s := range bad {
		if _, err := ValidateBaseURL(s); !errors.Is(err, ErrInvalidBaseURL) {
			t.Errorf("ValidateBaseURL(%q) = %v, want ErrInvalidBaseURL", s, err)
		}
	}
}

// A rejected base URL may carry a password or a key in its query; the error is
// shown to the operator and logged, so it must not repeat the input.
func TestValidateBaseURL_ErrorDoesNotEchoInput(t *testing.T) {
	for _, s := range []string{
		"https://user:s3cretpw@example.com/v1",
		"https://example.com/v1?key=s3cretpw",
		"https://s3cretpw@exa mple.com",
		"https://example.com/%zz-s3cretpw",
	} {
		_, err := ValidateBaseURL(s)
		if err == nil {
			t.Errorf("ValidateBaseURL(%q) accepted", s)
			continue
		}
		if strings.Contains(err.Error(), "s3cretpw") {
			t.Errorf("error repeats the input: %v", err)
		}
	}
}

func TestPublicIP(t *testing.T) {
	public := []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111", "::ffff:8.8.8.8", "172.32.0.1", "100.128.0.1"}
	private := []string{
		"127.0.0.1", "127.255.255.254", "10.0.0.5", "172.16.3.4", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "169.254.0.1",
		"100.64.0.1", "100.127.255.255", "0.0.0.0", "0.1.2.3", "224.0.0.1", "239.255.255.250", "255.255.255.255",
		"::1", "::", "fe80::1", "febf::1", "fc00::1", "fd12:3456::1", "ff02::1", "ff05::2",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:172.16.0.1", "::ffff:192.168.0.1",
		"::ffff:169.254.169.254", "::ffff:100.64.0.1", "::ffff:0.0.0.0", "::ffff:224.0.0.1",
		"64:ff9b::7f00:1", "64:ff9b:1::a00:1", "2002:7f00:1::1", "2001:0:7f00:1::1",
		"::7f00:1", "::a00:1", "::ffff:0:7f00:1", "::ffff:0:a00:1", "fec0::1", "feff::1",
	}
	for _, s := range public {
		if !PublicIP(net.ParseIP(s)) {
			t.Errorf("PublicIP(%s) = false, want true", s)
		}
	}
	for _, s := range private {
		if PublicIP(net.ParseIP(s)) {
			t.Errorf("PublicIP(%s) = true, want false", s)
		}
	}
	if PublicIP(nil) {
		t.Error("PublicIP(nil) = true")
	}
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, len(ips))
	for i, s := range ips {
		out[i] = net.IPAddr{IP: net.ParseIP(s)}
	}
	return out, nil
}

func TestCheckHostPublic(t *testing.T) {
	r := fakeResolver{
		"ok.example":    {"93.184.216.34"},
		"mixed.example": {"93.184.216.34", "10.0.0.1"}, // one private answer is enough to refuse
		"int.example":   {"192.168.1.10"},
		"empty.example": {},
	}
	ctx := context.Background()
	if err := CheckHostPublic(ctx, r, "ok.example"); err != nil {
		t.Errorf("ok.example: %v", err)
	}
	for _, h := range []string{"mixed.example", "int.example", "127.0.0.1", "[::1]", "localhost.invalid", "empty.example", "::ffff:10.0.0.1"} {
		if err := CheckHostPublic(ctx, r, h); err == nil {
			t.Errorf("CheckHostPublic(%s) = nil, want an error", h)
		}
	}
	if err := CheckHostPublic(ctx, r, "int.example"); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("int.example err = %v, want ErrBlockedAddress", err)
	}
}
