package proxy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxy"
)

// gRPC through http services (H6–H9, H14), with a hand-rolled gRPC framing
// over the standard library's HTTP/2: a message is a 5-byte prefix (flag,
// big-endian length) and its bytes, the outcome is the grpc-status trailer.

func grpcFrame(msg string) []byte {
	b := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(b[1:5], uint32(len(msg)))
	copy(b[5:], msg)
	return b
}

func readGRPCFrame(r io.Reader) (string, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", err
	}
	b := make([]byte, binary.BigEndian.Uint32(hdr[1:5]))
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// grpcEcho is a gRPC service over HTTP/2: it answers every message with
// "echo:"+message as it arrives and ends with grpc-status 0, announced as a
// trailer, or with http.TrailerPrefix when prefixed is set. It records the
// grpc-timeout it was sent.
type grpcEcho struct {
	prefixed bool
	mu       sync.Mutex
	timeouts []string
	major    int
}

func (e *grpcEcho) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.timeouts = append(e.timeouts, r.Header.Get("Grpc-Timeout"))
	e.major = r.ProtoMajor
	e.mu.Unlock()
	if r.ProtoMajor != 2 {
		http.Error(w, "want HTTP/2", http.StatusHTTPVersionNotSupported)
		return
	}
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("X-From-Service", "yes")
	if !e.prefixed {
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	for {
		msg, err := readGRPCFrame(r.Body)
		if err != nil {
			break
		}
		_, _ = w.Write(grpcFrame("echo:" + msg))
		_ = rc.Flush()
	}
	if e.prefixed {
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "done")
		return
	}
	w.Header().Set("Grpc-Status", "0")
	w.Header().Set("Grpc-Message", "done")
}

func (e *grpcEcho) lastTimeout() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.timeouts) == 0 {
		return "<none>"
	}
	return e.timeouts[len(e.timeouts)-1]
}

// grpcFront serves h as a gRPC caller reaches it: HTTP/2 over TLS ("h2"),
// or by prior knowledge in plain text ("h2c", as a TLS-terminating proxy in
// front sends it). The client speaks the same.
func grpcFront(t *testing.T, h http.Handler, front string) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	if front == "h2" {
		ts.EnableHTTP2 = true
		ts.StartTLS()
		t.Cleanup(ts.Close)
		return ts, ts.Client()
	}
	var sp http.Protocols
	sp.SetHTTP1(true)
	sp.SetUnencryptedHTTP2(true)
	ts.Config.Protocols = &sp
	ts.Start()
	t.Cleanup(ts.Close)
	var cp http.Protocols
	cp.SetUnencryptedHTTP2(true)
	return ts, &http.Client{Transport: &http.Transport{Protocols: &cp}}
}

// grpcRequest is a gRPC call to the service sub at path, with body.
func grpcRequest(t *testing.T, ts *httptest.Server, sub, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = sub + "." + authDomain
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("Te", "trailers")
	return req
}

// echoProxy is a proxy before a grpcEcho service "app" over h2c, with the
// connection log recorded.
func echoProxy(t *testing.T, echo *grpcEcho, res proxy.Resolved, opts ...proxy.Option) (*fakeDialer, *connLogRec, *proxy.Proxy) {
	t.Helper()
	d := newFakeDialer(echo)
	d.h2c = true
	if res.ServiceID == "" {
		res.ServiceID = "s1"
	}
	if res.AccessMode == "" {
		res.AccessMode = "open"
	}
	if res.UpstreamProtocol == "" {
		res.UpstreamProtocol = proto.UpstreamH2C
	}
	res.LocalHost = "127.0.0.1:50051"
	d.register("app", &res)
	logs := &connLogRec{}
	return d, logs, proxy.New(d, openChecker{}, authDomain, testLog(), append([]proxy.Option{proxy.WithConnLogSink(logs)}, opts...)...)
}

