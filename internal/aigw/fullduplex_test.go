package aigw_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
)

// A service without AI config is passed through with its request body
// unbuffered. An upstream that answers before it has read the body must still
// get all of it, and the client the whole answer: Go's HTTP/1 server would
// otherwise consume the rest of the body when the response headers go out,
// the forward would send a short body and the response would be cut off.
func TestChain_PassThrough_UpstreamAnswersBeforeTheBodyIsRead(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "early\n")
		_ = rc.Flush()
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = fmt.Fprintf(w, "got=%d\n", n)
	})
	chain := aigw.NewChain(nil, nil, nil, nil, nil, nil, nil, nil, testLog())
	// No Loader: every service is pass-through. The body is the server's own.
	srv := runChainOverServer(t, chain, upstream)

	for _, method := range []string{http.MethodPut, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
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
			req, _ := http.NewRequest(method, srv.URL+"/upload", pr)
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
