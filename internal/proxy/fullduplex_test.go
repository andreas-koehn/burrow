package proxy_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proxy"
)

// The tunnel proxy forwards the request body as it arrives. An app that
// answers before it has read the body must still get all of it, and the
// visitor the whole answer: Go's HTTP/1 server would otherwise consume the
// rest of the body when the response headers go out, the forward would send a
// short body and the response would be cut off.
func TestProxy_UpstreamAnswersBeforeTheBodyIsRead(t *testing.T) {
	lookup := func(_ context.Context, host string) (string, bool, error) {
		return "svc1", host == "app.example.org", nil
	}
	for _, tc := range []struct {
		name string
		host string
		opts []proxy.Option
	}{
		{name: "host route", host: "abc123." + authDomain},
		{name: "through the AI chain", host: "abc123." + authDomain, opts: []proxy.Option{proxy.WithAIChain(passChain{})}},
		{name: "custom domain", host: "app.example.org", opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup)}},
		{name: "custom domain through the AI chain", host: "app.example.org", opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup), proxy.WithAIChain(passChain{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDialer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rc := http.NewResponseController(w)
				_ = rc.EnableFullDuplex()
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "early\n")
				_ = rc.Flush()
				n, _ := io.Copy(io.Discard, r.Body)
				_, _ = fmt.Fprintf(w, "got=%d\n", n)
			}))
			d.register("abc123", &proxy.Resolved{ServiceID: "svc1", AccessMode: "open", LocalHost: "127.0.0.1:3000"})
			ts := httptest.NewServer(proxy.New(d, openChecker{}, authDomain, testLog(), tc.opts...))
			defer ts.Close()

			half := strings.Repeat("x", 4096)
			pr, pw := io.Pipe()
			sawEarly := make(chan struct{})
			go func() {
				_, _ = io.WriteString(pw, half)
				select {
				case <-sawEarly:
					_, _ = io.WriteString(pw, half)
					_ = pw.Close()
				case <-time.After(20 * time.Second):
					_ = pw.CloseWithError(fmt.Errorf("the response never started"))
				}
			}()
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/upload", pr)
			req.Host = tc.host
			resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
			if err != nil {
				t.Fatalf("the response did not start while the request body was still being sent: %v", err)
			}
			defer resp.Body.Close()
			br := bufio.NewReader(resp.Body)
			if line, err := br.ReadString('\n'); line != "early\n" {
				t.Fatalf("first line = %q err = %v (status %d)", line, err, resp.StatusCode)
			}
			close(sawEarly)
			rest, err := io.ReadAll(br)
			if want := fmt.Sprintf("got=%d\n", 2*len(half)); err != nil || string(rest) != want {
				t.Fatalf("rest = %q err = %v, want %q", rest, err, want)
			}
		})
	}
}

// earlyOK answers "ok" at once and never reads the request body. The answer
// has no Content-Length: it ends with the final chunk, which the relay's
// server writes when the proxy's handler has returned.
var earlyOK = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	// A stand-in that leaves the body unread under plain full duplex must not
	// keep its connection (see the package comment of httpduplex).
	w.Header().Set("Connection", "close")
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	_, _ = io.WriteString(w, "ok")
	_ = rc.Flush()
})

// rawUpload opens a connection to the relay and sends head (a request line,
// headers and the first part of a body).
func rawUpload(t *testing.T, ts *httptest.Server, host, head string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := io.WriteString(conn, strings.Replace(head, "{host}", host, 1)); err != nil {
		t.Fatal(err)
	}
	return conn, bufio.NewReader(conn)
}

// completeOK reads one response and requires it complete.
func completeOK(t *testing.T, br *bufio.Reader, status int, body string) {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != status || string(got) != body {
		t.Fatalf("the response is not complete: status %d body %q err %v", resp.StatusCode, got, err)
	}
}

// connClosed requires that the relay closes the connection without sending
// anything more.
func connClosed(t *testing.T, conn net.Conn, br *bufio.Reader, within time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	if _, err := br.Peek(1); err == nil {
		rest, _ := br.Peek(br.Buffered())
		t.Fatalf("the connection was kept and sent more: %q", rest)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("the connection is still open %v after the response", within)
	}
}

