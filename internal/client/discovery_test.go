package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/version"
)

const discoveryJSON = `{"control":"burrow.example.com:7000","version":"0.6.0","min_client_version":"0.6.0","protocol_version":1}`

// relayStub is an HTTPS server standing in for a relay's dashboard origin.
func relayStub(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func discover(t *testing.T, srv *httptest.Server) (Discovery, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Discover(ctx, srv.Client(), srv.URL)
}

func TestDiscover_OK(t *testing.T) {
	var gotPath, gotUA, gotMethod, gotAuth string
	srv := relayStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotUA, gotMethod, gotAuth = r.URL.Path, r.Header.Get("User-Agent"), r.Method, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(discoveryJSON))
	})
	d, err := discover(t, srv)
	if err != nil {
		t.Fatal(err)
	}
	if d.Control != "burrow.example.com:7000" || d.Version != "0.6.0" || d.MinClientVersion != "0.6.0" || d.ProtocolVersion != 1 {
		t.Fatalf("%+v", d)
	}
	if d.Date.IsZero() {
		t.Fatal("the Date header was not kept")
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/client/discovery" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	if gotUA != "burrow/"+version.Version {
		t.Fatalf("User-Agent %q", gotUA)
	}
	if gotAuth != "" {
		t.Fatal("discovery sent an Authorization header")
	}
	// A trailing slash on the relay address is the same relay.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Discover(ctx, srv.Client(), srv.URL+"/"); err != nil || gotPath != "/api/v1/client/discovery" {
		t.Fatalf("with a trailing slash: %v, path %q", err, gotPath)
	}
}

// A relay from before discovery answers 404, as JSON from its API router.
func TestDiscover_NoDiscovery(t *testing.T) {
	srv := relayStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	d, err := discover(t, srv)
	if !errors.Is(err, ErrNoDiscovery) {
		t.Fatalf("err = %v, want ErrNoDiscovery", err)
	}
	if d.Date.IsZero() {
		t.Fatal("the Date header of a 404 is still the relay's clock")
	}
	if d.Control != "" {
		t.Fatalf("control %q from a 404", d.Control)
	}
}

func TestDiscover_NotARelay(t *testing.T) {
	cases := map[string]struct {
		ctype, body string
	}{
		"html from an SPA catch-all or a proxy": {"text/html", "<!doctype html><html><body>Welcome</body></html>"},
		"invalid json":                          {"application/json", `{"control":`},
		"json of another shape":                 {"application/json", `["a","b"]`},
		"json without a control endpoint":       {"application/json", `{"hello":"world"}`},
		"empty":                                 {"application/json", ``},
		"trailing data":                         {"application/json", discoveryJSON + `{"control":"evil.example.com:1"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := relayStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				_, _ = w.Write([]byte(tc.body))
			})
			d, err := discover(t, srv)
			if !errors.Is(err, ErrNotARelay) {
				t.Fatalf("err = %v, want ErrNotARelay", err)
			}
			if !strings.Contains(err.Error(), "does not look like a Burrow relay") {
				t.Fatalf("message %q", err.Error())
			}
			if d.Control != "" {
				t.Fatalf("control %q from an answer that is not a discovery document", d.Control)
			}
		})
	}
}

func TestDiscover_ControlIsValidated(t *testing.T) {
	bad := []string{
		"burrow.example.com", "burrow.example.com:", ":7000", "burrow.example.com:0", "burrow.example.com:65536",
		"burrow.example.com:http", "https://burrow.example.com:7000", "burrow.example.com:7000/path",
		"user@burrow.example.com:7000", "burrow example.com:7000", "burrow.example.com:7000\n", "bur\trow:7000",
		"-leading.example.com:7000", "a..b:7000", "::1:7000", "[::1]", "[not-an-ip]:7000",
		strings.Repeat("a", 254) + ":7000", "bürrow.example.com:7000",
	}
	for _, c := range bad {
		if err := ValidateControl(c); err == nil {
			t.Errorf("ValidateControl(%q) accepted it", c)
		}
	}
	good := []string{"burrow.example.com:7000", "relay:1", "127.0.0.1:7000", "[::1]:7000", "[2001:db8::1]:65535", "Burrow.Example.COM:7000", "xn--brrow-kva.example.com:7000"}
	for _, c := range good {
		if err := ValidateControl(c); err != nil {
			t.Errorf("ValidateControl(%q): %v", c, err)
		}
	}

	srv := relayStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"control":"https://evil.example.com/x","version":"0.6.0","min_client_version":"","protocol_version":1}`))
	})
	d, err := discover(t, srv)
	if !errors.Is(err, ErrNotARelay) || d.Control != "" {
		t.Fatalf("err = %v, control %q", err, d.Control)
	}
	// What the relay sent is not repeated: it is not to be trusted.
	if strings.Contains(err.Error(), "evil.example.com") {
		t.Fatalf("message repeats the answer: %q", err.Error())
	}
}

