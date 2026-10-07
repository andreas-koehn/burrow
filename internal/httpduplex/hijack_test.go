package httpduplex

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A handler that hijacks a request with a body and returns while a goroutine
// keeps the connection: the connection stays usable. Serve must not set a
// read deadline on it, read from it or have the server close it.
func TestServe_HijackedConnectionIsLeftAlone(t *testing.T) {
	shortSettle(t)
	returned := make(chan struct{})
	result := make(chan string, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		go func() {
			defer conn.Close()
			<-returned
			// Well after the handler returned: the 5 bytes of the request
			// body, never read by the handler, then a line the client sends
			// only now.
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			body := make([]byte, 5)
			if _, err := io.ReadFull(brw, body); err != nil {
				result <- "read body: " + err.Error()
				return
			}
			line, err := brw.ReadString('\n')
			if err != nil {
				result <- "read line: " + err.Error()
				return
			}
			_, _ = brw.WriteString("got " + string(body) + " " + line)
			if err := brw.Flush(); err != nil {
				result <- "write: " + err.Error()
				return
			}
			result <- "ok"
		}()
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Serve(w, r, h)
		close(returned)
	}))
	t.Cleanup(srv.Close)

	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, "POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\n\r\n01234"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return")
	}
	// Longer than the time Serve gives a body to arrive: whatever it would
	// do to the connection, it has done by now.
	time.Sleep(settleTimeout + 200*time.Millisecond)
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	if got := <-result; got != "ok" {
		t.Fatalf("the hijacked connection was broken: %s", got)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "got 01234 ping\n" {
		t.Fatalf("answer on the hijacked connection = %q err = %v", line, err)
	}
}

// noter is a writer with the optional method a provider's upstream handler
// looks for.
type noter struct {
	http.ResponseWriter
	noted bool
}

func (n *noter) NoteUpstreamTimeout()        { n.noted = true }
func (n *noter) Unwrap() http.ResponseWriter { return n.ResponseWriter }

// The writer the handler gets still reaches what the caller's writer offers:
// the response controller (flush, deadlines), http.Flusher and
// NoteUpstreamTimeout.
func TestServe_OptionalInterfacesStayReachable(t *testing.T) {
	var outer *noter
	var problems []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, guarded := r.Body.(*guard); !guarded {
			problems = append(problems, "the body is not guarded: this request did not take the wrapped path")
		}
		tn, ok := w.(interface{ NoteUpstreamTimeout() })
		if !ok {
			problems = append(problems, "NoteUpstreamTimeout is hidden")
		} else {
			tn.NoteUpstreamTimeout()
		}
		if _, ok := w.(http.Flusher); !ok {
			problems = append(problems, "http.Flusher is hidden")
		}
		if _, ok := w.(http.Hijacker); !ok {
			problems = append(problems, "http.Hijacker is hidden")
		}
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			problems = append(problems, "SetWriteDeadline: "+err.Error())
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "first\n")
		if err := rc.Flush(); err != nil {
			problems = append(problems, "Flush: "+err.Error())
		}
		_, _ = io.WriteString(w, "second\n")
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outer = &noter{ResponseWriter: w}
		Serve(outer, r, h)
	}))
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "first\nsecond\n" {
		t.Fatalf("body = %q err = %v", body, err)
	}
	if len(problems) != 0 {
		t.Fatal(strings.Join(problems, "; "))
	}
	if !outer.noted {
		t.Fatal("the note did not reach the caller's writer")
	}
}
