package config

import (
	"strings"
	"testing"
)

func TestCleanClientDownloadBase(t *testing.T) {
	if DefaultClientDownloadBase != "https://github.com/andreas-koehn/burrow/releases/download" {
		t.Fatalf("DefaultClientDownloadBase = %q", DefaultClientDownloadBase)
	}
	good := map[string]string{
		"":                               DefaultClientDownloadBase,
		"  ":                             DefaultClientDownloadBase,
		DefaultClientDownloadBase:        DefaultClientDownloadBase,
		DefaultClientDownloadBase + "/":  DefaultClientDownloadBase,
		"https://mirror.example.com":     "https://mirror.example.com",
		"http://assets:8080/dl/burrow//": "http://assets:8080/dl/burrow",
	}
	for in, want := range good {
		got, err := CleanClientDownloadBase(in)
		if err != nil || got != want {
			t.Errorf("CleanClientDownloadBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"ftp://x/y", "//x/y", "x/y", "https://", "https://user:pw@x/y", "https://x/y?z=1", "https://x/y#z",
		"https://x/a b", "https://x/$(id)", "https://x/`id`", "https://x/\"", "https://x/'", "https://x/a\nb",
		"https://x/../y", "https://x/%2e%2e", "javascript:alert(1)", "https://x\\y",
	} {
		if got, err := CleanClientDownloadBase(in); err == nil {
			t.Errorf("CleanClientDownloadBase accepted %q → %q", in, got)
		}
	}
}

// A relay that is reached over HTTPS must not send its clients to a plain
// HTTP download: archive and checksums would both be open to a swap on the
// way. Loopback is the exception (a mirror on the same machine).
func TestClientDownloadBase_PlainHTTPNeedsAPlainRelay(t *testing.T) {
	https := map[string][2]string{
		"secure cookies": {"BURROW_HTTP_SECURE_COOKIES", "true"},
		"acme":           {"BURROW_ACME_DOMAIN", "burrow.example.com"},
	}
	for name, kv := range https {
		t.Run(name, func(t *testing.T) {
			t.Setenv(kv[0], kv[1])
			if name == "acme" {
				t.Setenv("BURROW_ACME_EMAIL", "ops@example.com")
			}
			if _, err := LoadServer(nil); err != nil {
				t.Fatalf("default base: %v", err)
			}
			for _, base := range []string{"http://assets:8080/dl", "http://mirror.example.com", "http://127.evil.example.com/x", "http://localhost.example.com/x"} {
				t.Setenv("BURROW_CLIENT_DOWNLOAD_BASE", base)
				_, err := LoadServer(nil)
				if err == nil || !strings.Contains(err.Error(), "client_download_base") || !strings.Contains(err.Error(), "https") {
					t.Errorf("%s: err = %v, want one naming client_download_base and https", base, err)
				}
			}
			for _, base := range []string{"http://127.0.0.1:9000/dl", "http://localhost/dl", "http://127.8.9.10/dl", "https://mirror.example.com"} {
				t.Setenv("BURROW_CLIENT_DOWNLOAD_BASE", base)
				if _, err := LoadServer(nil); err != nil {
					t.Errorf("%s: %v", base, err)
				}
			}
		})
	}
	t.Run("native tls", func(t *testing.T) {
		over := map[string]any{"http_tls_cert": "c.pem", "http_tls_key": "k.pem", "client_download_base": "http://assets/dl"}
		if _, err := LoadServer(over); err == nil || !strings.Contains(err.Error(), "client_download_base") {
			t.Errorf("err = %v, want one naming client_download_base", err)
		}
	})
	// A relay served over plain HTTP may point at a plain HTTP mirror.
	t.Run("plain relay", func(t *testing.T) {
		t.Setenv("BURROW_CLIENT_DOWNLOAD_BASE", "http://assets:8080/dl")
		if _, err := LoadServer(nil); err != nil {
			t.Errorf("plain relay, plain base: %v", err)
		}
	})
}