func TestDiscover_RedirectIsNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := relayStub(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"control":"evil.example.com:7000","version":"0.6.0","min_client_version":"","protocol_version":1}`))
	})
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		srv := relayStub(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/api/v1/client/discovery", code)
		})
		// A client that trusts both servers: only the redirect rule stops it.
		d, err := discover(t, srv)
		var se *DiscoveryStatusError
		if !errors.As(err, &se) || se.Status != code {
			t.Fatalf("%d: err = %v, want a DiscoveryStatusError", code, err)
		}
		if d.Control != "" {
			t.Fatalf("%d: control %q taken from another host", code, d.Control)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the redirect target was asked %d times", n)
	}
	// The caller's own client keeps its redirect rule.
	c := other.Client()
	if c.CheckRedirect != nil {
		t.Fatal("test setup: the client has a redirect rule")
	}
	if _, err := Discover(context.Background(), c, other.URL); err != nil || c.CheckRedirect != nil {
		t.Fatalf("err %v; the caller's client was changed: %v", err, c.CheckRedirect != nil)
	}
}

func TestDiscover_OtherStatus(t *testing.T) {
	srv := relayStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(discoveryJSON))
	})
	d, err := discover(t, srv)
	var se *DiscoveryStatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway || d.Control != "" {
		t.Fatalf("err = %v, control %q", err, d.Control)
	}
	if errors.Is(err, ErrNoDiscovery) || errors.Is(err, ErrNotARelay) {
		t.Fatal("a 502 is neither an older relay nor another kind of server")
	}
}

func TestDiscover_HTTPSOnly(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(discoveryJSON))
	}))
	defer plain.Close()
	for _, u := range []string{
		plain.URL, strings.Replace(plain.URL, "http://", "", 1), "ftp://burrow.example.com", "",
		"https://", "https://user:pw@burrow.example.com", "https://burrow.example.com/dashboard",
		"https://burrow.example.com?x=1", "https://burrow.example.com#f",
	} {
		if _, err := Discover(context.Background(), plain.Client(), u); err == nil {
			t.Errorf("Discover(%q) did not refuse the address", u)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("a request went out without https")
	}
}

func TestDiscover_LargeResponseIsRefused(t *testing.T) {
	pad := strings.Repeat(" ", maxDiscoveryBody)
	srv := relayStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(pad + discoveryJSON))
	})
	d, err := discover(t, srv)
	if !errors.Is(err, ErrNotARelay) || d.Control != "" {
		t.Fatalf("err = %v, control %q", err, d.Control)
	}
	// Up to the limit it is read.
	fits := strings.Repeat(" ", maxDiscoveryBody-len(discoveryJSON))
	srv = relayStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fits + discoveryJSON))
	})
	if _, err := discover(t, srv); err != nil {
		t.Fatalf("a response of exactly the limit: %v", err)
	}
	if maxDiscoveryBody != 64<<10 {
		t.Fatalf("limit = %d, want 64 KiB", maxDiscoveryBody)
	}
}

// A relay that accepts the request and then says nothing: the call ends by
// itself, without a deadline from the caller.
func TestDiscover_Timeout(t *testing.T) {
	if DiscoveryTimeout != 10*time.Second {
		t.Fatalf("DiscoveryTimeout = %v, want 10s", DiscoveryTimeout)
	}
	release := make(chan struct{})
	srv := relayStub(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	old := discoveryTimeout
	discoveryTimeout = 200 * time.Millisecond
	defer func() { discoveryTimeout = old }()

	start := time.Now()
	_, err := Discover(context.Background(), srv.Client(), srv.URL)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	// A body that never ends is cut off as well.
	srv2 := relayStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"control":`))
		w.(http.Flusher).Flush()
		<-release
	})
	start = time.Now()
	if _, err := Discover(context.Background(), srv2.Client(), srv2.URL); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("slow body: err = %v after %v", err, time.Since(start))
	}
}

func TestDiscover_CertificateErrorIsKept(t *testing.T) {
	srv := relayStub(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(discoveryJSON)) })
	// A client that does not know the test CA.
	_, err := Discover(context.Background(), &http.Client{}, srv.URL)
	if err == nil || errors.Is(err, ErrNotARelay) || errors.Is(err, ErrNoDiscovery) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the certificate problem is lost: %v", err)
	}
}

func TestSameEndpoint(t *testing.T) {
	if !SameEndpoint("Burrow.Example.com.:7000", "burrow.example.com:7000") || SameEndpoint("a:7000", "a:7001") || SameEndpoint("", "") {
		t.Fatal("SameEndpoint")
	}
}
