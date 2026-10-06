package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
)

const (
	authWithSummaries = `{"protocol_version":1,"token":"bur_test_0000","client_version":"0.7.0","os":"linux","arch":"amd64","capabilities":["request_summaries"]}`
	authOfBefore      = `{"protocol_version":1,"token":"bur_test_0000","client_version":"0.6.0","os":"linux","arch":"amd64"}`
)

// dialRaw connects with the auth request written out in auth and opens the
// control stream.
func dialRaw(t *testing.T, s *Server, pool *x509.CertPool, auth string) *oldClient {
	t.Helper()
	conn, err := tls.Dial("tcp", s.Addr(), &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	sendRaw(t, conn, proto.MsgAuthRequest, auth)
	if raw := readRaw(t, conn, proto.MsgAuthResponse); !strings.HasPrefix(raw, `{"ok":true`) {
		t.Fatalf("auth: %s", raw)
	}
	c := &oldClient{t: t, conn: conn}
	c.openControl()
	return c
}

// nothingBut sends a ping and expects its pong as the very next message: the
// relay has sent nothing else in between.
func (c *oldClient) nothingBut(nonce string) {
	c.t.Helper()
	sendRaw(c.t, c.ctrl, proto.MsgPing, `{"nonce":"`+nonce+`"}`)
	if got := readRaw(c.t, c.ctrl, proto.MsgPong); got != `{"nonce":"`+nonce+`"}` {
		c.t.Fatalf("pong = %s", got)
	}
}

func summaryOf(tunnelID string) proto.RequestSummary {
	return proto.RequestSummary{TunnelID: tunnelID, Time: "2026-10-05T14:02:11Z", Method: "GET", Path: "/api/users", Status: 200, DurationMs: 12}
}

func TestSummary_GoesToTheClientThatAnnouncedAndOwnsTheTunnel(t *testing.T) {
	s, pool := newRelay(t, "")

	asked := dialRaw(t, s, pool, authWithSummaries)
	_, web := asked.register(`{"name":"web","type":"http","remote_port":0,"local_addr":"127.0.0.1:3000"}`)
	_, tcp := asked.register(`{"name":"echo","type":"tcp","remote_port":0,"local_addr":"127.0.0.1:9"}`)
	if !web.OK || !tcp.OK {
		t.Fatalf("registrations: %+v %+v", web, tcp)
	}
	// Another client of the same user, which asked too, with another service.
	other := dialRaw(t, s, pool, authWithSummaries)
	_, api := other.register(`{"name":"api","type":"http","remote_port":0,"local_addr":"127.0.0.1:3001"}`)
	// A client of before: it announces nothing.
	old := dialRaw(t, s, pool, authOfBefore)
	_, legacy := old.register(`{"name":"legacy","type":"http","remote_port":0,"local_addr":"127.0.0.1:3002"}`)
	if !api.OK || !legacy.OK {
		t.Fatalf("registrations: %+v %+v", api, legacy)
	}

	s.RequestSummary("svc-web", summaryOf(web.TunnelID))
	want := `{"tunnel_id":"` + web.TunnelID + `","time":"2026-10-05T14:02:11Z","method":"GET","path":"/api/users","status":200,"duration_ms":12}`
	if got := readRaw(t, asked.ctrl, proto.MsgRequestSummary); got != want {
		t.Fatalf("summary\n got %s\nwant %s", got, want)
	}
	// The proxy may not know the tunnel; the service's live tunnel is it then.
	s.RequestSummary("svc-web", summaryOf(""))
	if got := readRaw(t, asked.ctrl, proto.MsgRequestSummary); got != want {
		t.Fatalf("summary by service\n got %s\nwant %s", got, want)
	}

	// None of these reaches anyone:
	s.RequestSummary("svc-legacy", summaryOf(legacy.TunnelID)) // the client did not announce the capability
	s.RequestSummary("svc-legacy", summaryOf(""))
	s.RequestSummary("", summaryOf(tcp.TunnelID))        // a tcp tunnel
	s.RequestSummary("svc-web", summaryOf(tcp.TunnelID)) // a tcp tunnel under an http service's name
	s.RequestSummary("svc-api", summaryOf(web.TunnelID)) // a tunnel that does not serve that service
	s.RequestSummary("svc-web", summaryOf("no-such-tunnel"))
	s.RequestSummary("svc-nobody", summaryOf(""))
	s.RequestSummary("", summaryOf(""))
	// and one for the other client's service reaches that client only.
	s.RequestSummary("svc-api", summaryOf(api.TunnelID))
	if got := readRaw(t, other.ctrl, proto.MsgRequestSummary); !strings.Contains(got, `"tunnel_id":"`+api.TunnelID+`"`) {
		t.Fatalf("the other client got %s", got)
	}
	time.Sleep(50 * time.Millisecond)
	asked.nothingBut("a")
	other.nothingBut("b")
	old.nothingBut("c")
}

// Names of capabilities this relay does not know are not a reason to refuse,
// and do not turn anything on.
func TestSummary_UnknownCapabilities(t *testing.T) {
	s, pool := newRelay(t, "")
	c := dialRaw(t, s, pool, `{"protocol_version":1,"token":"bur_test_0000","client_version":"0.9.0","os":"linux","arch":"amd64","capabilities":["later","request_summaries_v2",""]}`)
	_, web := c.register(`{"name":"web","type":"http","remote_port":0,"local_addr":"127.0.0.1:3000"}`)
	s.RequestSummary("svc-web", summaryOf(web.TunnelID))
	time.Sleep(30 * time.Millisecond)
	c.nothingBut("a")
	for _, cs := range s.reg.Sessions() {
		if cs.WantsRequestSummaries() {
			t.Fatal("summaries are on for a client that did not ask")
		}
	}
}

// A client that does not read its control stream costs the requests nothing:
// offering a summary returns at once, at most summaryQueue wait, the rest is
// dropped and counted, and the session's sender ends with the session.
func TestSummary_StalledControlStream(t *testing.T) {
	before := runtime.NumGoroutine()
	pr, pw := io.Pipe() // nobody reads: every write blocks
	cs := &ClientSession{SessionID: "s1", summaries: make(chan proto.RequestSummary, summaryQueue)}
	cs.SetControl(pw)
	done := make(chan struct{})
	ended := make(chan struct{})
	go func() { defer close(ended); cs.runSummaries(done) }()

	var slowest time.Duration
	for i := 0; i < 10000; i++ {
		start := time.Now()
		cs.offerSummary(summaryOf("t1"))
		if d := time.Since(start); d > slowest {
			slowest = d
		}
		if n := len(cs.summaries); n > summaryQueue {
			t.Fatalf("%d summaries wait, the limit is %d", n, summaryQueue)
		}
	}
	if slowest > 50*time.Millisecond {
		t.Fatalf("offering a summary took %v behind a stalled control stream", slowest)
	}
	if cap(cs.summaries) != 256 {
		t.Fatalf("queue of %d", cap(cs.summaries))
	}
	// One is in the blocked write, 256 wait, all others were dropped.
	if d := cs.SummariesDropped(); d < 10000-summaryQueue-1 || d >= 10000 {
		t.Fatalf("%d of 10000 were dropped", d)
	}

	// The session ends: its stream is closed, which ends the blocked write.
	close(done)
	_ = pr.Close()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the sender did not end with the session")
	}
	// After that a summary is dropped without a word.
	cs.offerSummary(summaryOf("t1"))
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("%d goroutines before, %d after", before, n)
	}
}

