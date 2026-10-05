package aiprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type mapVault map[string]string

func (v mapVault) Get(slot string) (string, bool) { s, ok := v[slot]; return s, ok }

func testWriteErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}

func TestUpstreamPath(t *testing.T) {
	cases := []struct{ base, rest, want string }{
		{"/api/v1", "/v1/chat/completions", "/api/v1/chat/completions"},
		{"/api/coding/paas/v4", "/v1/chat/completions", "/api/coding/paas/v4/chat/completions"},
		{"/api/paas/v4/", "/v1/models", "/api/paas/v4/models"},
		{"", "/v1/models", "/models"},
		{"/api/v1", "/v1", "/api/v1"},
		{"/api/v1", "/chat/completions", "/api/v1/chat/completions"}, // client omitted /v1
		{"/api/v1", "/v1beta/x", "/api/v1/v1beta/x"},                 // only a whole "/v1" segment is stripped
		{"/api/v1", "/", "/api/v1/"},
	}
	for _, c := range cases {
		if got := UpstreamPath(c.base, c.rest); got != c.want {
			t.Errorf("UpstreamPath(%q, %q) = %q, want %q", c.base, c.rest, got, c.want)
		}
	}
}

// newPair starts a TLS upstream and returns a handler that forwards to it.
func newPair(t *testing.T, upstream http.HandlerFunc, cfg Config, v Vault) http.Handler {
	t.Helper()
	srv := httptest.NewTLSServer(upstream)
	t.Cleanup(srv.Close)
	cfg.BaseURL = srv.URL + "/api/v1"
	h, err := NewUpstream(cfg, v, srv.Client().Transport, testWriteErr)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestUpstream_ForwardsWithUpstreamCredential(t *testing.T) {
	var got *http.Request
	var gotBody string
	h := newPair(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}, Config{Slug: "openrouter", CredentialSlot: "OPENROUTER", ExtraHeaders: map[string]string{"X-Title": "Burrow"}},
		mapVault{"OPENROUTER": "sk-upstream"})

	req := httptest.NewRequest("POST", "/v1/chat/completions?x=1", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer burrow-key")
	req.Header.Set("X-Api-Key", "burrow-key")
	req.Header.Set("Cookie", "burrow_session=abc")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Host", "relay.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Port", "443")
	req.Header.Set("X-Real-Ip", "203.0.113.9")
	req.Header.Set("Forwarded", "for=203.0.113.9")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if got.URL.Path != "/api/v1/chat/completions" || got.URL.RawQuery != "x=1" {
		t.Fatalf("upstream URL = %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Bearer sk-upstream" {
		t.Fatalf("Authorization = %q", got.Header.Get("Authorization"))
	}
	for _, h := range []string{"X-Api-Key", "Cookie", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-Ip", "Forwarded"} {
		if got.Header.Get(h) != "" {
			t.Errorf("%s reached the upstream: %q", h, got.Header.Get(h))
		}
	}
	if got.Header.Get("X-Title") != "Burrow" || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", got.Header)
	}
	if gotBody != `{"model":"m"}` {
		t.Fatalf("body = %s", gotBody)
	}
	if strings.Contains(rec.Body.String(), "sk-upstream") || strings.Contains(strings.Join(rec.Header().Values("Authorization"), ""), "sk-upstream") {
		t.Fatal("upstream credential echoed to the client")
	}
}

func TestUpstream_CustomAuthHeader(t *testing.T) {
	var auth, key string
	h := newPair(t, func(w http.ResponseWriter, r *http.Request) {
		auth, key = r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
	}, Config{Slug: "anth", CredentialSlot: "S", AuthHeader: "X-Api-Key", AuthFormat: "{key}"}, mapVault{"S": "sk-up"})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer burrow-key")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if key != "sk-up" || auth != "" {
		t.Fatalf("X-Api-Key=%q Authorization=%q", key, auth)
	}
}

// A caller must not be able to add a second value to the credential header,
// nor have it dropped by naming it in Connection, nor may a provider's extra
// header replace it.
func TestUpstream_CallerCannotOverrideOrStripCredential(t *testing.T) {
	var got http.Header
	h := newPair(t, func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}, Config{Slug: "p", CredentialSlot: "S", AuthHeader: "X-Upstream-Key", AuthFormat: "{key}",
		ExtraHeaders: map[string]string{"x-upstream-key": "from-extra", "X-Title": "Burrow"}}, mapVault{"S": "sk-up"})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Add("X-Upstream-Key", "caller-1")
	req.Header.Add("X-Upstream-Key", "caller-2")
	req.Header.Add("x-upstream-key", "caller-3")
	req.Header.Set("X-Title", "caller")
	req.Header.Set("Connection", "X-Upstream-Key, X-Title")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if v := got.Values("X-Upstream-Key"); len(v) != 1 || v[0] != "sk-up" {
		t.Fatalf("X-Upstream-Key = %q, want exactly the upstream credential", v)
	}
	if v := got.Values("X-Title"); len(v) != 1 || v[0] != "Burrow" {
		t.Fatalf("X-Title = %q", v)
	}
}

