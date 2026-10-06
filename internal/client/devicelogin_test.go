package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDeviceCode stands in for a device code. Tests check that it shows up in
// request bodies only and never print it.
const fakeDeviceCode = "dc_test_0000000000000000000000000000000000000000"

// fakeLoginToken stands in for the token a relay hands out.
const fakeLoginToken = "bur_test_0000"

// deviceRelay is an HTTPS server standing in for a relay. Every request is
// recorded; start and poll answer what the test scripts.
type deviceRelay struct {
	*httptest.Server
	mu    sync.Mutex
	reqs  []deviceReq
	start func(w http.ResponseWriter, r *http.Request)
	// polls are the answers of poll, in order; the last one repeats.
	polls []func(w http.ResponseWriter)
	nPoll int
}

type deviceReq struct {
	method, url, body string
	header            http.Header
}

func answer(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

var (
	pending  = answer(http.StatusAccepted, `{"status":"pending"}`)
	slowDown = answer(http.StatusTooManyRequests, `{"error":"slow_down"}`)
	approved = answer(http.StatusOK, `{"token":"`+fakeLoginToken+`","token_name":"laptop","email":"admin@example.com"}`)
)

func newDeviceRelay(t *testing.T) *deviceRelay {
	t.Helper()
	dr := &deviceRelay{}
	dr.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		dr.mu.Lock()
		dr.reqs = append(dr.reqs, deviceReq{r.Method, r.URL.String(), string(body), r.Header.Clone()})
		start := dr.start
		var poll func(http.ResponseWriter)
		if r.URL.Path == DeviceLoginPollPath && len(dr.polls) > 0 {
			i := dr.nPoll
			if i >= len(dr.polls) {
				i = len(dr.polls) - 1
			}
			poll = dr.polls[i]
			dr.nPoll++
		}
		dr.mu.Unlock()
		switch {
		case r.URL.Path == DeviceLoginStartPath && start != nil:
			start(w, r)
		case poll != nil:
			poll(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(dr.Close)
	return dr
}

func (dr *deviceRelay) requests() []deviceReq {
	dr.mu.Lock()
	defer dr.mu.Unlock()
	return append([]deviceReq(nil), dr.reqs...)
}

func (dr *deviceRelay) pollCount() int {
	n := 0
	for _, r := range dr.requests() {
		if strings.HasPrefix(r.url, DeviceLoginPollPath) {
			n++
		}
	}
	return n
}

// startAnswer is a start answer of this relay with the given verification URL
// ("" for the relay's own).
func (dr *deviceRelay) startAnswer(verification string, expiresIn, interval int) {
	if verification == "" {
		verification = dr.URL + "/link?code=BRRW-7Q4K"
	}
	raw, _ := json.Marshal(map[string]any{"device_code": fakeDeviceCode, "user_code": "BRRW-7Q4K",
		"verification_url": verification, "expires_in": expiresIn, "interval": interval})
	body := string(raw)
	dr.mu.Lock()
	dr.start = func(w http.ResponseWriter, _ *http.Request) { answer(http.StatusOK, body)(w) }
	dr.mu.Unlock()
}

// sleeps is an injected Sleep that returns at once and records what it was
// asked for. It honours a cancelled context like the real one.
type sleeps struct {
	mu  sync.Mutex
	got []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.got = append(s.got, d)
	s.mu.Unlock()
	return nil
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.got...)
}

func (dr *deviceRelay) login(s *sleeps) DeviceLogin {
	d := DeviceLogin{HTTP: dr.Client(), Relay: dr.URL}
	if s != nil {
		d.Sleep = s.sleep
	}
	return d
}

// a started request as Start returns it for this relay.
func (dr *deviceRelay) started(expires, interval time.Duration) DeviceStart {
	return DeviceStart{DeviceCode: fakeDeviceCode, UserCode: "BRRW-7Q4K",
		VerificationURL: dr.URL + "/link?code=BRRW-7Q4K", ExpiresIn: expires, Interval: interval}
}

func TestDeviceStart_PostsTheMachineAndParsesTheAnswer(t *testing.T) {
	dr := newDeviceRelay(t)
	dr.startAnswer("", 600, 2)
	d := dr.login(nil)
	d.Meta.Hostname, d.Meta.OS, d.Meta.Arch, d.Meta.ClientVersion = "kohns-laptop", "linux", "amd64", "v0.7.0"
	s, err := d.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.DeviceCode != fakeDeviceCode || s.UserCode != "BRRW-7Q4K" || s.VerificationURL != dr.URL+"/link?code=BRRW-7Q4K" ||
		s.ExpiresIn != 10*time.Minute || s.Interval != 2*time.Second || s.OwnURL {
		t.Fatalf("user code %q, url %q, expires %v, interval %v, own url %v", s.UserCode, s.VerificationURL, s.ExpiresIn, s.Interval, s.OwnURL)
	}
	reqs := dr.requests()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].url != DeviceLoginStartPath {
		t.Fatalf("requests: %d, first %s %s", len(reqs), reqs[0].method, reqs[0].url)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(reqs[0].body), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"hostname": "kohns-laptop", "os": "linux", "arch": "amd64", "client_version": "v0.7.0"}
	if len(got) != len(want) {
		t.Fatalf("body %v, want %v (no token_name without a name)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("body[%q] = %v, want %v", k, got[k], v)
		}
	}
	if ct := reqs[0].header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}

	d.Meta.TokenName = "work"
	if _, err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body := dr.requests()[1].body; !strings.Contains(body, `"token_name":"work"`) {
		t.Fatalf("the name is not in the start request: %s", body)
	}
}

