package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
)

// newRegister answers a registration as a relay of this code base does.
func newRegister(req proto.TunnelRegister) string {
	if req.Type == "tcp" {
		return `{"ok":true,"tunnel_id":"tcp-` + req.Name + `","remote_port":9000}`
	}
	return `{"ok":true,"tunnel_id":"t-` + req.Name + `","url":"https://burrow.example.com/svc/abc234/","access_mode":"open"}`
}

// summaryClient runs a client that asks for request summaries (or not) against
// a relay that speaks in text, and returns once its tunnels are registered and
// its first ping was answered: from then on the relay's side of the control
// stream is the test's alone.
func summaryClient(t *testing.T, ask bool, log *slog.Logger) (*rawRelay, *recObserver) {
	t.Helper()
	r := startRawRelay(t, oldAuthOK, newRegister)
	obs := &recObserver{}
	o := r.options(TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"}, TunnelSpec{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432"})
	o.Observer, o.RequestSummaries = obs, ask
	if log != nil {
		o.Logger = log
	}
	c := New(o)
	runUntilDone(t, c)
	if !waitTrue(func() bool { return c.Registered() && obs.has("latency") }, 3*time.Second) {
		t.Fatalf("not ready: %v", obs.snapshot())
	}
	return r, obs
}

func summaryJSON(tunnel, at, method, path string, status int) string {
	b, _ := json.Marshal(map[string]any{"tunnel_id": tunnel, "time": at, "method": method, "path": path, "status": status, "duration_ms": 3})
	return string(b)
}

// The capability is announced by a client that was asked to, and by no other:
// `burrow connect` and every run that prints log lines send the auth request
// they always sent.
func TestSummaries_AnnouncedOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ask      bool
		observer bool
		want     bool
	}{
		{"asked, with an observer", true, true, true},
		{"not asked", false, true, false},
		{"asked without anyone to tell", true, false, false},
		{"neither", false, false, false},
	} {
		r := startRawRelay(t, oldAuthOK, oldRegister)
		o := r.options(TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
		o.RequestSummaries = tc.ask
		if tc.observer {
			o.Observer = &recObserver{}
		}
		c := New(o)
		runUntilDone(t, c)
		if !waitTrue(c.Registered, 3*time.Second) {
			t.Fatalf("%s: never registered", tc.name)
		}
		auths, _ := r.sent()
		const tail = `,"capabilities":["request_summaries"]}`
		if got := strings.HasSuffix(auths[0], tail); got != tc.want {
			t.Errorf("%s: auth request %s", tc.name, strings.Replace(auths[0], "bur_test_0000", "…", 1))
		}
		var keys map[string]json.RawMessage
		_ = json.Unmarshal([]byte(auths[0]), &keys)
		if want := map[bool]int{true: 6, false: 5}[tc.want]; len(keys) != want {
			t.Errorf("%s: %d fields in the auth request, want %d", tc.name, len(keys), want)
		}
	}
}

