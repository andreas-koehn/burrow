package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/client"
)

var testPin = strings.Repeat("ab", 32)

// pinningDiscovery makes the harness's relay name a control endpoint and a
// certificate fingerprint.
func pinningDiscovery(h *harness, control, pin string) {
	h.discover = func(relay string, g globalFlags) (client.Discovery, error) {
		return client.Discovery{Control: control, Version: "dev", ProtocolVersion: 1, ControlCertSHA256: pin}, nil
	}
}

func TestLoginToken_StoresThePin(t *testing.T) {
	h := newHarness(t)
	pinningDiscovery(h, "burrow.example.com:7000", testPin)
	if code := h.exec("login", "burrow.example.com", "--token", testToken); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := h.stored(h.cfgPath).ControlSHA256; got != testPin {
		t.Fatalf("stored pin %q, want %q", got, testPin)
	}
}

func TestLoginToken_NoPinFromAnOlderRelay(t *testing.T) {
	h := newHarness(t)
	pinningDiscovery(h, "burrow.example.com:7000", "")
	if code := h.exec("login", "burrow.example.com", "--token", testToken); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := h.stored(h.cfgPath).ControlSHA256; got != "" {
		t.Fatalf("stored pin %q, want none", got)
	}
}

// With --insecure the dashboard was not verified either, so what it says
// about the control certificate is no better than a guess: nothing is pinned.
func TestLoginToken_InsecureStoresNoPin(t *testing.T) {
	h := newHarness(t)
	pinningDiscovery(h, "burrow.example.com:7000", testPin)
	if code := h.exec("login", "burrow.example.com", "--token", testToken, "--insecure"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := h.stored(h.cfgPath).ControlSHA256; got != "" {
		t.Fatalf("stored pin %q under --insecure", got)
	}
	if !strings.Contains(h.stderr.String(), "--insecure") {
		t.Fatalf("no warning: %q", h.stderr.String())
	}
}

// The fingerprint is the one of the endpoint the relay named. A --control on
// another endpoint does not get it.
func TestLoginToken_NoPinForAnotherControl(t *testing.T) {
	h := newHarness(t)
	pinningDiscovery(h, "burrow.example.com:7000", testPin)
	if code := h.exec("login", "burrow.example.com", "--token", testToken, "--control", "ctl.example.com:7001"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := h.stored(h.cfgPath).ControlSHA256; got != "" {
		t.Fatalf("stored pin %q for another endpoint", got)
	}
}

func TestLoginBrowser_StoresThePin(t *testing.T) {
	relay := newSignInRelay(t, http.StatusOK)
	h := browserHarness(t)
	control := net.JoinHostPort(relay.host(t), "7000")
	pinningDiscovery(h, control, testPin)
	if code := h.execWithin("login", relay.URL, "--cacert", relay.caFile(t)); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := h.stored(h.cfgPath).ControlSHA256; got != testPin {
		t.Fatalf("stored pin %q, want %q", got, testPin)
	}
}

func TestRunClient_PassesThePinAndRefreshes(t *testing.T) {
	st := captureStart(t)
	creds := client.Credentials{Control: "r.example.com:7000", Token: "bur_x", Relay: "https://r.example.com", Pin: testPin}
	if err := runClient(context.Background(), creds, nil, globalFlags{logLevel: "info", logFormat: "text"}); err != nil {
		t.Fatal(err)
	}
	if st.opts.PinSHA256 != testPin || st.opts.RefreshPin == nil || st.opts.PinRefreshed == nil {
		t.Fatalf("options %+v", st.opts)
	}
}

func TestRunClient_NoPinOptionsWithoutAPin(t *testing.T) {
	st := captureStart(t)
	creds := client.Credentials{Control: "r.example.com:7000", Token: "bur_x", Relay: "https://r.example.com"}
	if err := runClient(context.Background(), creds, nil, globalFlags{logLevel: "info", logFormat: "text"}); err != nil {
		t.Fatal(err)
	}
	if st.opts.PinSHA256 != "" || st.opts.RefreshPin != nil {
		t.Fatalf("options %+v", st.opts)
	}
}

func TestRunClient_MismatchSaysHowToLogInAgain(t *testing.T) {
	st := captureStart(t)
	st.ret = &client.PinMismatchError{Want: testPin, Got: strings.Repeat("cd", 32)}
	creds := client.Credentials{Control: "r.example.com:7000", Token: "bur_x", Relay: "https://r.example.com", Pin: testPin}
	err := runClient(context.Background(), creds, nil, globalFlags{logLevel: "info", logFormat: "text"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != exitUnreachable {
		t.Fatalf("error %v, want an exit %d error", err, exitUnreachable)
	}
	for _, want := range []string{"does not match", "burrow login r.example.com --force"} {
		if !strings.Contains(ee.msg, want) {
			t.Errorf("message lacks %q: %s", want, ee.msg)
		}
	}
}

func TestRunClient_InsecureWarnsLoudly(t *testing.T) {
	captureStart(t)
	var buf strings.Builder
	prev := viewErr
	viewErr = &buf
	t.Cleanup(func() { viewErr = prev })
	creds := client.Credentials{Control: "r.example.com:7000", Token: "bur_x", Pin: testPin}
	if err := runClient(context.Background(), creds, nil, globalFlags{logLevel: "info", logFormat: "text", insecure: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "--insecure") || !strings.Contains(out, "r.example.com:7000") {
		t.Fatalf("warning: %q", out)
	}
}

func TestRequireSecureForward(t *testing.T) {
	if err := (globalFlags{}).requireSecureForward(); err != nil {
		t.Fatal(err)
	}
	if err := (globalFlags{insecure: true, insecureForward: true}).requireSecureForward(); err != nil {
		t.Fatal(err)
	}
	err := (globalFlags{insecure: true}).requireSecureForward()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != exitUsage || !strings.Contains(ee.msg, "--insecure-forward") {
		t.Fatalf("error %v", err)
	}
}

func TestInsecureForwardFlagIsRead(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	if code := h.exec("http", "3000", "--insecure", "--insecure-forward"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	g := h.oneRun().g
	if !g.insecure || !g.insecureForward {
		t.Fatalf("globals %+v", g)
	}
}