func TestDeviceStart_Statuses(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		is     error
		says   string
	}{
		"404 is a relay without browser sign-in": {http.StatusNotFound, ErrNoBrowserLogin, "does not support browser sign-in"},
		"405 too":                                {http.StatusMethodNotAllowed, ErrNoBrowserLogin, ""},
		"429 says to try again":                  {http.StatusTooManyRequests, ErrLoginBusy, "too many pending sign-ins"},
		"500":                                    {http.StatusInternalServerError, nil, "status 500"},
		"400":                                    {http.StatusBadRequest, nil, "status 400"},
	} {
		t.Run(name, func(t *testing.T) {
			dr := newDeviceRelay(t)
			dr.start = func(w http.ResponseWriter, _ *http.Request) { answer(tc.status, `{"error":"x"}`)(w) }
			_, err := dr.login(nil).Start(context.Background())
			if err == nil || (tc.is != nil && !errors.Is(err, tc.is)) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("err = %v", err)
			}
			if tc.is == ErrLoginBusy && !strings.Contains(err.Error(), "try again in a minute") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A redirect is never followed: the poll would carry the device code along.
func TestDeviceLogin_RedirectIsNotFollowed(t *testing.T) {
	other := newDeviceRelay(t)
	other.startAnswer("", 600, 2)
	other.polls = []func(http.ResponseWriter){approved}
	dr := newDeviceRelay(t)
	redirect := func(to string) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Location", to)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
	}
	dr.start = func(w http.ResponseWriter, _ *http.Request) { redirect(other.URL + DeviceLoginStartPath)(w) }
	dr.polls = []func(http.ResponseWriter){redirect(other.URL + DeviceLoginPollPath)}
	// One client that trusts both servers, so that only the refusal to follow
	// keeps the request away from the other one.
	d := dr.login(&sleeps{})
	if _, err := d.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "status 307") {
		t.Fatalf("start: err = %v", err)
	}
	if _, err := d.Wait(context.Background(), dr.started(time.Minute, time.Second)); err == nil || !strings.Contains(err.Error(), "status 307") {
		t.Fatalf("wait: err = %v", err)
	}
	if n := len(other.requests()); n != 0 {
		t.Fatalf("%d requests followed the redirect", n)
	}
}

