package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/devcert"
)

// pinRelay serves a TLS listener with a fresh dev leaf and returns its address
// and the leaf's fingerprint. The listener accepts and drops connections.
func pinRelay(t *testing.T) (addr, fp string) {
	t.Helper()
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "dev-server.pem"), filepath.Join(dir, "dev-server-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = c.Read(make([]byte, 1)); c.Close() }()
		}
	}()
	sum := sha256.Sum256(cert.Certificate[0])
	return l.Addr().String(), hex.EncodeToString(sum[:])
}

func TestDialControl_PinMatchSkipsChainAndName(t *testing.T) {
	addr, fp := pinRelay(t)
	// No roots and a wrong name: only the pin vouches for the relay.
	conn, err := dialControl(context.Background(), &Options{Server: addr, PinSHA256: fp, ServerName: "not-the-name.example"})
	if err != nil {
		t.Fatalf("pinned dial: %v", err)
	}
	conn.Close()
}

func TestDialControl_PinMismatchRefuses(t *testing.T) {
	addr, fp := pinRelay(t)
	other := strings.Repeat("0", 64)
	_, err := dialControl(context.Background(), &Options{Server: addr, PinSHA256: other})
	var pm *PinMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("error %v, want *PinMismatchError", err)
	}
	if pm.Want != other || pm.Got != fp {
		t.Fatalf("mismatch %+v, want pinned %s presented %s", pm, other, fp)
	}
	if !strings.Contains(err.Error(), "burrow login") {
		t.Fatalf("message does not say how to log in again: %v", err)
	}
}

func TestDialControl_NoPinUsesSystemRoots(t *testing.T) {
	addr, _ := pinRelay(t)
	_, err := dialControl(context.Background(), &Options{Server: addr, ServerName: "localhost"})
	if err == nil {
		t.Fatal("a self-signed leaf was trusted without a pin")
	}
	var pm *PinMismatchError
	if errors.As(err, &pm) {
		t.Fatal("no pin set, yet a pin error")
	}
}

func TestDialControl_InsecureIgnoresThePin(t *testing.T) {
	addr, _ := pinRelay(t)
	conn, err := dialControl(context.Background(), &Options{Server: addr, Insecure: true, PinSHA256: strings.Repeat("0", 64)})
	if err != nil {
		t.Fatalf("--insecure dial: %v", err)
	}
	conn.Close()
}

func TestDialControl_RootCAsBeatThePin(t *testing.T) {
	// --cacert is explicit: the chain is checked, the stored pin is not used.
	dir := t.TempDir()
	if err := devcert.Generate(dir, true); err != nil {
		t.Fatal(err)
	}
	addr, _ := pinRelay(t) // a different dev CA than dir's: the chain check fails
	o := checkOpts(t, dir, addr, "t")
	o.PinSHA256 = strings.Repeat("0", 64)
	_, err := dialControl(context.Background(), &o)
	var pm *PinMismatchError
	if err == nil || errors.As(err, &pm) {
		t.Fatalf("error %v, want a certificate error from the chain check", err)
	}
}

// A renewed certificate: the pin fails, the dashboard (valid HTTPS) names the
// certificate the relay presents now, so the pin is replaced.
func TestDialControl_RefreshesAPinThatTheDashboardVouchesFor(t *testing.T) {
	addr, fp := pinRelay(t)
	old := strings.Repeat("1", 64)
	var refreshed string
	o := &Options{Server: addr, PinSHA256: old,
		RefreshPin:   func(context.Context) (string, error) { return fp, nil },
		PinRefreshed: func(p string) { refreshed = p },
	}
	conn, err := dialControl(context.Background(), o)
	if err != nil {
		t.Fatalf("refresh dial: %v", err)
	}
	conn.Close()
	if refreshed != fp || o.PinSHA256 != fp {
		t.Fatalf("pin after refresh %q / callback %q, want %q", o.PinSHA256, refreshed, fp)
	}
}

// An interceptor presents its own certificate; the dashboard still names the
// real one, so the pin is not replaced.
func TestDialControl_NoRefreshWhenTheDashboardDisagrees(t *testing.T) {
	addr, _ := pinRelay(t)
	old := strings.Repeat("1", 64)
	called := false
	o := &Options{Server: addr, PinSHA256: old,
		RefreshPin:   func(context.Context) (string, error) { return strings.Repeat("2", 64), nil },
		PinRefreshed: func(string) { called = true },
	}
	_, err := dialControl(context.Background(), o)
	var pm *PinMismatchError
	if !errors.As(err, &pm) || called || o.PinSHA256 != old {
		t.Fatalf("err %v, refreshed %v, pin %q: want a mismatch and the old pin", err, called, o.PinSHA256)
	}
}

