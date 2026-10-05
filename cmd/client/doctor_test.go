package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
)

const fakeSecret = "bur_SECRETSECRETSECRET_9f3a"

var epoch = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// goodDeps is a doctorDeps whose every check passes without touching anything
// real. Tests replace the part they are about.
func goodDeps(t *testing.T) doctorDeps {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := client.SaveUserConfig(cfg, client.UserConfig{Relay: "https://relay.example.com", Control: "relay.example.com:7000", Token: fakeSecret, TokenName: "laptop"}); err != nil {
		t.Fatal(err)
	}
	return doctorDeps{
		resolve: func() (client.Credentials, error) {
			return client.Credentials{Control: "relay.example.com:7000", Token: fakeSecret, TokenName: "laptop", Relay: "https://relay.example.com", Source: client.SourceUserConfig}, nil
		},
		configPath: cfg,
		lookup:     func(context.Context, string) ([]string, error) { return []string{"192.0.2.1"}, nil },
		http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return discoveryResponse(200, `{"control":"relay.example.com:7000","version":"v1.2.0","min_client_version":"v1.0.0","protocol_version":1}`, epoch), nil
		})},
		dialTCP:      func(context.Context, string) (net.Conn, error) { c, _ := net.Pipe(); return c, nil },
		tlsHandshake: func(context.Context, net.Conn, string) error { return nil },
		checkAuth: func(context.Context, client.Options) (client.AuthResult, error) {
			return client.AuthResult{OK: true}, nil
		},
		now:           func() time.Time { return epoch },
		services:      func() (string, []client.TunnelSpec, bool) { return "", nil, false },
		probe:         func(context.Context, string) bool { return true },
		clientVersion: "v1.2.0",
		timeout:       time.Second,
		total:         5 * time.Second,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func discoveryResponse(code int, body string, date time.Time) *http.Response {
	h := http.Header{}
	if !date.IsZero() {
		h.Set("Date", date.Format(http.TimeFormat))
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func find(t *testing.T, rs []checkResult, name string) checkResult {
	t.Helper()
	for _, r := range rs {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("no check %q in %+v", name, rs)
	return checkResult{}
}

func TestDoctor_AllPass(t *testing.T) {
	rs := runDoctor(context.Background(), goodDeps(t))
	if len(rs) != 8 {
		t.Fatalf("%d checks", len(rs))
	}
	for _, r := range rs {
		want := "pass"
		if r.name == checkLocal {
			want = "skip" // no burrow.yaml
		}
		if r.status != want {
			t.Errorf("%s: %s (%s)", r.name, r.status, r.detail)
		}
	}
}

func TestDoctor_Config(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits")
	}
	d := goodDeps(t)
	if err := os.Chmod(d.configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	r := find(t, runDoctor(context.Background(), d), checkConfig)
	if r.status != "warn" || r.fix != "chmod 600 "+d.configPath {
		t.Fatalf("%+v", r)
	}
	_ = os.Chmod(d.configPath, 0o600)
	if r := find(t, runDoctor(context.Background(), d), checkConfig); r.status != "pass" {
		t.Fatalf("%+v", r)
	}
}

func TestDoctor_NotSignedInSkipsTheRest(t *testing.T) {
	d := goodDeps(t)
	d.resolve = func() (client.Credentials, error) { return client.Credentials{}, client.ErrNotSignedIn }
	d.checkAuth = func(context.Context, client.Options) (client.AuthResult, error) {
		t.Fatal("authenticated without credentials")
		return client.AuthResult{}, nil
	}
	rs := runDoctor(context.Background(), d)
	if r := find(t, rs, checkConfig); r.status != "fail" || !strings.Contains(r.detail, "Not signed in") || r.fix != "burrow login <relay>" {
		t.Fatalf("%+v", r)
	}
	for _, n := range []string{checkResolve, checkDiscovery, checkControl, checkToken, checkVersions, checkClock} {
		if r := find(t, rs, n); r.status != "skip" || r.detail == "" {
			t.Errorf("%s: %+v", n, r)
		}
	}
	if exitCodeOf(rs) != exitNotSignedIn {
		t.Fatalf("exit %d", exitCodeOf(rs))
	}
}

// Ruling 3: a stored token is not tried against another relay.
func TestDoctor_OtherRelayIsNotSignedIn(t *testing.T) {
	d := goodDeps(t)
	d.resolve = func() (client.Credentials, error) {
		return client.Credentials{}, &client.RelayMismatchError{Control: "other.example.com:7000", StoredControl: "relay.example.com:7000"}
	}
	d.checkAuth = func(context.Context, client.Options) (client.AuthResult, error) {
		t.Fatal("the stored token was sent to another relay")
		return client.AuthResult{}, nil
	}
	rs := runDoctor(context.Background(), d)
	r := find(t, rs, checkConfig)
	if r.status != "fail" || !strings.Contains(r.detail, "not signed in to other.example.com:7000") {
		t.Fatalf("%+v", r)
	}
}

func TestDoctor_Resolve(t *testing.T) {
	d := goodDeps(t)
	d.lookup = func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	rs := runDoctor(context.Background(), d)
	r := find(t, rs, checkResolve)
	if r.status != "fail" || !strings.Contains(r.detail, "relay.example.com") {
		t.Fatalf("%+v", r)
	}
	if exitCodeOf(rs) != exitUnreachable {
		t.Fatalf("exit %d", exitCodeOf(rs))
	}
	if r := find(t, runDoctor(context.Background(), goodDeps(t)), checkResolve); !strings.Contains(r.detail, "192.0.2.1") {
		t.Fatalf("%+v", r)
	}
}

func TestDoctor_Discovery(t *testing.T) {
	d := goodDeps(t)
	d.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return discoveryResponse(404, "", epoch), nil
	})}
	r := find(t, runDoctor(context.Background(), d), checkDiscovery)
	if r.status != "warn" || !strings.Contains(r.detail, "older and has no discovery; using relay.example.com:7000") {
		t.Fatalf("%+v", r)
	}
}