func TestDeviceStart_AnswersThatAreNotASignIn(t *testing.T) {
	for name, body := range map[string]string{
		"a page":               "<!doctype html><html></html>",
		"no device code":       `{"user_code":"BRRW-7Q4K","expires_in":600,"interval":2}`,
		"no user code":         `{"device_code":"x","expires_in":600,"interval":2}`,
		"user code with a url": `{"device_code":"x","user_code":"a/../b?c=d","expires_in":600,"interval":2}`,
		"user code with esc":   `{"device_code":"x","user_code":"AB\u001b[2J","expires_in":600,"interval":2}`,
		"device code with nl":  `{"device_code":"x\ny","user_code":"BRRW-7Q4K","expires_in":600,"interval":2}`,
		"trailing data":        `{"device_code":"x","user_code":"BRRW-7Q4K"} {}`,
		"too large":            `{"device_code":"` + strings.Repeat("a", maxDeviceBody) + `","user_code":"BRRW-7Q4K"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dr := newDeviceRelay(t)
			dr.start = func(w http.ResponseWriter, _ *http.Request) { answer(http.StatusOK, body)(w) }
			_, err := dr.login(nil).Start(context.Background())
			if !errors.Is(err, ErrNotASignIn) {
				t.Fatalf("err = %v, want ErrNotASignIn", err)
			}
		})
	}
}

// What the relay says about timing is bounded: nothing makes the client poll
// in a tight loop or wait for days.
func TestDeviceStart_BoundsTheTiming(t *testing.T) {
	for _, tc := range []struct {
		expires, interval       int
		wantExpires, wantPeriod time.Duration
	}{
		{0, 0, 10 * time.Minute, 5 * time.Second},
		{-5, -1, 10 * time.Minute, 5 * time.Second},
		{1 << 30, 1 << 30, time.Hour, time.Minute},
		{30, 1, 30 * time.Second, time.Second},
	} {
		dr := newDeviceRelay(t)
		dr.startAnswer("", tc.expires, tc.interval)
		s, err := dr.login(nil).Start(context.Background())
		if err != nil || s.ExpiresIn != tc.wantExpires || s.Interval != tc.wantPeriod {
			t.Fatalf("%d/%d: expires %v interval %v, err %v", tc.expires, tc.interval, s.ExpiresIn, s.Interval, err)
		}
	}
}

// The page the user opens is on the relay that was asked. An address the
// relay names is used only when it is on that origin; otherwise the relay's
// own /link is built and OwnURL says so.
func TestDeviceStart_VerificationURL(t *testing.T) {
	dr := newDeviceRelay(t)
	own := dr.URL + "/link?code=BRRW-7Q4K"
	host := strings.TrimPrefix(dr.URL, "https://")
	for name, tc := range map[string]struct {
		given string
		want  string // "" = the relay's own page, with OwnURL set
	}{
		"the relay's own":         {own, own},
		"another path, same host": {dr.URL + "/link/v2?code=BRRW-7Q4K", dr.URL + "/link/v2?code=BRRW-7Q4K"},
		"another host":            {"https://evil.example.com/link?code=BRRW-7Q4K", ""},
		"another port":            {"https://127.0.0.1:1/link?code=BRRW-7Q4K", ""},
		"plain http":              {"http://" + host + "/link?code=BRRW-7Q4K", ""},
		"credentials":             {"https://user:pw@" + host + "/link", ""},
		"host as credentials":     {"https://" + host + "@evil.example.com/link", ""},
		"a file":                  {"file:///etc/passwd", ""},
		"a script":                {"javascript:alert(1)", ""},
		"an option":               {"--version", ""},
		"a space and a command":   {dr.URL + "/link?code=A B;rm", ""},
		"a quote":                 {dr.URL + `/link?code="x`, ""},
		"a backslash":             {dr.URL + `\@evil.example.com/link`, ""},
		"a fragment":              {dr.URL + "/link#x", ""},
		"a control character":     {dr.URL + "/link?code=\x1b[2J", ""},
		"very long":               {dr.URL + "/link?code=" + strings.Repeat("A", 600), ""},
	} {
		t.Run(name, func(t *testing.T) {
			dr.startAnswer(tc.given, 600, 2)
			s, err := dr.login(nil).Start(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if s.VerificationURL != own || !s.OwnURL {
					t.Fatalf("url %q (own %v), want the relay's own page", s.VerificationURL, s.OwnURL)
				}
				return
			}
			if s.VerificationURL != tc.want || s.OwnURL {
				t.Fatalf("url %q (own %v), want %q", s.VerificationURL, s.OwnURL, tc.want)
			}
		})
	}
}

