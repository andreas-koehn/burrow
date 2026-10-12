package server

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/observe"
)

// recObserver records what a Registry and a Server tell their observer.
type recObserver struct {
	mu       sync.Mutex
	sessions []observe.Session
	traffic  []observe.Traffic
}

func (o *recObserver) ObserveSession(e observe.Session) {
	o.mu.Lock()
	o.sessions = append(o.sessions, e)
	o.mu.Unlock()
}

func (o *recObserver) ObserveTraffic(e observe.Traffic) {
	o.mu.Lock()
	o.traffic = append(o.traffic, e)
	o.mu.Unlock()
}

func (o *recObserver) kinds() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, 0, len(o.sessions))
	for _, e := range o.sessions {
		out = append(out, e.Kind+":"+e.TunnelID)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A session's life is told in order and each part once: opened, a tunnel
// opened and closed by unregistering, a second tunnel closed by the end of
// the session, then closed. Removing the session a second time (a teardown
// that runs twice) tells nothing more.
func TestRegistry_ObserverSeesEachEventOnce(t *testing.T) {
	r := NewRegistry()
	obs := &recObserver{}
	r.SetObserver(obs)
	cs := &ClientSession{SessionID: "s1", UserID: "u1", TokenID: "tok1", ConnectedAt: time.Now().Add(-2 * time.Second)}

	r.AddSession(cs)
	r.AddTunnel(cs, &Tunnel{ID: "t1", Name: "web", Type: "http", ServiceID: "svc1", IsHTTP: true, accessMode: "open", created: true})
	r.AddTunnel(cs, &Tunnel{ID: "t2", Name: "db", Type: ""})
	r.RemoveTunnel(cs, "t1")
	r.RemoveTunnel(cs, "t1")      // already gone
	r.RemoveTunnel(cs, "missing") // never there
	r.RemoveSession(cs)
	r.RemoveSession(cs) // a second teardown

	want := []string{
		observe.SessionOpened + ":",
		observe.TunnelOpened + ":t1",
		observe.TunnelOpened + ":t2",
		observe.TunnelClosed + ":t1",
		observe.TunnelClosed + ":t2",
		observe.SessionClosed + ":",
	}
	if got := obs.kinds(); !sameStrings(got, want) {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
	opened := obs.sessions[1]
	if opened.SessionID != "s1" || opened.UserID != "u1" || opened.TokenID != "tok1" ||
		opened.ServiceID != "svc1" || opened.TunnelType != "http" || opened.TunnelName != "web" ||
		opened.AccessMode != "open" || !opened.ServiceCreated {
		t.Fatalf("tunnel opened event: %+v", opened)
	}
	if tcp := obs.sessions[2]; tcp.TunnelType != "tcp" || tcp.ServiceID != "" || tcp.ServiceCreated {
		t.Fatalf("tcp tunnel opened event: %+v", tcp)
	}
	if closed := obs.sessions[5]; closed.Duration < 2*time.Second || closed.Duration > time.Minute {
		t.Fatalf("session closed after %s, want about 2s", closed.Duration)
	}
}

// Without an observer every call is a no-op, and a session built by hand
// (no ConnectedAt) reports no duration instead of the age of the zero time.
func TestRegistry_NoObserverAndZeroConnectedAt(t *testing.T) {
	r := NewRegistry()
	cs := &ClientSession{SessionID: "s"}
	r.AddSession(cs)
	r.AddTunnel(cs, &Tunnel{ID: "t"})
	r.RemoveSession(cs) // must not panic

	obs := &recObserver{}
	r.SetObserver(obs)
	r.AddSession(cs)
	r.RemoveSession(cs)
	if n := len(obs.sessions); n != 3 { // opened, tunnel closed (it is still in the session's map), closed
		t.Fatalf("%d events, want 3: %v", n, obs.kinds())
	}
	if d := obs.sessions[2].Duration; d != 0 {
		t.Fatalf("duration %s for a session without ConnectedAt", d)
	}
}

// Concurrent sessions: every one is opened and closed exactly once.
func TestRegistry_ObserverConcurrent(t *testing.T) {
	r := NewRegistry()
	obs := &recObserver{}
	r.SetObserver(obs)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			cs := &ClientSession{SessionID: id(n), ConnectedAt: time.Now()}
			r.AddSession(cs)
			r.AddTunnel(cs, &Tunnel{ID: id(n)})
			r.RemoveSession(cs)
		}(i)
	}
	wg.Wait()
	count := map[string]int{}
	for _, e := range obs.sessions {
		count[e.Kind+":"+e.SessionID]++
	}
	for i := 0; i < 50; i++ {
		for _, kind := range []string{observe.SessionOpened, observe.TunnelOpened, observe.TunnelClosed, observe.SessionClosed} {
			if n := count[kind+":"+id(i)]; n != 1 {
				t.Fatalf("session %s: %d %s events, want 1", id(i), n, kind)
			}
		}
	}
}

// What passes through a tunnel stream is added to the tunnel's counters:
// written to the client is BytesIn, read from it BytesOut.
func TestCountingStream_CountsBothDirections(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	var in, out atomic.Uint64
	cs := &countingStream{Conn: a, in: &in, out: &out}

	go func() {
		buf := make([]byte, 5)
		_, _ = io.ReadFull(b, buf)
		_, _ = b.Write([]byte("abc"))
	}()
	if n, err := cs.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("write: %d %v", n, err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(cs, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if in.Load() != 5 || out.Load() != 3 {
		t.Fatalf("in=%d out=%d, want 5 and 3", in.Load(), out.Load())
	}
}

// benchCopy pushes 64 MiB through a connection in 32 KiB chunks, the size
// io.Copy uses, and reads it on the other end.
func benchCopy(b *testing.B, wrap func(net.Conn) net.Conn) {
	const chunk, total = 32 * 1024, 64 << 20
	b.SetBytes(total)
	b.ReportAllocs()
	buf := make([]byte, chunk)
	for i := 0; i < b.N; i++ {
		near, far := net.Pipe()
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, far)
			close(done)
		}()
		w := wrap(near)
		for sent := 0; sent < total; sent += chunk {
			if _, err := w.Write(buf); err != nil {
				b.Fatal(err)
			}
		}
		_ = near.Close()
		<-done
		_ = far.Close()
	}
}

// BenchmarkTunnelStreamCopy is the copy path of an http tunnel: bare (as it
// was before the counters), and with the byte counters on it. The two must
// not differ by more than noise, and the counting one must not allocate per
// chunk.
func BenchmarkTunnelStreamCopy(b *testing.B) {
	b.Run("bare", func(b *testing.B) {
		benchCopy(b, func(c net.Conn) net.Conn { return c })
	})
	b.Run("counted", func(b *testing.B) {
		var in, out atomic.Uint64
		benchCopy(b, func(c net.Conn) net.Conn { return &countingStream{Conn: c, in: &in, out: &out} })
	})
}

// The counters cost no allocation per chunk.
func TestCountingStream_NoAllocPerChunk(t *testing.T) {
	near, far := net.Pipe()
	defer near.Close()
	defer far.Close()
	go func() { _, _ = io.Copy(io.Discard, far) }()
	var in, out atomic.Uint64
	bare := testing.AllocsPerRun(200, func() { _, _ = near.Write([]byte("0123456789abcdef")) })
	cs := &countingStream{Conn: near, in: &in, out: &out}
	counted := testing.AllocsPerRun(200, func() { _, _ = cs.Write([]byte("0123456789abcdef")) })
	if counted > bare {
		t.Fatalf("counting adds allocations: %.1f per write against %.1f bare", counted, bare)
	}
}