func TestDoctor_DiscoveryCertificateNotTrusted(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	d := goodDeps(t)
	d.resolve = func() (client.Credentials, error) {
		return client.Credentials{Control: "relay.example.com:7000", Token: fakeSecret, Relay: srv.URL}, nil
	}
	d.http = &http.Client{} // trusts only the system roots
	rs := runDoctor(context.Background(), d)
	r := find(t, rs, checkDiscovery)
	if r.status != "fail" || !strings.Contains(r.fix, "--cacert") {
		t.Fatalf("%+v", r)
	}
	if exitCodeOf(rs) != exitUnreachable {
		t.Fatalf("exit %d", exitCodeOf(rs))
	}
}

func TestDoctor_DiscoveryTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	d := goodDeps(t)
	d.resolve = func() (client.Credentials, error) {
		return client.Credentials{Control: "relay.example.com:7000", Token: fakeSecret, Relay: srv.URL}, nil
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	d.http = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	d.timeout = 200 * time.Millisecond
	start := time.Now()
	r := find(t, runDoctor(context.Background(), d), checkDiscovery)
	if r.status != "fail" || !strings.Contains(r.detail, "no answer") || time.Since(start) > 3*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
}

func TestDoctor_Control(t *testing.T) {
	d := goodDeps(t)
	d.dialTCP = func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	}
	rs := runDoctor(context.Background(), d)
	r := find(t, rs, checkControl)
	if r.status != "fail" || !strings.Contains(r.fix, "port 7000 is open on the relay's firewall") {
		t.Fatalf("%+v", r)
	}
	if exitCodeOf(rs) != exitUnreachable {
		t.Fatalf("exit %d", exitCodeOf(rs))
	}
	// TCP works, TLS does not.
	d = goodDeps(t)
	d.tlsHandshake = func(context.Context, net.Conn, string) error { return errors.New("tls: handshake failure") }
	if r := find(t, runDoctor(context.Background(), d), checkControl); r.status != "fail" || !strings.Contains(r.detail, "TLS") {
		t.Fatalf("%+v", r)
	}
	// An untrusted certificate names its issuer.
	d.tlsHandshake = func(context.Context, net.Conn, string) error {
		return x509.UnknownAuthorityError{Cert: &x509.Certificate{}}
	}
	if r := find(t, runDoctor(context.Background(), d), checkControl); r.status != "fail" || !strings.Contains(r.fix, "--cacert") {
		t.Fatalf("%+v", r)
	}
}

func TestDoctor_Token(t *testing.T) {
	d := goodDeps(t)
	d.checkAuth = func(context.Context, client.Options) (client.AuthResult, error) {
		return client.AuthResult{OK: false, Error: "invalid token"}, nil
	}
	rs := runDoctor(context.Background(), d)
	r := find(t, rs, checkToken)
	if r.status != "fail" || r.fix != "burrow login <relay>" {
		t.Fatalf("%+v", r)
	}
	if exitCodeOf(rs) != exitTokenRejected {
		t.Fatalf("exit %d", exitCodeOf(rs))
	}
	d.checkAuth = func(context.Context, client.Options) (client.AuthResult, error) {
		return client.AuthResult{}, errors.New("dial: connection refused")
	}
	rs = runDoctor(context.Background(), d)
	if r := find(t, rs, checkToken); r.status != "fail" || exitCodeOf(rs) != exitUnreachable {
		t.Fatalf("%+v exit %d", r, exitCodeOf(rs))
	}
}