// A relay that names no page gets its own /link, and there is nothing to
// point out.
func TestDeviceStart_NoVerificationURL(t *testing.T) {
	dr := newDeviceRelay(t)
	dr.startAnswer(" ", 600, 2)
	s, err := dr.login(nil).Start(context.Background())
	if err != nil || s.VerificationURL != dr.URL+"/link?code=BRRW-7Q4K" || s.OwnURL {
		t.Fatalf("url %q (own %v), err %v", s.VerificationURL, s.OwnURL, err)
	}
}

// The device code travels over https only, and only to the relay's origin.
func TestDeviceLogin_HTTPSOnly(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a request was sent without TLS")
	}))
	defer plain.Close()
	for _, relay := range []string{plain.URL, "http://127.0.0.1:1", "http://localhost", "ftp://burrow.example.com", "burrow.example.com",
		"https://user:pw@burrow.example.com", "https://burrow.example.com/path", "https://burrow.example.com?x=1", ""} {
		d := DeviceLogin{HTTP: plain.Client(), Relay: relay, Sleep: (&sleeps{}).sleep}
		if _, err := d.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "https://") {
			t.Fatalf("Start(%q): err = %v", relay, err)
		}
		_, err := d.Wait(context.Background(), DeviceStart{DeviceCode: fakeDeviceCode, ExpiresIn: time.Minute, Interval: time.Second})
		if err == nil || !strings.Contains(err.Error(), "https://") {
			t.Fatalf("Wait(%q): err = %v", relay, err)
		}
	}
}

func TestDeviceWait_PollsUntilApproved(t *testing.T) {
	dr := newDeviceRelay(t)
	dr.polls = []func(http.ResponseWriter){pending, pending, approved}
	s := &sleeps{}
	tok, err := dr.login(s).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != fakeLoginToken || tok.TokenName != "laptop" || tok.Email != "admin@example.com" {
		t.Fatalf("token name %q, email %q, token as sent: %v", tok.TokenName, tok.Email, tok.Token == fakeLoginToken)
	}
	if n := dr.pollCount(); n != 3 {
		t.Fatalf("%d polls, want exactly 3", n)
	}
	// A little slower than the relay's interval: a poll that arrives early is
	// answered with slow_down.
	want := 2*time.Second + pollMargin
	got := s.all()
	if len(got) != 3 || got[0] != want || got[1] != want || got[2] != want {
		t.Fatalf("slept %v, want three times %v", got, want)
	}
	// The device code is in the body of the polls and nowhere else.
	for _, r := range dr.requests() {
		if r.method != http.MethodPost || r.url != DeviceLoginPollPath {
			t.Fatalf("request %s %s", r.method, r.url)
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(r.body), &body); err != nil || len(body) != 1 || body["device_code"] != fakeDeviceCode {
			t.Fatal("the poll body is not {device_code}")
		}
		for k, v := range r.header {
			if strings.Contains(strings.Join(v, " "), fakeDeviceCode) {
				t.Fatalf("the device code is in header %s", k)
			}
		}
	}
}

func TestDeviceWait_SlowDownGrowsTheInterval(t *testing.T) {
	dr := newDeviceRelay(t)
	dr.polls = []func(http.ResponseWriter){pending, slowDown, pending, slowDown, approved}
	s := &sleeps{}
	if _, err := dr.login(s).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	base := 2*time.Second + pollMargin
	want := []time.Duration{base, base, base + 5*time.Second, base + 5*time.Second, base + 10*time.Second}
	got := s.all()
	if len(got) != len(want) {
		t.Fatalf("slept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slept %v, want %v", got, want)
		}
	}
}

// However often the relay says slow_down, the wait between polls stays bounded.
func TestDeviceWait_SlowDownIsBounded(t *testing.T) {
	dr := newDeviceRelay(t)
	dr.polls = []func(http.ResponseWriter){slowDown}
	s := &sleeps{}
	_, err := dr.login(s).Wait(context.Background(), dr.started(time.Hour, 2*time.Second))
	if !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("err = %v", err)
	}
	for _, d := range s.all() {
		if d > maxPollInterval+pollMargin {
			t.Fatalf("slept %v", d)
		}
	}
}