func TestUpstream_KeepsEscapedPathAndRefusesDotSegments(t *testing.T) {
	var gotURI string
	calls := 0
	h := newPair(t, func(_ http.ResponseWriter, r *http.Request) {
		calls++
		gotURI = r.RequestURI
	}, Config{Slug: "p", CredentialSlot: "S"}, mapVault{"S": "k"})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/models/org%2Fname", nil))
	if gotURI != "/api/v1/models/org%2Fname" {
		t.Fatalf("upstream request URI = %q", gotURI)
	}

	// The base URL's path is the scope the operator configured; a caller must
	// not climb out of it with the relay's credential attached.
	calls = 0
	for _, p := range []string{"/v1/../../admin", "/v1/%2e%2e/%2e%2e/admin", "/v1/./x", "/v1/x/.."} {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		u, err := url.ParseRequestURI(p)
		if err != nil {
			t.Fatal(err)
		}
		req.URL = u
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_path") {
			t.Errorf("%s → %d %s", p, rec.Code, rec.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("%d dot-segment requests reached the upstream", calls)
	}
}

func TestUpstream_ErrorMapping(t *testing.T) {
	respond := func(status int, hdr map[string]string, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range hdr {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	cfg := Config{Slug: "p", CredentialSlot: "S"}
	v := mapVault{"S": "sk-up"}
	do := func(up http.HandlerFunc) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		newPair(t, up, cfg, v).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		return rec
	}

	for _, status := range []int{401, 403} {
		rec := do(respond(status, nil, `{"error":"key sk-up is invalid for org 42"}`))
		if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream_auth_failed") {
			t.Errorf("upstream %d → %d %s", status, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "sk-up") || strings.Contains(rec.Body.String(), "org 42") {
			t.Errorf("upstream auth error body leaked: %s", rec.Body.String())
		}
	}

	rec := do(respond(429, map[string]string{"Retry-After": "7"}, `{"error":"slow down"}`))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "7" || !strings.Contains(rec.Body.String(), "slow down") {
		t.Errorf("429 not passed through: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}

	rec = do(respond(400, nil, `{"error":"bad model"}`))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad model") {
		t.Errorf("400 not passed through: %d %s", rec.Code, rec.Body.String())
	}

	// Upstream error bodies are passed through byte for byte.
	const raw = "{\"error\": {\"message\":\"boom\",\n  \"type\":\"server_error\"}}\n"
	rec = do(respond(500, map[string]string{"Content-Type": "application/json", "X-Request-Id": "up-1"}, raw))
	if rec.Code != 500 || rec.Body.String() != raw || rec.Header().Get("X-Request-Id") != "up-1" {
		t.Errorf("500 not passed through untouched: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}

	for _, status := range []int{301, 302, 303, 307, 308} {
		rec = do(respond(status, map[string]string{"Location": "https://elsewhere.example/x"}, ""))
		if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream_redirect") || rec.Header().Get("Location") != "" {
			t.Errorf("redirect %d not blocked: %d %v %s", status, rec.Code, rec.Header(), rec.Body.String())
		}
	}
}

// A redirect must not be followed either: the target never sees a request.
func TestUpstream_RedirectIsNotFollowed(t *testing.T) {
	hits := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(target.Close)
	h := newPair(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusFound)
	}, Config{Slug: "p", CredentialSlot: "S"}, mapVault{"S": "k"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 502 || hits != 0 {
		t.Fatalf("status %d, redirect target hit %d times", rec.Code, hits)
	}
}

// captureLog collects what the package logs for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := new(syncBuffer)
	prevLog, prevSlog := log.Writer(), slog.Default()
	log.SetOutput(buf)
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { log.SetOutput(prevLog); slog.SetDefault(prevSlog) })
	return buf
}

// syncBuffer is a buffer the test servers' own goroutines may log into.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

type failingRT struct{}

func (failingRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: timeout")
}

func TestUpstream_Unreachable(t *testing.T) {
	h, err := NewUpstream(Config{Slug: "p", BaseURL: "https://up.example/v1", CredentialSlot: "S"}, mapVault{"S": "k"}, failingRT{}, testWriteErr)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream_unavailable") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dial tcp") {
		t.Fatal("transport error text leaked to the client")
	}
}

// leakyRT fails with an error that quotes the outbound request, credential
// and query included, the way a careless transport might.
type leakyRT struct{}

func (leakyRT) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: r.Method, URL: r.URL.String(), Err: errors.New("refused; sent " + r.Header.Get("Authorization"))}
}