func TestDoctor_Versions(t *testing.T) {
	cases := []struct {
		name, client, relay, min, want, in string
	}{
		{"equal", "v1.2.0", "v1.2.0", "v1.0.0", "pass", ""},
		{"older than relay", "v1.1.0", "v1.2.0", "v1.0.0", "warn", "burrow update"},
		{"older than minimum", "v0.9.0", "v1.2.0", "v1.0.0", "fail", "burrow update"},
		{"newer than relay", "v1.3.0", "v1.2.0", "v1.0.0", "pass", ""},
		{"develop", "develop", "v1.2.0", "v1.0.0", "skip", ""},
		{"no leading v", "1.2.0", "v1.2.0", "1.0.0", "pass", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := goodDeps(t)
			d.clientVersion = tc.client
			d.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return discoveryResponse(200, `{"control":"relay.example.com:7000","version":"`+tc.relay+`","min_client_version":"`+tc.min+`"}`, epoch), nil
			})}
			rs := runDoctor(context.Background(), d)
			r := find(t, rs, checkVersions)
			if r.status != tc.want || !strings.Contains(r.fix+r.detail, tc.in) {
				t.Fatalf("%+v", r)
			}
			if tc.want == "fail" && exitCodeOf(rs) != exitClientTooOld {
				t.Fatalf("exit %d", exitCodeOf(rs))
			}
		})
	}
	// The relay's version is unknown: skipped, not passed.
	d := goodDeps(t)
	d.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return discoveryResponse(404, "", epoch), nil
	})}
	if r := find(t, runDoctor(context.Background(), d), checkVersions); r.status != "skip" {
		t.Fatalf("%+v", r)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v1.2.3", "v1.2.3", 0, true}, {"1.2.3", "v1.2.4", -1, true}, {"v1.10.0", "v1.9.0", 1, true},
		{"develop", "v1.0.0", 0, false}, {"v1.2.3-5-gabc", "v1.2.3", 0, false}, {"abc1234", "v1.0.0", 0, false},
		{"v1.2", "v1.2.0", 0, false}, {"", "v1.0.0", 0, false},
	} {
		c, ok := compareVersions(tc.a, tc.b)
		if c != tc.cmp || ok != tc.ok {
			t.Errorf("%q vs %q: %d %v", tc.a, tc.b, c, ok)
		}
	}
}

func TestDoctor_Clock(t *testing.T) {
	d := goodDeps(t)
	d.now = func() time.Time { return epoch.Add(5 * time.Minute) }
	r := find(t, runDoctor(context.Background(), d), checkClock)
	if r.status != "warn" || !strings.Contains(r.detail, "12:00:00") || !strings.Contains(r.detail, "12:05:00") {
		t.Fatalf("%+v", r)
	}
	d.now = func() time.Time { return epoch.Add(90 * time.Second) }
	if r := find(t, runDoctor(context.Background(), d), checkClock); r.status != "pass" {
		t.Fatalf("%+v", r)
	}
	d.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return discoveryResponse(200, `{}`, time.Time{}), nil
	})}
	if r := find(t, runDoctor(context.Background(), d), checkClock); r.status != "skip" {
		t.Fatalf("%+v", r)
	}
}

func TestDoctor_LocalTargets(t *testing.T) {
	d := goodDeps(t)
	d.services = func() (string, []client.TunnelSpec, bool) {
		return "burrow.yaml", []client.TunnelSpec{{Name: "a", LocalAddr: "127.0.0.1:3000"}, {Name: "b", LocalAddr: "127.0.0.1:3001"}}, true
	}
	d.probe = func(_ context.Context, addr string) bool { return addr == "127.0.0.1:3000" }
	rs := runDoctor(context.Background(), d)
	var local []checkResult
	for _, r := range rs {
		if r.name == checkLocal {
			local = append(local, r)
		}
	}
	if len(local) != 2 || local[0].status != "pass" || local[1].status != "warn" ||
		!strings.Contains(local[1].detail, "nothing is listening on 127.0.0.1:3001") {
		t.Fatalf("%+v", local)
	}
	if exitCodeOf(rs) != 0 {
		t.Fatalf("a warning must not fail: exit %d", exitCodeOf(rs))
	}
}