func TestDeviceWait_DeniedAndExpired(t *testing.T) {
	for name, tc := range map[string]struct {
		answer func(http.ResponseWriter)
		want   error
	}{
		"403": {answer(http.StatusForbidden, `{"error":"access_denied"}`), ErrLoginDenied},
		"410": {answer(http.StatusGone, `{"error":"expired_token"}`), ErrLoginExpired},
	} {
		t.Run(name, func(t *testing.T) {
			dr := newDeviceRelay(t)
			dr.polls = []func(http.ResponseWriter){pending, tc.answer}
			_, err := dr.login(&sleeps{}).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second))
			if !errors.Is(err, tc.want) || dr.pollCount() != 2 {
				t.Fatalf("err = %v after %d polls", err, dr.pollCount())
			}
		})
	}
}

// When the request's life is over the client stops by itself, without asking
// once more.
func TestDeviceWait_StopsAtExpiry(t *testing.T) {
	dr := newDeviceRelay(t)
	dr.polls = []func(http.ResponseWriter){pending}
	s := &sleeps{}
	_, err := dr.login(s).Wait(context.Background(), dr.started(6*time.Second, 2*time.Second))
	if !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("err = %v", err)
	}
	// 2.5 s, 2.5 s, and the third wait would pass the six seconds.
	if n := dr.pollCount(); n != 2 {
		t.Fatalf("%d polls, want 2", n)
	}
	var total time.Duration
	for _, d := range s.all() {
		total += d
	}
	if total < 5*time.Second || total > 6*time.Second {
		t.Fatalf("waited %v in all, want the request's life of 6s and no more", total)
	}
}

func TestDeviceWait_NetworkErrors(t *testing.T) {
	hang := func(w http.ResponseWriter) {
		// The connection is cut without an answer.
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				_ = c.Close()
			}
		}
	}
	t.Run("one failure is retried at the next interval", func(t *testing.T) {
		dr := newDeviceRelay(t)
		dr.polls = []func(http.ResponseWriter){hang, hang, pending, hang, approved}
		s := &sleeps{}
		tok, err := dr.login(s).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second))
		if err != nil || tok.Token != fakeLoginToken {
			t.Fatalf("err = %v", err)
		}
		if n := len(s.all()); n != 5 {
			t.Fatalf("slept %d times, want once before each of the 5 polls", n)
		}
	})
	t.Run("three in a row end the wait", func(t *testing.T) {
		dr := newDeviceRelay(t)
		dr.polls = []func(http.ResponseWriter){pending, hang}
		_, err := dr.login(&sleeps{}).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second))
		if err == nil || errors.Is(err, ErrLoginExpired) || errors.Is(err, ErrLoginDenied) {
			t.Fatalf("err = %v", err)
		}
		if n := dr.pollCount(); n != 4 {
			t.Fatalf("%d polls, want 1 answered and 3 failed", n)
		}
		if strings.Contains(err.Error(), fakeDeviceCode) || strings.Contains(err.Error(), DeviceLoginPollPath) {
			t.Fatal("the error repeats the request")
		}
	})
	t.Run("a status that means nothing counts as a failure", func(t *testing.T) {
		dr := newDeviceRelay(t)
		dr.polls = []func(http.ResponseWriter){answer(http.StatusBadGateway, "<html>")}
		_, err := dr.login(&sleeps{}).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second))
		if err == nil || !strings.Contains(err.Error(), "status 502") || dr.pollCount() != 3 {
			t.Fatalf("err = %v after %d polls", err, dr.pollCount())
		}
	})
}