func TestUpstream_TransportFailureLeaksNothing(t *testing.T) {
	logged := captureLog(t)
	h, err := NewUpstream(Config{Slug: "leaky-provider", BaseURL: "https://up.example/v1", CredentialSlot: "S"}, mapVault{"S": "sk-secret-credential"}, leakyRT{}, testWriteErr)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models?api_key=q-secret", nil))
	if rec.Code != 502 {
		t.Fatalf("status %d", rec.Code)
	}
	// The operator still gets a line saying which provider failed.
	if !strings.Contains(logged.String(), "provider=leaky-provider") || !strings.Contains(logged.String(), "reason=") {
		t.Errorf("log line = %q", logged.String())
	}
	for _, out := range []string{rec.Body.String(), logged.String()} {
		for _, secret := range []string{"sk-secret-credential", "q-secret", "up.example", "/v1/models"} {
			if strings.Contains(out, secret) {
				t.Errorf("%q leaked into %q", secret, out)
			}
		}
	}
}

func TestNewUpstream_ConfigErrors(t *testing.T) {
	if _, err := NewUpstream(Config{BaseURL: "http://x/v1", CredentialSlot: "S"}, mapVault{"S": "k"}, failingRT{}, testWriteErr); !errors.Is(err, ErrInvalidBaseURL) {
		t.Errorf("http base err = %v", err)
	}
	if _, err := NewUpstream(Config{BaseURL: "https://u:pw@x/v1", CredentialSlot: "S"}, mapVault{"S": "k"}, failingRT{}, testWriteErr); !errors.Is(err, ErrInvalidBaseURL) {
		t.Errorf("userinfo base err = %v", err)
	}
	// Without a transport ReverseProxy would use http.DefaultTransport: no
	// address guard, environment proxies honoured.
	if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S"}, mapVault{"S": "k"}, nil, testWriteErr); err == nil {
		t.Error("nil transport accepted")
	}
	var nilTransport *http.Transport
	if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S"}, mapVault{"S": "k"}, nilTransport, testWriteErr); err == nil {
		t.Error("typed nil transport accepted")
	}
	if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S"}, mapVault{"S": "k"}, failingRT{}, nil); err == nil {
		t.Error("nil error writer accepted")
	}
	if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S"}, nil, failingRT{}, testWriteErr); err == nil {
		t.Error("nil vault accepted")
	}
	if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "MISSING"}, mapVault{}, failingRT{}, testWriteErr); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("missing slot err = %v", err)
	}
	if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S"}, mapVault{"S": ""}, failingRT{}, testWriteErr); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("empty slot err = %v", err)
	}
	if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S", AuthFormat: "Bearer"}, mapVault{"S": "k"}, failingRT{}, testWriteErr); err == nil {
		t.Error("auth format without {key} accepted")
	}
	if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S", AuthHeader: "X Bad"}, mapVault{"S": "k"}, failingRT{}, testWriteErr); err == nil {
		t.Error("invalid auth header name accepted")
	}
	// A header name or value that would split the request must be refused.
	for _, hdr := range []map[string]string{{"X-A\r\nX-B": "v"}, {"X-A": "v\r\nX-B: w"}, {"": "v"}, {"X-A": "v\x00"}} {
		if _, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S", ExtraHeaders: hdr}, mapVault{"S": "k"}, failingRT{}, testWriteErr); err == nil {
			t.Errorf("extra header %q accepted", hdr)
		}
	}
	// A credential that cannot be a header value is refused without quoting it.
	_, err := NewUpstream(Config{BaseURL: "https://x/v1", CredentialSlot: "S"}, mapVault{"S": "sk-topsecret\r\nX-Evil: 1"}, failingRT{}, testWriteErr)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("bad credential err = %v", err)
	}
}