// Unary and bidi streaming calls pass through over h2c and h2 fronts,
// unbuffered: the answer to the first message reaches the caller before the
// second is sent. Trailers arrive, announced and prefixed alike, and the
// response is passed as the service wrote it.
func TestGRPC_UnaryAndBidiUnbuffered(t *testing.T) {
	for _, front := range []string{"h2c", "h2"} {
		for _, prefixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s prefixed=%v", front, prefixed), func(t *testing.T) {
				echo := &grpcEcho{prefixed: prefixed}
				d, logs, p := echoProxy(t, echo, proxy.Resolved{})
				ts, client := grpcFront(t, p, front)

				// Unary.
				req := grpcRequest(t, ts, "app", "/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				msg, err := readGRPCFrame(resp.Body)
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if err != nil || msg != "echo:hi" {
					t.Fatalf("unary: %q %v", msg, err)
				}
				if resp.StatusCode != 200 || resp.Trailer.Get("Grpc-Status") != "0" || resp.Trailer.Get("Grpc-Message") != "done" {
					t.Fatalf("unary: %d trailers %v", resp.StatusCode, resp.Trailer)
				}
				if resp.Header.Get("X-From-Service") != "yes" || resp.Header.Get("Content-Type") != "application/grpc" {
					t.Fatalf("unary: headers %v", resp.Header)
				}
				e := logs.last(t)
				if e.Protocol != "grpc" || e.GRPCMethod != "/pkg.Echo/Unary" || e.GRPCStatus == nil || *e.GRPCStatus != 0 || e.Status != proxy.StatusClosedClean {
					t.Fatalf("unary log %+v", e)
				}

				// Bidi, interleaved.
				pr, pw := io.Pipe()
				defer pw.CloseWithError(io.ErrUnexpectedEOF) // a failing test leaves no call open
				req = grpcRequest(t, ts, "app", "/pkg.Echo/Bidi", pr)
				type result struct {
					resp *http.Response
					err  error
				}
				got := make(chan result, 1)
				go func() {
					resp, err := client.Do(req)
					got <- result{resp, err}
				}()
				if _, err := pw.Write(grpcFrame("one")); err != nil {
					t.Fatal(err)
				}
				var r result
				select {
				case r = <-got:
				case <-time.After(5 * time.Second):
					t.Fatal("no response to the first message while the call is open")
				}
				if r.err != nil {
					t.Fatal(r.err)
				}
				defer r.resp.Body.Close()
				br := bufio.NewReader(r.resp.Body)
				for _, m := range []string{"one", "two", "three"} {
					if m != "one" {
						if _, err := pw.Write(grpcFrame(m)); err != nil {
							t.Fatal(err)
						}
					}
					answer := make(chan string, 1)
					go func() { s, _ := readGRPCFrame(br); answer <- s }()
					select {
					case s := <-answer:
						if s != "echo:"+m {
							t.Fatalf("answer %q to %q", s, m)
						}
					case <-time.After(5 * time.Second):
						t.Fatalf("the answer to %q was held back", m)
					}
				}
				_ = pw.Close()
				if rest, _ := io.ReadAll(br); len(rest) != 0 {
					t.Fatalf("extra body %q", rest)
				}
				if r.resp.Trailer.Get("Grpc-Status") != "0" {
					t.Fatalf("bidi trailers %v", r.resp.Trailer)
				}
				streamsClosed(t, d)
			})
		}
	}
}

// fakeKeys validates one API key.
type fakeKeys struct{ good string }

func (k fakeKeys) ValidateAPIKey(_ context.Context, _, presented string) (string, bool, error) {
	return "key-1", presented == k.good, nil
}

// limitChain stands in for the AI chain's rate limit: it refuses every call
// with 429 and Retry-After, as the quota middleware does.
type limitChain struct{}