// The same against the relay itself: a real client that stops reading. The
// requests are not held up, and when the client is gone nothing of its
// session is left.
func TestSummary_StalledClientOfTheRelay(t *testing.T) {
	s, pool := newRelay(t, "")
	before := runtime.NumGoroutine()
	c := dialRaw(t, s, pool, authWithSummaries)
	_, web := c.register(`{"name":"web","type":"http","remote_port":0,"local_addr":"127.0.0.1:3000"}`)
	if !web.OK {
		t.Fatal("not registered")
	}
	var cs *ClientSession
	for _, one := range s.reg.Sessions() {
		cs = one
	}
	if cs == nil || !cs.WantsRequestSummaries() {
		t.Fatal("no session that wants summaries")
	}
	// The client reads nothing from here on.
	sum := summaryOf(web.TunnelID)
	sum.Path = "/" + strings.Repeat("a", 255)
	var slowest time.Duration
	for i := 0; i < 10000; i++ {
		start := time.Now()
		s.RequestSummary("svc-web", sum)
		if d := time.Since(start); d > slowest {
			slowest = d
		}
	}
	if slowest > 50*time.Millisecond {
		t.Fatalf("a request waited %v for a client that does not read", slowest)
	}
	if cs.SummariesDropped() == 0 {
		t.Fatal("10000 summaries of 300 bytes for a client that does not read, and none was dropped")
	}
	if n := len(cs.summaries); n > summaryQueue {
		t.Fatalf("%d summaries wait", n)
	}

	_ = c.sess.Close()
	_ = c.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (len(s.reg.Sessions()) > 0 || runtime.NumGoroutine() > before) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(s.reg.Sessions()) != 0 {
		t.Fatal("the session is still there")
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("%d goroutines before the client, %d after it left", before, n)
	}
}

// A session that did not ask has no queue and no sender.
func TestSummary_NothingRunsForAClientThatDidNotAsk(t *testing.T) {
	cs := &ClientSession{SessionID: "s1"}
	if cs.WantsRequestSummaries() || cs.offerSummary(summaryOf("t1")) || cs.SummariesDropped() != 0 {
		t.Fatal("a session without the capability took a summary")
	}
}

// What is sent is the summary and nothing around it.
func TestSummary_Envelope(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	cs := &ClientSession{SessionID: "s1", summaries: make(chan proto.RequestSummary, summaryQueue)}
	cs.SetControl(pw)
	done := make(chan struct{})
	defer close(done)
	go cs.runSummaries(done)
	if !cs.offerSummary(summaryOf("t1")) {
		t.Fatal("not taken")
	}
	var env proto.Envelope
	if err := proto.ReadFrame(pr, &env); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	if want := `{"type":"request_summary","payload":{"tunnel_id":"t1","time":"2026-10-05T14:02:11Z","method":"GET","path":"/api/users","status":200,"duration_ms":12}}`; string(raw) != want {
		t.Fatalf("got %s\nwant %s", raw, want)
	}
}