// An app that answers an upload early, through the real proxy. Whatever the
// visitor does with the rest of its body, it gets the complete answer; the
// connection is used again only when the body arrived in time.
func TestProxy_UploadAnsweredEarly(t *testing.T) {
	lookup := func(_ context.Context, host string) (string, bool, error) {
		return "svc1", host == "app.example.org", nil
	}
	routes := []struct {
		name string
		host string
		opts []proxy.Option
	}{
		{name: "host route", host: "abc123." + authDomain},
		{name: "custom domain through the AI chain", host: "app.example.org", opts: []proxy.Option{proxy.WithCustomDomainLookup(lookup), proxy.WithAIChain(passChain{})}},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			t.Run("the visitor sends the rest after the answer began: answered, connection reused", func(t *testing.T) {
				d, ts := leakStack(t, earlyOK, "", rt.opts...)
				conn, br := rawUpload(t, ts, rt.host, "POST /upload HTTP/1.1\r\nHost: {host}\r\nContent-Length: 10\r\n\r\n01234")
				// The second half goes out only once the answer's headers
				// are here: the app has answered with half the body unread,
				// and the relay has to take in the rest itself to keep the
				// connection.
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("no answer while half the body was outstanding: %v", err)
				}
				_, _ = io.WriteString(conn, "56789")
				got, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != 200 || string(got) != "ok" {
					t.Fatalf("the response is not complete: status %d body %q err %v", resp.StatusCode, got, err)
				}
				// The next request on the same connection is served.
				_, _ = io.WriteString(conn, "GET /again HTTP/1.1\r\nHost: "+rt.host+"\r\n\r\n")
				completeOK(t, br, 200, "ok")
				_ = conn.Close()
				if opened, closed := d.settled(); opened != 2 || closed != opened {
					t.Fatalf("%d streams opened, %d closed", opened, closed)
				}
			})
			t.Run("the visitor stops sending: answered within the bound, connection closed", func(t *testing.T) {
				d, ts := leakStack(t, earlyOK, "", rt.opts...)
				start := time.Now()
				conn, br := rawUpload(t, ts, rt.host, "POST /upload HTTP/1.1\r\nHost: {host}\r\nTransfer-Encoding: chunked\r\n\r\n5\r\n01234\r\n")
				_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
				completeOK(t, br, 200, "ok")
				if el := time.Since(start); el > 15*time.Second {
					t.Fatalf("the answer took %v", el)
				}
				connClosed(t, conn, br, 10*time.Second)
				if opened, closed := d.settled(); opened != 1 || closed != opened {
					t.Fatalf("%d streams opened, %d closed", opened, closed)
				}
			})
			t.Run("the visitor sends an endless chunked body: answered, connection closed", func(t *testing.T) {
				d, ts := leakStack(t, earlyOK, "", rt.opts...)
				conn, br := rawUpload(t, ts, rt.host, "POST /upload HTTP/1.1\r\nHost: {host}\r\nTransfer-Encoding: chunked\r\n\r\n")
				stop := make(chan struct{})
				defer close(stop)
				go func() {
					chunk := fmt.Sprintf("%x\r\n%s\r\n", 8192, strings.Repeat("c", 8192))
					for {
						select {
						case <-stop:
							return
						default:
						}
						if _, err := io.WriteString(conn, chunk); err != nil {
							return
						}
					}
				}()
				_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
				completeOK(t, br, 200, "ok")
				connClosed(t, conn, br, 10*time.Second)
				if opened, closed := d.settled(); opened != 1 || closed != opened {
					t.Fatalf("%d streams opened, %d closed", opened, closed)
				}
			})
		})
	}
}

// An upload announced with "Expect: 100-continue" that the app refuses at
// once: the visitor gets the complete refusal and the connection is closed.
// (The tunnel's transport sends the request on at once, so the visitor may be
// asked for the body; it is not waited for.)
func TestProxy_ExpectContinueUploadRefusedEarly(t *testing.T) {
	refuse := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A stand-in that leaves the body unread under plain full duplex must not
		// keep its connection (see the package comment of httpduplex).
		w.Header().Set("Connection", "close")
		_ = http.NewResponseController(w).EnableFullDuplex()
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "no")
	})
	d, ts := leakStack(t, refuse, "")
	host := "abc123." + authDomain
	start := time.Now()
	conn, br := rawUpload(t, ts, host, "POST /upload HTTP/1.1\r\nHost: {host}\r\nExpect: 100-continue\r\nContent-Length: 100000\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	if strings.HasPrefix(line, "HTTP/1.1 100") {
		// Asked for the body; the visitor does not send it. Skip the blank line.
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		line = ""
	}
	completeOK(t, bufio.NewReader(io.MultiReader(strings.NewReader(line), br)), 401, "no")
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("the refusal took %v", el)
	}
	connClosed(t, conn, br, 10*time.Second)
	if opened, closed := d.settled(); opened != 1 || closed != opened {
		t.Fatalf("%d streams opened, %d closed", opened, closed)
	}
}