func TestUpstream_Streams(t *testing.T) {
	h := newPair(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = w.Write([]byte("data: one\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}, Config{Slug: "p", CredentialSlot: "S"}, mapVault{"S": "k"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`)))
	if !strings.Contains(rec.Body.String(), "data: one") || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// realPair wires a handler to a loopback TLS upstream through the real guarded
// transport. The upstream is addressed by name ("example.com", which the
// httptest certificate covers) so the dial goes through the resolver.
func realPair(t *testing.T, upstream http.HandlerFunc, allowPrivate bool, tune func(*http.Transport)) http.Handler {
	t.Helper()
	srv := httptest.NewTLSServer(upstream)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	tr := newTransport(allowPrivate, fakeResolver{"example.com": {"127.0.0.1"}})
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	if tune != nil {
		tune(tr)
	}
	t.Cleanup(tr.CloseIdleConnections)
	h, err := NewUpstream(Config{Slug: "p", BaseURL: "https://example.com:" + port + "/api/v1", CredentialSlot: "S"},
		mapVault{"S": "sk-secret-credential"}, tr, testWriteErr)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Review Focus 4, end to end: the provider's name resolves to loopback at
// request time; the request is refused before it leaves the relay.
func TestUpstream_GuardRefusesPrivateUpstreamAtConnect(t *testing.T) {
	logged := captureLog(t)
	hits := 0
	up := func(http.ResponseWriter, *http.Request) { hits++ }
	rec := httptest.NewRecorder()
	realPair(t, up, false, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	// The client sees the one neutral error; only the log names the cause.
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream_unavailable") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if hits != 0 {
		t.Fatal("request reached the loopback upstream")
	}
	for _, s := range []string{"127.0.0.1", "sk-secret-credential", "example.com", "public", "blocked"} {
		if strings.Contains(rec.Body.String(), s) {
			t.Errorf("%q leaked: %s", s, rec.Body.String())
		}
	}
	if !strings.Contains(logged.String(), "reason=address_blocked") || !strings.Contains(logged.String(), "provider=p") {
		t.Errorf("log line = %q", logged.String())
	}
	if strings.Contains(logged.String(), "sk-secret-credential") || strings.Contains(logged.String(), "/api/v1") {
		t.Errorf("log leaked: %q", logged.String())
	}

	// The explicit switch, and only that, opens private upstreams.
	rec = httptest.NewRecorder()
	realPair(t, up, true, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 || hits != 1 {
		t.Fatalf("allowPrivate: status %d hits %d", rec.Code, hits)
	}
}

func TestUpstream_TransportFailures(t *testing.T) {
	slow := func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}
	logged := captureLog(t)
	const neutral = "the provider did not answer"
	// No response headers within the transport's bound.
	rec := httptest.NewRecorder()
	realPair(t, slow, true, func(tr *http.Transport) { tr.ResponseHeaderTimeout = 50 * time.Millisecond }).
		ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream_unavailable") || !strings.Contains(rec.Body.String(), neutral) {
		t.Errorf("header timeout → %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logged.String(), "reason=timeout") {
		t.Errorf("timeout log = %q", logged.String())
	}
	logged.Reset()

	// Certificate not trusted → 502.
	rec = httptest.NewRecorder()
	realPair(t, slow, true, func(tr *http.Transport) { tr.TLSClientConfig = nil }).
		ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream_unavailable") {
		t.Errorf("TLS failure → %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "certificate") || strings.Contains(rec.Body.String(), "sk-secret-credential") {
		t.Errorf("TLS failure detail leaked: %s", rec.Body.String())
	}
	if !strings.Contains(logged.String(), "reason=tls") {
		t.Errorf("tls log = %q", logged.String())
	}
	logged.Reset()

	// Nothing listening → 502.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	h, err := NewUpstream(Config{Slug: "p", BaseURL: "https://example.com:" + port + "/v1", CredentialSlot: "S"},
		mapVault{"S": "k"}, newTransport(true, fakeResolver{"example.com": {"127.0.0.1"}}), testWriteErr)
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream_unavailable") {
		t.Errorf("dial refused → %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logged.String(), "reason=dial") || strings.Contains(logged.String(), "sk-secret-credential") {
		t.Errorf("dial log = %q", logged.String())
	}
}

// within fails the test if fn has not returned after five seconds.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out: %s", what)
	}
}

// The first chunk must reach the client while the upstream is still holding
// the response open, and a stream that runs past the response-header timeout
// must not be cut.
func TestUpstream_FlushesAndOutlivesHeaderTimeout(t *testing.T) {
	// Long enough that a loaded machine still gets the headers in time; the
	// test then waits past it once.
	const headerTimeout = time.Second
	release := make(chan struct{})
	h := realPair(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: one\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}, true, func(tr *http.Transport) { tr.ResponseHeaderTimeout = headerTimeout })
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	resp, err := front.Client().Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	var first string
	within(t, "first chunk before the upstream finished", func() { first, _ = br.ReadString('\n') })
	if first != "data: one\n" {
		t.Fatalf("first chunk = %q", first)
	}
	time.Sleep(headerTimeout + 250*time.Millisecond)
	close(release)
	var rest []byte
	within(t, "rest of the stream", func() { rest, err = io.ReadAll(br) })
	if err != nil || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest = %q, err = %v", rest, err)
	}
}

