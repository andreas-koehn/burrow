package proxy_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
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