func TestSummaries_ReachTheObserver(t *testing.T) {
	r, obs := summaryClient(t, true, nil)
	r.push(t, proto.MsgRequestSummary, `{"tunnel_id":"t-web","time":"2026-10-05T14:02:11Z","method":"GET","path":"/api/users","status":200,"duration_ms":12}`)
	r.push(t, proto.MsgRequestSummary, `{"tunnel_id":"t-web","time":"2026-10-05T16:02:12+02:00","method":"POST","path":"/api/login","status":401,"duration_ms":0,"later":true}`)
	if !waitTrue(func() bool { return obs.count("request:") == 2 }, 3*time.Second) {
		t.Fatalf("events: %v", obs.snapshot())
	}
	if !obs.has("request:t-web:GET:/api/users:200") || !obs.has("request:t-web:POST:/api/login:401") {
		t.Fatalf("events: %v", obs.snapshot())
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if want := time.Date(2026, 10, 5, 14, 2, 11, 0, time.UTC); !obs.reqAt[0].Equal(want) || obs.reqAt[0].Location() != time.Local {
		t.Errorf("time %v (%v), want %v in local time", obs.reqAt[0], obs.reqAt[0].Location(), want)
	}
	if want := time.Date(2026, 10, 5, 14, 2, 12, 0, time.UTC); !obs.reqAt[1].Equal(want) {
		t.Errorf("time %v, want %v", obs.reqAt[1], want)
	}
}

// What is not a summary of a request to one of this session's http tunnels is
// dropped: it is not shown, and the client goes on.
func TestSummaries_MalformedAreDropped(t *testing.T) {
	r, obs := summaryClient(t, true, nil)
	const at = "2026-10-05T14:02:11Z"
	for _, bad := range []string{
		summaryJSON("t-web", "yesterday", "GET", "/", 200),
		summaryJSON("t-web", "", "GET", "/", 200),
		summaryJSON("t-web", at, "", "/", 200),
		summaryJSON("t-web", at, "GET", "/", 0),
		summaryJSON("t-web", at, "GET", "/", 99),
		summaryJSON("t-web", at, "GET", "/", 600),
		summaryJSON("t-web", at, "GET", "/", -200),
		summaryJSON("", at, "GET", "/", 200),
		summaryJSON("t-other", at, "GET", "/", 200),
		summaryJSON("tcp-pg", at, "GET", "/", 200), // a tcp tunnel has no requests
		`{"tunnel_id":"t-web","time":"` + at + `","method":"GET","path":"/","status":"200"}`,
		`[1,2,3]`, `"text"`, `null`, `{}`,
	} {
		r.push(t, proto.MsgRequestSummary, bad)
	}
	r.push(t, proto.MsgRequestSummary, summaryJSON("t-web", at, "GET", "/after", 204))
	if !waitTrue(func() bool { return obs.has("request:t-web:GET:/after:204") }, 3*time.Second) {
		t.Fatalf("the client did not go on: %v", obs.snapshot())
	}
	if n := obs.count("request:"); n != 1 {
		t.Fatalf("%d summaries were shown: %v", n, obs.snapshot())
	}
}

// The relay's words are bounded and made printable again, whatever the relay
// did with them.
func TestSummaries_TextIsBoundedAndSanitisedAgain(t *testing.T) {
	r, obs := summaryClient(t, true, nil)
	const at = "2026-10-05T14:02:11Z"
	r.push(t, proto.MsgRequestSummary, summaryJSON("t-web", at, "GE\x1b[31mT", "/a\x1b[2J\r\nb\u202ec\u200bd", 200))
	r.push(t, proto.MsgRequestSummary, summaryJSON("t-web", at, strings.Repeat("M", 500), "/"+strings.Repeat("p", 100000), 200))
	r.push(t, proto.MsgRequestSummary, summaryJSON("t-web", at, "GET", "", 200))
	if !waitTrue(func() bool { return obs.count("request:") == 3 }, 3*time.Second) {
		t.Fatalf("events: %v", obs.snapshot())
	}
	ev := obs.snapshot()
	var got []string
	for _, e := range ev {
		if strings.HasPrefix(e, "request:") {
			got = append(got, e)
		}
	}
	want := []string{
		"request:t-web:GE??31mT:/a?[2J??b?c?d:200", // "[" is not part of a method either
		"request:t-web:" + strings.Repeat("M", 16) + ":/" + strings.Repeat("p", 255) + ":200",
		"request:t-web:GET:/:200",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("summary %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

// A client that did not ask shows none, should a relay send one all the same.
func TestSummaries_NotAskedNotShown(t *testing.T) {
	r, obs := summaryClient(t, false, nil)
	r.push(t, proto.MsgRequestSummary, summaryJSON("t-web", "2026-10-05T14:02:11Z", "GET", "/", 200))
	r.push(t, proto.MsgPong, `{"nonce":"hb"}`)
	time.Sleep(100 * time.Millisecond)
	if obs.has("request:") {
		t.Fatalf("events: %v", obs.snapshot())
	}
}

// recordingHandler keeps the log records of warn and above.
type recordingHandler struct {
	mu   sync.Mutex
	warn []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.mu.Lock()
		h.warn = append(h.warn, r.Message)
		h.mu.Unlock()
	}
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// A newer relay may send a message this client does not know. It is passed
// over without a warning in the log, and the client goes on. The same goes
// for a summary nobody asked for, and for one that cannot be read.
func TestClient_UnknownMessageTypeIsIgnoredQuietly(t *testing.T) {
	for _, ask := range []bool{true, false} {
		h := &recordingHandler{}
		r, obs := summaryClient(t, ask, slog.New(h))
		r.push(t, "message_of_a_later_version", `{"anything":["at","all"]}`)
		r.push(t, "", `null`)
		r.push(t, proto.MsgRequestSummary, `{"tunnel_id":7}`)
		r.push(t, proto.MsgRequestSummary, summaryJSON("t-web", "2026-10-05T14:02:11Z", "GET", "/still-here", 200))
		r.push(t, proto.MsgNewConnection, `{"tunnel_id":"t-nobody","stream_id":"s","source_ip":"203.0.113.7:1"}`)
		if ask {
			if !waitTrue(func() bool { return obs.has("request:t-web:GET:/still-here:200") }, 3*time.Second) {
				t.Fatalf("the client did not go on: %v", obs.snapshot())
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
		if obs.has("state:reconnecting") {
			t.Fatalf("the connection ended: %v", obs.snapshot())
		}
		h.mu.Lock()
		if len(h.warn) != 0 {
			t.Errorf("ask %v: warnings in the log: %q", ask, h.warn)
		}
		h.mu.Unlock()
	}
}

// 5000 summaries at once, behind an observer that needs a moment for each:
// the reader of the control stream is not held up (the pong that follows them
// is read), what waits for the observer stays bounded, and what the view
// cannot do without — the state and the registrations — is not pushed out by
// the lines.
func TestSummaries_BurstBehindASlowObserver(t *testing.T) {
	r := startRawRelay(t, oldAuthOK, newRegister)
	obs := &recObserver{delay: 2 * time.Millisecond}
	o := r.options(TunnelSpec{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"})
	o.Observer, o.RequestSummaries = obs, true
	c := New(o)
	runUntilDone(t, c)
	if !waitTrue(func() bool { return c.Registered() && obs.has("latency") }, 3*time.Second) {
		t.Fatalf("not ready: %v", obs.snapshot())
	}
	start := time.Now()
	for i := 0; i < 5000; i++ {
		r.push(t, proto.MsgRequestSummary, summaryJSON("t-web", "2026-10-05T14:02:11Z", "GET", fmt.Sprintf("/n/%d", i), 200))
		c.events.mu.Lock()
		waiting := len(c.events.pending)
		c.events.mu.Unlock()
		if waiting > maxPendingEvents {
			t.Fatalf("%d calls wait for the observer, the queue holds %d", waiting, maxPendingEvents)
		}
	}
	// The relay could write all of them: the client read them without waiting
	// for its observer, which needs ten seconds for 5000 calls.
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the relay needed %v to hand 5000 summaries to the client", d)
	}
	if !waitTrue(func() bool { return obs.has("request:t-web:GET:/n/4999:200") }, 5*time.Second) {
		t.Fatal("the newest summary was not delivered")
	}
	// The observer takes 500 a second; most of the 5000 gave way.
	if n := obs.count("request:"); n > 3000 {
		t.Fatalf("%d of 5000 summaries were delivered", n)
	}
	if !obs.has("state:connected") || !obs.has("registered:web") {
		t.Fatalf("events: %v", obs.snapshot()[:5])
	}
}