// The request body is forwarded as it arrives: the upstream sees the first
// part before the client has sent the second.
func TestUpstream_StreamsRequestBody(t *testing.T) {
	gotFirst := make(chan string, 1)
	gotRest := make(chan string, 1)
	h := realPair(t, func(_ http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 5)
		_, _ = io.ReadFull(r.Body, buf)
		gotFirst <- string(buf)
		rest, _ := io.ReadAll(r.Body)
		gotRest <- string(rest)
	}, true, nil)
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	pr, pw := io.Pipe()
	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", pr)
	done := make(chan error, 1)
	go func() {
		resp, err := front.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	_, _ = pw.Write([]byte("first"))
	select {
	case s := <-gotFirst:
		if s != "first" {
			t.Fatalf("first part = %q", s)
		}
	case <-time.After(5 * time.Second):
		_ = pw.Close()
		t.Fatal("upstream did not see the first part before the body ended")
	}
	_, _ = pw.Write([]byte("second"))
	_ = pw.Close()
	if s := <-gotRest; s != "second" {
		t.Fatalf("rest = %q", s)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Replaced responses are logged with the provider and the reason only.
func TestUpstream_ReplacedResponsesAreLogged(t *testing.T) {
	cfg := Config{Slug: "openrouter", CredentialSlot: "S"}
	v := mapVault{"S": "sk-up"}
	cases := map[string]struct {
		status int
		reason string
	}{
		"401":      {401, "upstream_auth_failed"},
		"403":      {403, "upstream_auth_failed"},
		"redirect": {302, "upstream_redirect"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPair(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://elsewhere.example/landing")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"key sk-up is invalid for org 42"}`))
			}, cfg, v)
			logs := captureLog(t)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/models", nil))
			out := logs.String()
			if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "provider=openrouter") || !strings.Contains(out, "reason="+tc.reason) {
				t.Fatalf("log = %q", out)
			}
			for _, leak := range []string{"sk-up", "org 42", "elsewhere.example", "landing"} {
				if strings.Contains(out, leak) {
					t.Fatalf("log contains %q: %s", leak, out)
				}
			}
		})
	}
}

type canceledRT struct{}

func (canceledRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("round trip: %w", context.Canceled)
}

// A caller that hangs up before the response headers is not a provider
// failure: it is logged at debug, not at warn.
func TestUpstream_ClientDisconnectLogsAtDebug(t *testing.T) {
	h, err := NewUpstream(Config{Slug: "p", BaseURL: "https://up.example/v1", CredentialSlot: "S"}, mapVault{"S": "k"}, canceledRT{}, testWriteErr)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(syncBuffer)
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/models", nil))
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "provider=p") || !strings.Contains(out, "reason=canceled") {
		t.Fatalf("log = %q", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("a client disconnect was logged at warn: %s", out)
	}
}

// An upstream cannot set cookies on the origin the relay is served from.
func TestUpstream_StripsSetCookie(t *testing.T) {
	h := newPair(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "burrow_session=evil; Path=/")
		w.Header().Add("Set-Cookie", "burrow_csrf=evil; Path=/")
		w.Header().Set("Set-Cookie2", "burrow_session=evil; Version=1")
		w.Header().Set("X-Request-Id", "up-1")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	}, Config{Slug: "p", CredentialSlot: "S"}, mapVault{"S": "k"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 429 || len(rec.Header().Values("Set-Cookie")) != 0 || len(rec.Header().Values("Set-Cookie2")) != 0 {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	if rec.Header().Get("X-Request-Id") != "up-1" || rec.Header().Get("Retry-After") != "7" {
		t.Fatalf("other headers were touched: %v", rec.Header())
	}
}

func TestDropSetCookie(t *testing.T) {
	h := http.Header{}
	h.Add("Set-Cookie", "a=1")
	h.Add("Set-Cookie", "b=2")
	h.Set("Set-Cookie2", "c=3")
	h.Set("Content-Type", "application/json")
	DropSetCookie(h)
	if len(h) != 1 || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
}