func (limitChain) Dispatch(w http.ResponseWriter, _ *http.Request, _, _, _, _ string, _ http.Handler) {
	w.Header().Set("Retry-After", "7")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"error":"rate limit exceeded"}`)
}

// upstreamAnswer is a local service that answers every request with status
// and body, and the given headers.
func upstreamAnswer(status int, body string, hdr ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

// Every refusal reaches a grpc or grpc-web caller as a trailers-only reply:
// HTTP 200, the request's content type, grpc-status and grpc-message, no
// body. Whoever refused: the IP policy, the access check, the rate limit,
// an unknown host, a gateway-only service, a dial error, an HTTP/1.1
// upstream (H4), or the upstream itself. A response that carries
// grpc-status, and any refusal of a request that is not gRPC, pass as they
// are.
func TestGRPC_RefusalsAsStatus(t *testing.T) {
	type tc struct {
		name        string
		res         proxy.Resolved
		host        string // default app
		checker     proxy.AccessChecker
		chain       proxy.AIChain
		local       http.Handler
		contentType string // default application/grpc+proto
		header      map[string]string

		wantHTTP   int
		wantCode   string // "" = no grpc-status expected
		wantMsg    string
		wantExtra  map[string]string
		wantBody   string
		wantRow    string // the connection-log status, "" = no row
		wantRowLog int    // the row's grpc_status
	}
	keys := fakeKeys{good: "sk-good"}
	cases := []tc{
		{name: "ip policy", res: proxy.Resolved{IPBlockCIDRs: []string{"127.0.0.0/8"}},
			wantHTTP: 200, wantCode: "7", wantMsg: "forbidden: ip not allowed", wantRow: proxy.StatusRejected, wantRowLog: 7},
		{name: "api key missing", res: proxy.Resolved{AccessMode: "api_key"}, checker: proxy.NewAccessChecker(keys, authDomain),
			wantHTTP: 200, wantCode: "16", wantMsg: "missing api key", wantRow: proxy.StatusRejected, wantRowLog: 16},
		{name: "api key in metadata", res: proxy.Resolved{AccessMode: "api_key"}, checker: proxy.NewAccessChecker(keys, authDomain),
			header:   map[string]string{"Authorization": "Bearer sk-good"},
			wantHTTP: 200, wantCode: "0", wantRow: proxy.StatusClosedClean},
		{name: "api key wrong", res: proxy.Resolved{AccessMode: "api_key"}, checker: proxy.NewAccessChecker(keys, authDomain),
			header:   map[string]string{"Authorization": "Bearer sk-bad"},
			wantHTTP: 200, wantCode: "16", wantMsg: "invalid api key", wantRow: proxy.StatusRejected, wantRowLog: 16},
		{name: "burrow_login", res: proxy.Resolved{AccessMode: "burrow_login"}, checker: proxy.NewAccessChecker(keys, authDomain),
			wantHTTP: 200, wantCode: "16", wantMsg: "this service needs a browser sign-in; use access mode api_key for gRPC", wantRow: proxy.StatusRejected, wantRowLog: 16},
		{name: "burrow_login grpc-web", res: proxy.Resolved{AccessMode: "burrow_login"}, checker: proxy.NewAccessChecker(keys, authDomain),
			contentType: "application/grpc-web+proto",
			wantHTTP:    200, wantCode: "16", wantRow: proxy.StatusRejected, wantRowLog: 16},
		{name: "rate limit", chain: limitChain{},
			wantHTTP: 200, wantCode: "8", wantMsg: "rate limit exceeded", wantExtra: map[string]string{"Retry-After": "7"}, wantRow: proxy.StatusClosedClean, wantRowLog: 8},
		{name: "unknown host", host: "nope", wantHTTP: 200, wantCode: "12", wantMsg: "tunnel not found"},
		{name: "gateway only", res: proxy.Resolved{GatewayOnly: true}, wantHTTP: 200, wantCode: "12"},
		{name: "dial error", res: proxy.Resolved{ServiceID: "gone"},
			wantHTTP: 200, wantCode: "14", wantMsg: "bad gateway", wantRow: proxy.StatusClosedError, wantRowLog: 14},
		{name: "http1 upstream", res: proxy.Resolved{UpstreamProtocol: proto.UpstreamHTTP1},
			wantHTTP: 200, wantCode: "14", wantMsg: "service upstream is HTTP/1.1; set upstream_protocol h2c or h2 for gRPC", wantRow: proxy.StatusClosedError, wantRowLog: 14},
		{name: "https upstream", res: proxy.Resolved{UpstreamProtocol: proto.UpstreamHTTPS},
			wantHTTP: 200, wantCode: "14", wantRow: proxy.StatusClosedError, wantRowLog: 14},
		{name: "upstream tls failure", res: proxy.Resolved{UpstreamProtocol: proto.UpstreamH2},
			wantHTTP: 200, wantCode: "14", wantMsg: "bad gateway", wantRow: proxy.StatusClosedError, wantRowLog: 14},
		{name: "upstream 503", local: upstreamAnswer(503, "<html>down</html>", "Content-Type", "text/html", "Retry-After", "3"),
			wantHTTP: 200, wantCode: "14", wantMsg: "service unavailable", wantExtra: map[string]string{"Retry-After": "3"}, wantRow: proxy.StatusClosedError, wantRowLog: 14},
		{name: "upstream 404 text", local: upstreamAnswer(404, "no such method\nsecond line"),
			wantHTTP: 200, wantCode: "12", wantMsg: "no such method", wantRow: proxy.StatusClosedClean, wantRowLog: 12},
		{name: "upstream status with grpc-status", local: upstreamAnswer(503, "", "Grpc-Status", "9", "Content-Type", "application/grpc"),
			wantHTTP: 503, wantCode: "9", wantRow: proxy.StatusClosedError, wantRowLog: 9},
		{name: "grpc-web ip policy", res: proxy.Resolved{IPBlockCIDRs: []string{"127.0.0.0/8"}}, contentType: "application/grpc-web-text",
			wantHTTP: 200, wantCode: "7", wantRow: proxy.StatusRejected, wantRowLog: 7},
		{name: "connect stream keeps http", res: proxy.Resolved{IPBlockCIDRs: []string{"127.0.0.0/8"}}, contentType: "application/connect+proto",
			wantHTTP: 403, wantBody: "forbidden: ip not allowed\n", wantRow: proxy.StatusRejected, wantRowLog: -1},
		{name: "json keeps http", res: proxy.Resolved{IPBlockCIDRs: []string{"127.0.0.0/8"}}, contentType: "application/json",
			wantHTTP: 403, wantBody: "forbidden: ip not allowed\n", wantRow: proxy.StatusRejected, wantRowLog: -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			local := c.local
			if local == nil {
				local = &grpcEcho{}
			}
			d := newFakeDialer(local)
			d.h2c = true
			res := c.res
			if res.ServiceID == "" {
				res.ServiceID = "s1"
			}
			if res.AccessMode == "" {
				res.AccessMode = "open"
			}
			if res.UpstreamProtocol == "" {
				res.UpstreamProtocol = proto.UpstreamH2C
			}
			res.LocalHost = "127.0.0.1:50051"
			d.register("app", &res)
			var dialer proxy.StreamDialer = d
			if res.ServiceID == "gone" {
				dialer = failDialer{d} // the lookup finds it, the dial fails
			}
			checker := c.checker
			if checker == nil {
				checker = openChecker{}
			}
			logs := &connLogRec{}
			opts := []proxy.Option{proxy.WithConnLogSink(logs)}
			if c.chain != nil {
				opts = append(opts, proxy.WithAIChain(c.chain))
			}
			ts, client := grpcFront(t, proxy.New(dialer, checker, authDomain, testLog(), opts...), "h2c")
			host := c.host
			if host == "" {
				host = "app"
			}
			req := grpcRequest(t, ts, host, "/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
			ct := c.contentType
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			} else {
				ct = "application/grpc+proto"
			}
			for k, v := range c.header {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != c.wantHTTP {
				t.Fatalf("HTTP %d, want %d (headers %v, body %q)", resp.StatusCode, c.wantHTTP, resp.Header, body)
			}
			code := resp.Header.Get("Grpc-Status")
			if code == "" {
				code = resp.Trailer.Get("Grpc-Status")
			}
			if code != c.wantCode {
				t.Fatalf("grpc-status %q, want %q (headers %v, trailers %v)", code, c.wantCode, resp.Header, resp.Trailer)
			}
			if c.wantCode != "" && c.wantCode != "0" && c.wantHTTP == 200 {
				// Trailers-only: no body, the request's content type, no
				// trailers of the refusal.
				if len(body) != 0 {
					t.Errorf("body %q", body)
				}
				if got := resp.Header.Get("Content-Type"); got != ct {
					t.Errorf("content-type %q, want %q", got, ct)
				}
				if len(resp.Trailer) != 0 {
					t.Errorf("trailers %v", resp.Trailer)
				}
			}
			if c.wantMsg != "" {
				if got := resp.Header.Get("Grpc-Message"); got != encodeMsg(c.wantMsg) {
					t.Errorf("grpc-message %q, want %q", got, encodeMsg(c.wantMsg))
				}
			}
			for k, v := range c.wantExtra {
				if resp.Header.Get(k) != v {
					t.Errorf("%s = %q, want %q", k, resp.Header.Get(k), v)
				}
			}
			if c.wantBody != "" && string(body) != c.wantBody {
				t.Errorf("body %q, want %q", body, c.wantBody)
			}
			if c.wantRow == "" {
				return
			}
			e := logs.last(t)
			if e.Status != c.wantRow {
				t.Errorf("row status %q, want %q (%+v)", e.Status, c.wantRow, e)
			}
			switch {
			case c.wantRowLog < 0:
				if e.GRPCStatus != nil || e.GRPCMethod != "" {
					t.Errorf("a request that is no gRPC call has gRPC fields: %+v", e)
				}
			case e.GRPCStatus == nil || *e.GRPCStatus != c.wantRowLog:
				t.Errorf("row grpc_status %v, want %d", e.GRPCStatus, c.wantRowLog)
			}
		})
	}
}

// encodeMsg percent-encodes as grpc-message does, for the few characters
// the test messages use.
func encodeMsg(s string) string { return strings.ReplaceAll(s, "%", "%25") }

// A gRPC call's relay deadline is its grpc-timeout plus a second: the
// timeout is forwarded unchanged, and a service that never answers is cut
// off with DEADLINE_EXCEEDED.
func TestGRPC_DeadlineCutoff(t *testing.T) {
	var gotTimeout sync.Map
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTimeout.Store("t", r.Header.Get("Grpc-Timeout"))
		<-r.Context().Done()
	})
	d := newFakeDialer(local)
	d.h2c = true
	d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:1", UpstreamProtocol: proto.UpstreamH2C})
	logs := &connLogRec{}
	ts, client := grpcFront(t, proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithConnLogSink(logs)), "h2c")

	req := grpcRequest(t, ts, "app", "/pkg.Slow/Call", strings.NewReader(string(grpcFrame("x"))))
	req.Header.Set("Grpc-Timeout", "200m")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	took := time.Since(start)
	if resp.StatusCode != 200 || resp.Header.Get("Grpc-Status") != "4" {
		t.Fatalf("%d grpc-status %q", resp.StatusCode, resp.Header.Get("Grpc-Status"))
	}
	if took < 1100*time.Millisecond || took > 4*time.Second {
		t.Errorf("cut off after %v, want about 1.2s (grpc-timeout + 1s)", took)
	}
	if v, _ := gotTimeout.Load("t"); v != "200m" {
		t.Errorf("upstream grpc-timeout %v, want 200m unchanged", v)
	}
	e := logs.last(t)
	if e.Status != proxy.StatusClosedError || e.GRPCStatus == nil || *e.GRPCStatus != 4 {
		t.Errorf("row %+v", e)
	}
	streamsClosed(t, d)
}

// grpc_max_duration_s caps every call: a missing or longer grpc-timeout is
// rewritten down to the cap, a shorter one is kept; a stream past the cap is
// reset and logged without a status.
func TestGRPC_MaxDurationCap(t *testing.T) {
	for _, c := range []struct {
		sent, want string
	}{
		{"", "300000u"},
		{"10S", "300000u"},
		{"bogus", "300000u"},
		{"100m", "100m"},
	} {
		t.Run("timeout "+c.sent, func(t *testing.T) {
			echo := &grpcEcho{}
			_, _, p := echoProxy(t, echo, proxy.Resolved{GRPCMaxDuration: 300 * time.Millisecond})
			ts, client := grpcFront(t, p, "h2c")
			req := grpcRequest(t, ts, "app", "/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
			if c.sent != "" {
				req.Header.Set("Grpc-Timeout", c.sent)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if got := echo.lastTimeout(); got != c.want {
				t.Errorf("upstream grpc-timeout %q, want %q", got, c.want)
			}
		})
	}

	t.Run("stream past the cap", func(t *testing.T) {
		local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/grpc")
			w.WriteHeader(200)
			_, _ = w.Write(grpcFrame("first"))
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		})
		d := newFakeDialer(local)
		d.h2c = true
		d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:1",
			UpstreamProtocol: proto.UpstreamH2C, GRPCMaxDuration: 200 * time.Millisecond})
		logs := &connLogRec{}
		ts, client := grpcFront(t, proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithConnLogSink(logs)), "h2c")
		req := grpcRequest(t, ts, "app", "/pkg.Stream/Long", strings.NewReader(string(grpcFrame("x"))))
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if msg, err := readGRPCFrame(resp.Body); err != nil || msg != "first" {
			t.Fatalf("first message %q %v", msg, err)
		}
		_, err = io.ReadAll(resp.Body)
		took := time.Since(start)
		if err == nil && resp.Trailer.Get("Grpc-Status") != "" {
			t.Fatalf("the stream ended with a status: %v", resp.Trailer)
		}
		if took > 4*time.Second {
			t.Errorf("stream cut after %v, want about 1.2s", took)
		}
		e := logs.last(t)
		if e.Status != proxy.StatusClosedError || e.Reason != proxy.ReasonGRPCNoStatus || e.GRPCStatus != nil {
			t.Errorf("row %+v", e)
		}
		streamsClosed(t, d)
	})
}

// The idle timeout, a test seam (0 in production), does not apply to gRPC
// calls: a slow stream outlives it.
func TestGRPC_IdleTimeoutDoesNotApply(t *testing.T) {
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "0")
		w.WriteHeader(200)
	})
	d := newFakeDialer(local)
	d.h2c = true
	d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:1", UpstreamProtocol: proto.UpstreamH2C})
	ts, client := grpcFront(t, proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithProxyIdleTimeout(100*time.Millisecond)), "h2c")
	resp, err := client.Do(grpcRequest(t, ts, "app", "/pkg.Slow/Call", strings.NewReader(string(grpcFrame("x")))))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Grpc-Status") != "0" {
		t.Fatalf("grpc-status %q: the idle timeout cut the call", resp.Header.Get("Grpc-Status"))
	}
}

// The observer and the request summary get the call's code; the row of an
// ordinary request names its protocol and has no gRPC fields.
func TestGRPC_ObserverSummaryAndProtocol(t *testing.T) {
	obs := &trafficObserver{}
	sums := &summarySink{}
	echo := &grpcEcho{}
	_, logs, p := echoProxy(t, echo, proxy.Resolved{}, proxy.WithObserver(obs), proxy.WithSummarySink(sums))
	ts, client := grpcFront(t, p, "h2")
	resp, err := client.Do(grpcRequest(t, ts, "app", "/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi")))))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	ev := obs.one(t)
	if !ev.GRPC || ev.GRPCStatus == nil || *ev.GRPCStatus != 0 {
		t.Errorf("observer %+v", ev)
	}
	s := sums.one(t)
	if s.s.GRPCStatus == nil || *s.s.GRPCStatus != 0 || s.s.Path != "/pkg.Echo/Unary" {
		t.Errorf("summary %+v", s.s)
	}

	// An ordinary request over HTTP/2 and over HTTP/1.1.
	for _, c := range []struct {
		client *http.Client
		want   string
	}{{client, "h2"}, {&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}, "http/1.1"}} { //nolint:gosec // test server
		n := logs.count()
		req, _ := http.NewRequest("GET", ts.URL+"/plain", nil)
		req.Host = "app." + authDomain
		resp, err := c.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if e := logs.after(t, n); e.Protocol != c.want || e.GRPCMethod != "" || e.GRPCStatus != nil {
			t.Errorf("plain request row %+v, want protocol %s", e, c.want)
		}
	}
}

// Trailers pass every writer between the router and the upstream on the
// path-routed door: the chi request logger, the path adapter, the proxy's
// gRPC and counting writers, httpduplex. Over HTTP/1.1 (chunked) and h2c.
func TestGRPC_TrailersThroughSvcPath(t *testing.T) {
	for _, front := range []string{"h1", "h2c"} {
		t.Run(front, func(t *testing.T) {
			echo := &grpcEcho{}
			_, logs, p := echoProxy(t, echo, proxy.Resolved{})
			router := api.NewRouter(api.Deps{TunnelProxy: p, AuthDomain: authDomain, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
			var ts *httptest.Server
			var client *http.Client
			if front == "h1" {
				ts = httptest.NewServer(router)
				t.Cleanup(ts.Close)
				client = ts.Client()
			} else {
				ts, client = grpcFront(t, router, "h2c")
			}
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/svc/app/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
			req.Host = authDomain
			req.Header.Set("Content-Type", "application/grpc")
			req.Header.Set("Te", "trailers")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			msg, _ := readGRPCFrame(resp.Body)
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || msg != "echo:hi" {
				t.Fatalf("%d %q", resp.StatusCode, msg)
			}
			if resp.Trailer.Get("Grpc-Status") != "0" || resp.Trailer.Get("Grpc-Message") != "done" {
				t.Fatalf("trailers %v", resp.Trailer)
			}
			if e := logs.last(t); e.GRPCMethod != "/pkg.Echo/Unary" || e.GRPCStatus == nil || *e.GRPCStatus != 0 {
				t.Errorf("row %+v", e)
			}

			// A refusal on this door is a gRPC status too.
			req, _ = http.NewRequest(http.MethodPost, ts.URL+"/svc/nope/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
			req.Host = authDomain
			req.Header.Set("Content-Type", "application/grpc")
			resp, err = client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get("Grpc-Status") != "12" {
				t.Fatalf("unknown slug: %d grpc-status %q", resp.StatusCode, resp.Header.Get("Grpc-Status"))
			}
		})
	}
}

// A custom domain reaches a service of any upstream protocol, and a gRPC call
// on it gets the same treatment as on the service's own name.
func TestGRPC_CustomDomain(t *testing.T) {
	echo := &grpcEcho{}
	d := newFakeDialer(echo)
	d.h2c = true
	d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:50051", UpstreamProtocol: proto.UpstreamH2C})
	lookup := proxy.WithCustomDomainLookup(func(_ context.Context, host string) (string, bool, error) {
		return "s1", host == "grpc.example.org", nil
	})
	ts, client := grpcFront(t, proxy.New(d, openChecker{}, authDomain, testLog(), lookup), "h2c")
	req := grpcRequest(t, ts, "app", "/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
	req.Host = "grpc.example.org"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := readGRPCFrame(resp.Body)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if msg != "echo:hi" || resp.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("%q trailers %v", msg, resp.Trailer)
	}
	echo.mu.Lock()
	major := echo.major
	echo.mu.Unlock()
	if major != 2 {
		t.Errorf("the service saw HTTP/%d, want h2c", major)
	}

	// An ordinary request over the custom domain, to an h2c service.
	req, _ = http.NewRequest("GET", ts.URL+"/x", nil)
	req.Host = "grpc.example.org"
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusHTTPVersionNotSupported && resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	streamsClosed(t, d)
}

// failDialer finds the tunnel and cannot open a stream to it.
type failDialer struct{ *fakeDialer }

func (failDialer) DialTunnelStreamByServiceID(context.Context, string) (net.Conn, error) {
	return nil, errors.New("tunnel stream refused")
}

// after waits for the connection-log row after the first n and returns it.
func (c *connLogRec) after(t *testing.T, n int) proxy.ConnLogEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.es) > n {
			e := c.es[n]
			c.mu.Unlock()
			return e
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no connection-log row after %d", n)
	return proxy.ConnLogEntry{}
}

func (c *connLogRec) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.es)
}

// grpcWebFrame is a grpc-web body frame: flag 0x00 for a message, 0x80 for
// the trailers block.
func grpcWebFrame(flag byte, payload string) []byte {
	b := make([]byte, 5+len(payload))
	b[0] = flag
	binary.BigEndian.PutUint32(b[1:5], uint32(len(payload)))
	copy(b[5:], payload)
	return b
}

// grpcWebUpstream answers a grpc-web call with body, written in pieces of
// at most chunk bytes, each flushed, so frames span writes.
func grpcWebUpstream(contentType string, body []byte, chunk int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		for len(body) > 0 {
			n := min(chunk, len(body))
			_, _ = w.Write(body[:n])
			w.(http.Flusher).Flush()
			body = body[n:]
		}
	})
}

// grpc-web carries grpc-status in a trailers frame of the body (flag 0x80).
// The proxy reads it as the frames pass, unbuffered, also when a frame
// spans writes: success is closed_clean with status 0, an error code
// promotes the row. grpc-web-text (base64) and a body that is no grpc-web
// framing leave the row at its HTTP outcome, without grpc_no_status.
func TestGRPC_WebStatusFromBody(t *testing.T) {
	msg := grpcWebFrame(0x00, "hello")
	for _, c := range []struct {
		name, ct  string
		body      []byte
		chunk     int
		wantRow   string
		wantCode  int // -1: none
		wantNoRsn bool
	}{
		{"ok", "application/grpc-web+proto", append(append([]byte{}, msg...), grpcWebFrame(0x80, "grpc-status:0\r\ngrpc-message:fine\r\n")...), 1 << 20, proxy.StatusClosedClean, 0, true},
		{"ok split", "application/grpc-web", append(append([]byte{}, msg...), grpcWebFrame(0x80, "Grpc-Status: 0\r\n")...), 3, proxy.StatusClosedClean, 0, true},
		{"error", "application/grpc-web+proto", append(append([]byte{}, msg...), grpcWebFrame(0x80, "grpc-message:boom\r\ngrpc-status:13\r\n")...), 7, proxy.StatusClosedError, 13, true},
		{"no trailers frame", "application/grpc-web+proto", msg, 1 << 20, proxy.StatusClosedError, -1, false},
		{"text mode", "application/grpc-web-text", []byte("AAAAAAVoZWxsbw=="), 1 << 20, proxy.StatusClosedClean, -1, true},
		{"not framing", "application/grpc-web+proto", []byte("\x42garbage that is no frame"), 1 << 20, proxy.StatusClosedClean, -1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newFakeDialer(grpcWebUpstream(c.ct, c.body, c.chunk))
			d.h2c = true
			d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:8080", UpstreamProtocol: proto.UpstreamH2C})
			logs := &connLogRec{}
			ts, client := grpcFront(t, proxy.New(d, openChecker{}, authDomain, testLog(), proxy.WithConnLogSink(logs)), "h2c")
			req := grpcRequest(t, ts, "app", "/pkg.Echo/Unary", strings.NewReader(string(grpcWebFrame(0, "hi"))))
			req.Header.Set("Content-Type", c.ct)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(got) != string(c.body) {
				t.Fatalf("body changed: %q", got)
			}
			e := logs.last(t)
			if e.Protocol != "grpc-web" || e.Status != c.wantRow {
				t.Errorf("row %+v, want status %s", e, c.wantRow)
			}
			switch {
			case c.wantCode < 0 && e.GRPCStatus != nil:
				t.Errorf("grpc_status %d, want none", *e.GRPCStatus)
			case c.wantCode >= 0 && (e.GRPCStatus == nil || *e.GRPCStatus != c.wantCode):
				t.Errorf("grpc_status %v, want %d", e.GRPCStatus, c.wantCode)
			}
			if c.wantNoRsn && e.Reason != "" {
				t.Errorf("reason %q", e.Reason)
			}
			if !c.wantNoRsn && e.Reason != proxy.ReasonGRPCNoStatus {
				t.Errorf("reason %q, want %s", e.Reason, proxy.ReasonGRPCNoStatus)
			}
		})
	}
}

// A grpc-timeout too long for a time.Duration does not wrap the relay's
// deadline into the past: the call runs.
func TestGRPC_HugeTimeout(t *testing.T) {
	echo := &grpcEcho{}
	_, _, p := echoProxy(t, echo, proxy.Resolved{})
	ts, client := grpcFront(t, p, "h2c")
	req := grpcRequest(t, ts, "app", "/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
	req.Header.Set("Grpc-Timeout", "99999999H")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := readGRPCFrame(resp.Body)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if msg != "echo:hi" || resp.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("%q headers %v trailers %v", msg, resp.Header, resp.Trailer)
	}
	if echo.lastTimeout() != "99999999H" {
		t.Errorf("forwarded grpc-timeout %q", echo.lastTimeout())
	}
}

// offlineDialer knows the service, and its tunnel is not connected.
type offlineDialer struct{ *fakeDialer }

func (offlineDialer) Lookup(context.Context, string) (*proxy.Resolved, error) {
	return nil, proxy.ErrTunnelOffline
}

func (offlineDialer) LookupByServiceID(context.Context, string) (*proxy.Resolved, error) {
	return nil, proxy.ErrTunnelOffline
}

// goneDialer finds the tunnel and it is gone when the stream is opened.
type goneDialer struct{ *fakeDialer }

func (goneDialer) DialTunnelStreamByServiceID(context.Context, string) (net.Conn, error) {
	return nil, proxy.ErrNotFound
}

// For gRPC an offline tunnel is UNAVAILABLE, not UNIMPLEMENTED: on lookup,
// by host and by custom domain, and when the tunnel went away before the
// stream was opened. An unknown name stays UNIMPLEMENTED; plain HTTP keeps
// its 404 throughout.
func TestGRPC_OfflineTunnelIsUnavailable(t *testing.T) {
	base := newFakeDialer(&grpcEcho{})
	base.h2c = true
	base.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:1", UpstreamProtocol: proto.UpstreamH2C})
	lookup := proxy.WithCustomDomainLookup(func(_ context.Context, host string) (string, bool, error) {
		return "s1", host == "grpc.example.org", nil
	})
	for _, c := range []struct {
		name   string
		dialer proxy.StreamDialer
		host   string
		want   string
	}{
		{"offline by host", offlineDialer{base}, "app." + authDomain, "14"},
		{"offline by custom domain", offlineDialer{base}, "grpc.example.org", "14"},
		{"gone before the stream", goneDialer{base}, "app." + authDomain, "14"},
		{"gone before the stream, custom domain", goneDialer{base}, "grpc.example.org", "14"},
		{"unknown name", base, "nope." + authDomain, "12"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts, client := grpcFront(t, proxy.New(c.dialer, openChecker{}, authDomain, testLog(), lookup), "h2c")
			req := grpcRequest(t, ts, "app", "/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
			req.Host = c.host
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get("Grpc-Status") != c.want {
				t.Fatalf("%d grpc-status %q, want %s", resp.StatusCode, resp.Header.Get("Grpc-Status"), c.want)
			}
			// Plain HTTP is unchanged: 404.
			req, _ = http.NewRequest("GET", ts.URL+"/", nil)
			req.Host = c.host
			resp, err = client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("plain HTTP: %d, want 404", resp.StatusCode)
			}
		})
	}
}

// A refusal the upstream writes is held to become a status, but not for
// long: past 64 KiB of error body, or a short while, the status is sent and
// the upstream's answer dropped.
func TestGRPC_HeldRefusalIsBounded(t *testing.T) {
	for _, c := range []struct {
		name    string
		timeout time.Duration
		local   http.Handler
	}{
		// The time bound is far off: the byte bound must end it.
		{"endless body", 30 * time.Second, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			chunk := []byte(strings.Repeat("x", 4096))
			for r.Context().Err() == nil {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		})},
		{"stalled body", 300 * time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "down for maintenance")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer proxy.SetHeldRefusalTimeoutForTest(c.timeout)()
			d := newFakeDialer(c.local)
			d.h2c = true
			d.register("app", &proxy.Resolved{ServiceID: "s1", AccessMode: "open", LocalHost: "127.0.0.1:1", UpstreamProtocol: proto.UpstreamH2C})
			ts, client := grpcFront(t, proxy.New(d, openChecker{}, authDomain, testLog()), "h2c")
			req := grpcRequest(t, ts, "app", "/pkg.Echo/Unary", strings.NewReader(string(grpcFrame("hi"))))
			start := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || len(body) != 0 {
				t.Fatalf("body %d bytes, err %v", len(body), err)
			}
			if resp.StatusCode != 200 || resp.Header.Get("Grpc-Status") != "14" {
				t.Fatalf("%d grpc-status %q", resp.StatusCode, resp.Header.Get("Grpc-Status"))
			}
			if took := time.Since(start); took > 3*time.Second {
				t.Fatalf("the status took %v", took)
			}
			streamsClosed(t, d)
		})
	}
}
