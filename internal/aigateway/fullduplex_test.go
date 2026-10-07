package aigateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proxy"
)

// earlyAnswerUpstream answers before it has read the request body: it sends
// "early", then reads the whole body and reports its length.
func earlyAnswerUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "early\n")
		_ = rc.Flush()
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = fmt.Fprintf(w, "got=%d\n", n)
	})
}

// sendInTwoHalves posts a body whose second half is sent only after the first
// line of the response has arrived, and returns the rest of the response. A
// relay that stops reading the request body once the response has started
// never delivers the second half.
func sendInTwoHalves(t *testing.T, req *http.Request, half string) string {
	t.Helper()
	pr, pw := io.Pipe()
	req.Body = pr
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
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
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
	if err != nil {
		t.Fatalf("the response was cut short: %q %v", rest, err)
	}
	return string(rest)
}

// Without a chain nothing buffers the request body. An upstream that answers
// before it has read the body must still get all of it, and the client the
// whole answer: Go's HTTP/1 server would otherwise consume the rest of the
// body when the response headers go out.
func TestServe_UpstreamAnswersBeforeTheBodyIsRead_NoChain(t *testing.T) {
	up := httptest.NewServer(earlyAnswerUpstream())
	defer up.Close()

	g := newGateway(nil, nil)
	g.Tunnels = streamTunnels{
		res:  &proxy.Resolved{ServiceID: "svc1", AccessMode: "api_key", LocalHost: "127.0.0.1:11434"},
		addr: up.Listener.Addr().String(),
	}
	// The server's own request body, unbuffered, as in production.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.Serve(w, r, "ollama")
	}))
	defer front.Close()

	half := strings.Repeat("x", 4096)
	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	if rest := sendInTwoHalves(t, req, half); rest != fmt.Sprintf("got=%d\n", 2*len(half)) {
		t.Fatalf("rest = %q, want the upstream to have read all %d bytes", rest, 2*len(half))
	}
}