func TestDoctor_HangingNetworkFinishesInTime(t *testing.T) {
	block := func(ctx context.Context) { <-ctx.Done() }
	d := goodDeps(t)
	d.lookup = func(ctx context.Context, _ string) ([]string, error) { block(ctx); return nil, ctx.Err() }
	d.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { block(r.Context()); return nil, r.Context().Err() })}
	d.dialTCP = func(ctx context.Context, _ string) (net.Conn, error) { block(ctx); return nil, ctx.Err() }
	d.checkAuth = func(ctx context.Context, _ client.Options) (client.AuthResult, error) {
		block(ctx)
		return client.AuthResult{}, ctx.Err()
	}
	d.services = func() (string, []client.TunnelSpec, bool) {
		return "burrow.yaml", []client.TunnelSpec{{Name: "a", LocalAddr: "127.0.0.1:3000"}}, true
	}
	d.probe = func(ctx context.Context, _ string) bool { block(ctx); return false }
	d.timeout, d.total = 100*time.Millisecond, 300*time.Millisecond
	start := time.Now()
	rs := runDoctor(context.Background(), d)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	for _, r := range rs {
		if r.status == "pass" && r.name != checkConfig {
			t.Errorf("%s passed while hanging", r.name)
		}
	}
	if len(rs) < 8 {
		t.Fatalf("%d results", len(rs))
	}
}

func TestDoctor_CancelledByCtrlC(t *testing.T) {
	d := goodDeps(t)
	d.lookup = func(ctx context.Context, _ string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() }
	d.total, d.timeout = time.Minute, time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	rs := runDoctor(ctx, d)
	if time.Since(start) > 2*time.Second {
		t.Fatal("not cancelled")
	}
	if r := find(t, rs, checkToken); r.status != "skip" {
		t.Fatalf("%+v", r)
	}
}

// Review focus 4: no check, however it fails, prints the token.
func TestDoctor_NeverPrintsTheToken(t *testing.T) {
	leak := errors.New("Authorization: Bearer " + fakeSecret + " rejected")
	variants := map[string]func(*doctorDeps){
		"pass": func(*doctorDeps) {},
		"errors carrying the token": func(d *doctorDeps) {
			d.lookup = func(context.Context, string) ([]string, error) { return nil, leak }
			d.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, leak })}
			d.dialTCP = func(context.Context, string) (net.Conn, error) { return nil, leak }
			d.checkAuth = func(context.Context, client.Options) (client.AuthResult, error) { return client.AuthResult{}, leak }
		},
		"relay answer carrying the token": func(d *doctorDeps) {
			d.checkAuth = func(context.Context, client.Options) (client.AuthResult, error) {
				return client.AuthResult{Error: "bad token " + fakeSecret}, nil
			}
		},
		"tls error carrying the token": func(d *doctorDeps) {
			d.tlsHandshake = func(context.Context, net.Conn, string) error { return leak }
		},
	}
	for name, mod := range variants {
		t.Run(name, func(t *testing.T) {
			d := goodDeps(t)
			mod(&d)
			rs := runDoctor(context.Background(), d)
			var sb strings.Builder
			printDoctor(&sb, rs, false)
			out := sb.String()
			if strings.Contains(out, fakeSecret) || strings.Contains(out, "SECRETSECRET") {
				t.Fatalf("output contains the token:\n%s", out)
			}
			if strings.Contains(out, "Bearer") && name == "pass" {
				t.Fatal("unexpected")
			}
		})
	}
}

func TestDoctor_Output(t *testing.T) {
	rs := []checkResult{
		{name: "a", status: "pass", detail: "fine"},
		{name: "b", status: "warn", detail: "hmm", fix: "do x"},
		{name: "c", status: "fail", detail: "bad", fix: "do y"},
		{name: "d", status: "skip", detail: "why"},
	}
	var sb strings.Builder
	printDoctor(&sb, rs, false)
	want := "ok    a  fine\nwarn  b  hmm\n  → do x\nFAIL  c  bad\n  → do y\nskip  d  why\n"
	if sb.String() != want {
		t.Fatalf("got:\n%q\nwant:\n%q", sb.String(), want)
	}
	sb.Reset()
	printDoctor(&sb, rs, true)
	if !strings.Contains(sb.String(), "\x1b[") {
		t.Fatal("no colour")
	}
}

func TestDoctorCommand(t *testing.T) {
	h := newHarness(t)
	if code := h.exec("doctor"); code != exitNotSignedIn {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "FAIL") || !strings.Contains(h.stdout.String(), "burrow login") {
		t.Fatalf("stdout:\n%s", h.stdout.String())
	}
	if code := h.exec("doctor", "extra"); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
}