func TestDialControl_RefreshFailureKeepsTheMismatch(t *testing.T) {
	addr, _ := pinRelay(t)
	o := &Options{Server: addr, PinSHA256: strings.Repeat("1", 64),
		RefreshPin: func(context.Context) (string, error) { return "", errors.New("dashboard down") }}
	_, err := dialControl(context.Background(), o)
	var pm *PinMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("err %v, want a mismatch", err)
	}
}

func TestRun_StopsOnAPinMismatch(t *testing.T) {
	addr, _ := pinRelay(t)
	c := New(Options{Server: addr, PinSHA256: strings.Repeat("1", 64)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Run(ctx)
	var pm *PinMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("Run returned %v, want the pin mismatch (retrying cannot fix it)", err)
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	hexv := strings.Repeat("ab", 32)
	for in, ok := range map[string]bool{
		hexv:                             true,
		strings.ToUpper(hexv):            true,
		"sha256:" + hexv:                 true,
		strings.Repeat("ab:", 31) + "ab": true,
		"":                               false,
		hexv[:62]:                        false,
		strings.Repeat("zz", 32):         false,
	} {
		got, good := NormalizeFingerprint(in)
		if good != ok || (ok && got != hexv) {
			t.Errorf("NormalizeFingerprint(%q) = %q, %v", in, got, good)
		}
	}
}

func TestForwardAllowed(t *testing.T) {
	if err := ForwardAllowed(false, false); err != nil {
		t.Fatal(err)
	}
	if err := ForwardAllowed(true, true); err != nil {
		t.Fatal(err)
	}
	err := ForwardAllowed(true, false)
	if !errors.Is(err, ErrInsecureForward) || !strings.Contains(err.Error(), "--insecure-forward") {
		t.Fatalf("err %v", err)
	}
}

func TestDiscover_ReadsTheFingerprint(t *testing.T) {
	good := strings.Repeat("cd", 32)
	for name, tc := range map[string]struct{ in, want string }{
		"present":   {good, good},
		"uppercase": {strings.ToUpper(good), good},
		"garbage":   {"nothex", ""},
		"absent":    {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := `{"control":"relay.example.com:7000","version":"v1","min_client_version":"","protocol_version":1`
				if tc.in != "" {
					body += `,"control_cert_sha256":"` + tc.in + `"`
				}
				_, _ = w.Write([]byte(body + `}`))
			}))
			defer srv.Close()
			d, err := Discover(context.Background(), srv.Client(), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if d.ControlCertSHA256 != tc.want {
				t.Fatalf("fingerprint %q, want %q", d.ControlCertSHA256, tc.want)
			}
		})
	}
}

func TestResolve_CarriesThePinOnlyForTheStoredEndpoint(t *testing.T) {
	pin := strings.Repeat("ef", 32)
	user := &UserConfig{Relay: "https://r.example.com", Control: "r.example.com:7000", Token: "bur_x", TokenName: "n", ControlSHA256: pin}
	c, err := Resolve(Sources{User: user})
	if err != nil || c.Pin != pin {
		t.Fatalf("stored: %+v %v", c, err)
	}
	c, err = Resolve(Sources{User: user, EnvServer: "R.example.com:7000"})
	if err != nil || c.Pin != pin {
		t.Fatalf("same endpoint from the environment: %+v %v", c, err)
	}
	// Another endpoint with its own token never gets the stored pin.
	c, err = Resolve(Sources{User: user, EnvServer: "other.example.com:7000", EnvToken: "bur_y"})
	if err != nil || c.Pin != "" {
		t.Fatalf("other endpoint: %+v %v", c, err)
	}
}

func TestUserConfig_PinRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	in := UserConfig{Relay: "https://r", Control: "r:7000", Token: "bur_x", ControlSHA256: strings.Repeat("ab", 32)}
	if err := SaveUserConfig(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadUserConfig(path)
	if err != nil || out.ControlSHA256 != in.ControlSHA256 {
		t.Fatalf("%+v %v", out, err)
	}
	var _ net.Conn
}