// A relay that accepts the connection and then says nothing does not hold the
// client longer than the time one request may take.
func TestDeviceWait_RequestTimeout(t *testing.T) {
	prev := deviceRequestTimeout
	deviceRequestTimeout = 150 * time.Millisecond
	defer func() { deviceRequestTimeout = prev }()

	release := make(chan struct{})
	dr := newDeviceRelay(t)
	dr.polls = []func(http.ResponseWriter){func(http.ResponseWriter) { <-release }}
	defer close(release)
	done := make(chan error, 1)
	go func() {
		_, err := dr.login(&sleeps{}).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not end: a silent relay holds the client")
	}
}

// Ctrl-C ends the wait at once: in the sleep between two polls, and in a
// request that is under way.
func TestDeviceWait_Cancel(t *testing.T) {
	t.Run("between polls", func(t *testing.T) {
		dr := newDeviceRelay(t)
		dr.polls = []func(http.ResponseWriter){pending}
		ctx, cancel := context.WithCancel(context.Background())
		d := dr.login(nil) // the real sleep: it must end with the context
		done := make(chan error, 1)
		go func() {
			_, err := d.Wait(ctx, dr.started(10*time.Minute, time.Minute))
			done <- err
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the wait went on after the context was cancelled")
		}
		if n := dr.pollCount(); n != 0 {
			t.Fatalf("%d polls after a cancel in the first wait", n)
		}
	})
	t.Run("during a request", func(t *testing.T) {
		release := make(chan struct{})
		asked := make(chan struct{}, 1)
		dr := newDeviceRelay(t)
		dr.polls = []func(http.ResponseWriter){func(http.ResponseWriter) {
			asked <- struct{}{}
			<-release
		}}
		defer close(release)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := dr.login(&sleeps{}).Wait(ctx, dr.started(10*time.Minute, 2*time.Second))
			done <- err
		}()
		<-asked
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the wait went on after the context was cancelled")
		}
	})
}

// What comes back with the token is shown on a terminal and stored: only a
// usable token is accepted, and name and email are text or nothing.
func TestDeviceWait_ChecksTheAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"no token":           `{"token":"","token_name":"laptop","email":"a@x"}`,
		"token with a space": `{"token":"bur_a b","token_name":"laptop","email":"a@x"}`,
		"token with newline": `{"token":"bur_a\nb","token_name":"laptop","email":"a@x"}`,
		"not json":           `<html>`,
		"too large":          `{"token":"` + strings.Repeat("a", maxDeviceBody) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dr := newDeviceRelay(t)
			dr.polls = []func(http.ResponseWriter){answer(http.StatusOK, body)}
			_, err := dr.login(&sleeps{}).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second))
			// The request is spent after a 200: asking again cannot help.
			if !errors.Is(err, ErrNotASignIn) || dr.pollCount() != 1 {
				t.Fatalf("err = %v after %d polls", err, dr.pollCount())
			}
		})
	}
	dr := newDeviceRelay(t)
	dr.polls = []func(http.ResponseWriter){answer(http.StatusOK,
		`{"token":"`+fakeLoginToken+`","token_name":"lap\u001b[2Jtop","email":"a@x\r\nSigned in as root"}`)}
	tok, err := dr.login(&sleeps{}).Wait(context.Background(), dr.started(10*time.Minute, 2*time.Second))
	if err != nil || tok.Token != fakeLoginToken || tok.TokenName != "" || tok.Email != "" {
		t.Fatalf("err %v, token name %q, email %q: text with control characters must be dropped", err, tok.TokenName, tok.Email)
	}
}

// Neither value shows its secret when it is formatted or logged by accident.
func TestDeviceLogin_SecretsAreNotFormatted(t *testing.T) {
	s := DeviceStart{DeviceCode: fakeDeviceCode, UserCode: "BRRW-7Q4K"}
	tok := DeviceToken{Token: fakeLoginToken, TokenName: "laptop"}
	var log strings.Builder
	slog.New(slog.NewTextHandler(&log, nil)).Info("x", "start", s, "token", tok)
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s), fmt.Sprintf("%v %+v %#v", &s, &s, &s),
		fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok), fmt.Sprintf("%v %+v %#v", &tok, &tok, &tok),
		log.String(),
	} {
		if strings.Contains(out, fakeDeviceCode) || strings.Contains(out, fakeLoginToken) {
			t.Fatal("a secret was formatted")
		}
	}
}
