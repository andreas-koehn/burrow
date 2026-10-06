package view

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
)

var _ client.Observer = (*Store)(nil)

func specs() []client.TunnelSpec {
	return []client.TunnelSpec{
		{Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000"},
		{Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000},
	}
}

func register(s *Store, suffix string) {
	s.Registered(client.RegisteredTunnel{TunnelID: "a" + suffix, Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000", URL: "https://burrow.example.com/svc/p7baeh/"})
	s.Registered(client.RegisteredTunnel{TunnelID: "b" + suffix, Name: "pg", Type: "tcp", LocalAddr: "127.0.0.1:5432", RemotePort: 9000})
}

func TestStore_StartsWithTheServicesOfTheCommand(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	m := s.Snapshot()
	if m.Relay != "burrow.example.com" || m.Version != "v0.6.0" || m.State != client.StateConnecting || len(m.Services) != 2 {
		t.Fatalf("model = %+v", m)
	}
	if a := m.Services[0]; a.Name != "my-app" || a.Type != "http" || a.Local != "127.0.0.1:3000" || a.Public != "" {
		t.Fatalf("service = %+v", a)
	}
}

func TestStore_RegisteredFillsThePublicAddress(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.State(client.StateConnected, "", 0)
	register(s, "1")
	m := s.Snapshot()
	if m.State != client.StateConnected || len(m.Services) != 2 {
		t.Fatalf("model = %+v", m)
	}
	if got := m.Services[0].Public; got != "https://burrow.example.com/svc/p7baeh/" {
		t.Fatalf("http public = %q", got)
	}
	if got := m.Services[1].Public; got != "burrow.example.com:9000" {
		t.Fatalf("tcp public = %q", got)
	}
	// A tunnel the command did not name still gets its block.
	s.Registered(client.RegisteredTunnel{TunnelID: "c", Name: "extra", Type: "tcp", LocalAddr: "127.0.0.1:1", RemotePort: 9001})
	if m := s.Snapshot(); len(m.Services) != 3 || m.Services[2].Name != "extra" || m.Services[2].Public != "burrow.example.com:9001" {
		t.Fatalf("services = %+v", m.Services)
	}
}

func TestStore_CountsConnections(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.State(client.StateConnected, "", 0)
	register(s, "1")
	base := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		s.Connection("a1", base.Add(time.Duration(i)*time.Second), fmt.Sprintf("203.0.113.%d", i))
	}
	s.Connection("unknown", base, "198.51.100.1") // not ours: ignored
	// The relay sends the address with the port of the visitor.
	s.Connection("b1", base, "203.0.113.7:51234")
	s.Connection("b1", base, "[2001:db8::1]:51234")
	if r := s.Snapshot().Services[1].Recent; r[0].SourceIP != "203.0.113.7" || r[1].SourceIP != "2001:db8::1" {
		t.Fatalf("source addresses = %+v", r)
	}
	s.ConnectionClosed("b1")
	s.ConnectionClosed("b1")
	m := s.Snapshot()
	a := m.Services[0]
	if a.Open != 12 || a.Total != 12 || len(a.Recent) != 10 {
		t.Fatalf("open %d total %d recent %d", a.Open, a.Total, len(a.Recent))
	}
	if a.Recent[0].SourceIP != "203.0.113.2" || a.Recent[9].SourceIP != "203.0.113.11" || !a.Recent[9].At.Equal(base.Add(11*time.Second)) {
		t.Fatalf("recent = %+v", a.Recent)
	}
	if b := m.Services[1]; b.Open != 0 || b.Total != 2 || len(b.Recent) != 2 {
		t.Fatalf("the other service changed: %+v", b)
	}
	for i := 0; i < 20; i++ {
		s.ConnectionClosed("a1")
	}
	s.ConnectionClosed("unknown")
	if a := s.Snapshot().Services[0]; a.Open != 0 || a.Total != 12 {
		t.Fatalf("after closing more than were open: open %d total %d", a.Open, a.Total)
	}
}

func TestStore_Reconnect(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.now = func() time.Time { return now }
	s.State(client.StateConnected, "", 0)
	register(s, "1")
	s.Latency(12 * time.Millisecond)
	s.Connection("a1", now, "203.0.113.7")
	if m := s.Snapshot(); m.RTT != 12*time.Millisecond || m.Services[0].Open != 1 {
		t.Fatalf("model = %+v", m)
	}

	s.State(client.StateReconnecting, "connection refused", 4*time.Second)
	m := s.Snapshot()
	if m.State != client.StateReconnecting || m.Detail != "connection refused" || m.RetryIn != 4*time.Second || m.RTT != 0 {
		t.Fatalf("model = %+v", m)
	}
	// The connections went down with the session.
	if a := m.Services[0]; a.Open != 0 || a.Total != 1 || len(a.Recent) != 1 {
		t.Fatalf("service = %+v", a)
	}
	// The delay counts down and does not go below zero.
	now = now.Add(1500 * time.Millisecond)
	if got := s.Snapshot().RetryIn; got != 2500*time.Millisecond {
		t.Fatalf("retry in %v", got)
	}
	now = now.Add(time.Minute)
	if got := s.Snapshot().RetryIn; got != 0 {
		t.Fatalf("retry in %v", got)
	}

	// The handler of the old connection ends after the session did.
	s.ConnectionClosed("a1")
	s.State(client.StateConnecting, "", 0)
	s.State(client.StateConnected, "", 0)
	register(s, "2")
	s.ConnectionClosed("a1")
	s.Connection("a2", now, "203.0.113.8")
	m = s.Snapshot()
	if len(m.Services) != 2 || m.Detail != "" || m.RetryIn != 0 {
		t.Fatalf("model = %+v", m)
	}
	if a := m.Services[0]; a.Open != 1 || a.Total != 2 || len(a.Recent) != 2 {
		t.Fatalf("service = %+v", a)
	}
}

func TestStore_TwoServicesWithTheSameNameAndTarget(t *testing.T) {
	same := client.TunnelSpec{Name: "x", Type: "tcp", LocalAddr: "127.0.0.1:1"}
	s := NewStore("r", "v", []client.TunnelSpec{same, same})
	s.Registered(client.RegisteredTunnel{TunnelID: "1", Name: "x", Type: "tcp", LocalAddr: "127.0.0.1:1", RemotePort: 9000})
	s.Registered(client.RegisteredTunnel{TunnelID: "2", Name: "x", Type: "tcp", LocalAddr: "127.0.0.1:1", RemotePort: 9001})
	m := s.Snapshot()
	if len(m.Services) != 2 || m.Services[0].Public != "r:9000" || m.Services[1].Public != "r:9001" {
		t.Fatalf("services = %+v", m.Services)
	}
}

func TestStore_LocalTarget(t *testing.T) {
	sp := append(specs(), client.TunnelSpec{Name: "again", Type: "tcp", LocalAddr: "127.0.0.1:3000"})
	s := NewStore("r", "v", sp)
	s.LocalTarget("127.0.0.1:3000", false)
	m := s.Snapshot()
	if !m.Services[0].LocalDown || m.Services[1].LocalDown || !m.Services[2].LocalDown {
		t.Fatalf("services = %+v", m.Services)
	}
	s.LocalTarget("127.0.0.1:3000", true)
	if m := s.Snapshot(); m.Services[0].LocalDown || m.Services[2].LocalDown {
		t.Fatalf("services = %+v", m.Services)
	}
}

func TestStore_SnapshotIsACopy(t *testing.T) {
	s := NewStore("r", "v", specs())
	register(s, "1")
	s.Connection("a1", time.Now(), "203.0.113.7")
	m := s.Snapshot()
	m.Services[0].Name = "changed"
	m.Services[0].Recent[0].SourceIP = "changed"
	if again := s.Snapshot(); again.Services[0].Name != "my-app" || again.Services[0].Recent[0].SourceIP != "203.0.113.7" {
		t.Fatalf("the store was changed through a snapshot: %+v", again.Services[0])
	}
}

func TestStore_ChangedNeverBlocks(t *testing.T) {
	s := NewStore("r", "v", specs())
	register(s, "1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ { // nobody reads Changed()
			s.Connection("a1", time.Now(), "203.0.113.7")
			s.ConnectionClosed("a1")
			s.Latency(time.Millisecond)
			s.LocalTarget("127.0.0.1:3000", i%2 == 0)
			s.State(client.StateConnected, "", 0)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the store blocked without a reader")
	}
	select {
	case <-s.Changed():
	default:
		t.Fatal("no signal after changes")
	}
	select {
	case <-s.Changed():
		t.Fatal("more than one signal was queued")
	default:
	}
}

func TestStore_ConcurrentUse(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.State(client.StateConnected, "", 0)
	register(s, "1")
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Connection("a1", time.Now(), "203.0.113.7")
				_ = Render(s.Snapshot(), 80, g%2 == 0)
				s.ConnectionClosed("a1")
				s.Latency(time.Duration(i) * time.Millisecond)
				s.LocalTarget("127.0.0.1:5432", i%2 == 0)
				select {
				case <-s.Changed():
				default:
				}
			}
		}(g)
	}
	wg.Wait()
	a := s.Snapshot().Services[0]
	if a.Total != 4000 || a.Open != 0 || len(a.Recent) != 10 {
		t.Fatalf("total %d open %d recent %d", a.Total, a.Open, len(a.Recent))
	}
}

func TestStore_RegisteredFillsTheAccessMode(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.State(client.StateConnected, "", 0)
	s.Registered(client.RegisteredTunnel{TunnelID: "a1", Name: "my-app", Type: "http", LocalAddr: "127.0.0.1:3000",
		URL: "https://burrow.example.com/svc/p7baeh/", AccessMode: "burrow_login"})
	if got := s.Snapshot().Services[0].Access; got != "burrow_login" {
		t.Fatalf("access = %q", got)
	}
	// After a reconnect to a relay that does not say (an older one took over),
	// the view does not go on showing what it no longer knows.
	s.State(client.StateReconnecting, "session closed", time.Second)
	s.State(client.StateConnected, "", 0)
	register(s, "2")
	if got := s.Snapshot().Services[0].Access; got != "" {
		t.Fatalf("access after an answer without it = %q", got)
	}
}

func TestStore_NotesAreKeptOnceAndBounded(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	<-drain(s)
	s.Note("first")
	select {
	case <-s.Changed():
	default:
		t.Fatal("a note did not signal a change")
	}
	s.Note("first")
	s.Note("")
	s.Note("second")
	if got := s.Snapshot().Notes; len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("notes = %q", got)
	}
	// The snapshot is a copy.
	snap := s.Snapshot()
	snap.Notes[0] = "changed"
	if s.Snapshot().Notes[0] != "first" {
		t.Fatal("the snapshot shares the notes with the store")
	}
	for i := 0; i < 100; i++ {
		s.Note(fmt.Sprintf("note %d", i))
	}
	if n := len(s.Snapshot().Notes); n != maxNotes {
		t.Fatalf("%d notes are kept, want %d", n, maxNotes)
	}
	// A reconnect keeps them: they are about the run, not the session.
	s.State(client.StateReconnecting, "x", time.Second)
	if n := len(s.Snapshot().Notes); n != maxNotes {
		t.Fatalf("%d notes after a reconnect", n)
	}

	s.SetNotice("The relay runs v0.7.0. Run: burrow update")
	if got := s.Snapshot().Notice; got != "The relay runs v0.7.0. Run: burrow update" {
		t.Fatalf("notice = %q", got)
	}
}

// drain empties the change signal and returns a closed channel, so that a
// test can start from "no change pending".
func drain(s *Store) <-chan struct{} {
	select {
	case <-s.Changed():
	default:
	}
	c := make(chan struct{})
	close(c)
	return c
}

var _ client.CountObserver = (*Store)(nil)

func TestStore_RequestsReplaceTheConnectionLines(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.State(client.StateConnected, "", 0)
	register(s, "1")
	base := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)

	// The connection of a request is told before its summary can be.
	s.Connection("a1", base, "203.0.113.7:51234")
	if r := s.Snapshot().Services[0].Recent; len(r) != 1 || r[0].SourceIP != "203.0.113.7" {
		t.Fatalf("before the first summary: %+v", r)
	}
	s.Request("a1", base.Add(time.Second), "GET", "/api/users", 200)
	s.Connection("a1", base.Add(2*time.Second), "203.0.113.7:51235")
	s.Request("a1", base.Add(2*time.Second), "POST", "/api/login", 401)
	a := s.Snapshot().Services[0]
	want := []Line{
		{At: base.Add(time.Second), Method: "GET", Path: "/api/users", Status: 200},
		{At: base.Add(2 * time.Second), Method: "POST", Path: "/api/login", Status: 401},
	}
	if len(a.Recent) != 2 || a.Recent[0] != want[0] || a.Recent[1] != want[1] {
		t.Fatalf("recent = %+v\nwant %+v", a.Recent, want)
	}
	// The connections are still counted.
	if a.Open != 2 || a.Total != 2 {
		t.Fatalf("open %d total %d", a.Open, a.Total)
	}

	// Ten are kept, the newest.
	for i := 0; i < 25; i++ {
		s.Request("a1", base.Add(time.Duration(10+i)*time.Second), "GET", fmt.Sprintf("/n/%d", i), 200)
	}
	if r := s.Snapshot().Services[0].Recent; len(r) != 10 || r[0].Path != "/n/15" || r[9].Path != "/n/24" {
		t.Fatalf("recent = %+v", r)
	}

	// A summary for a tunnel that is not ours, or for a tcp service, is not shown.
	s.Request("unknown", base, "GET", "/x", 200)
	s.Request("b1", base, "GET", "/x", 200)
	s.Connection("b1", base, "203.0.113.9:1")
	m := s.Snapshot()
	if r := m.Services[0].Recent; r[9].Path != "/n/24" {
		t.Fatalf("recent = %+v", r)
	}
	if r := m.Services[1].Recent; len(r) != 1 || r[0].Method != "" || r[0].SourceIP != "203.0.113.9" {
		t.Fatalf("the tcp service shows %+v", r)
	}
}

// What the relay sent is cut and made printable where it is kept, too.
func TestStore_RequestTextIsBounded(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.State(client.StateConnected, "", 0)
	register(s, "1")
	long := "/"
	for len(long) < 5000 {
		long += "abcdefghij"
	}
	s.Request("a1", time.Now(), "GE\x1bT-AND-MUCH-TOO-LONG-A-METHOD", long+"\x1b[2J", 200)
	l := s.Snapshot().Services[0].Recent[0]
	if l.Method != "GE?T-AND-MUCH-TO" || len(l.Path) != 256 || l.Path != long[:256] {
		t.Fatalf("kept %q and %d bytes of path", l.Method, len(l.Path))
	}
}

// After a reconnect the relay may be another one, without summaries: the
// connection lines come back until a summary arrives.
func TestStore_RequestsAfterAReconnect(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.State(client.StateConnected, "", 0)
	register(s, "1")
	now := time.Now()
	s.Request("a1", now, "GET", "/one", 200)
	s.State(client.StateReconnecting, "gone", time.Second)
	s.State(client.StateConnected, "", 0)
	register(s, "2")
	s.Request("a1", now, "GET", "/stale", 200) // a tunnel of the session before
	s.Connection("a2", now, "203.0.113.7:1")
	r := s.Snapshot().Services[0].Recent
	if len(r) != 2 || r[0].Path != "/one" || r[1].SourceIP != "203.0.113.7" {
		t.Fatalf("recent = %+v", r)
	}
	s.Request("a2", now, "GET", "/two", 200)
	r = s.Snapshot().Services[0].Recent
	if len(r) != 2 || r[0].Path != "/one" || r[1].Path != "/two" {
		t.Fatalf("recent = %+v", r)
	}
}

// The client tells the counts of a tunnel apart from the single connections,
// whose calls may be lost in a burst. Once it has, the counts are what it
// says: a lost ConnectionClosed leaves nothing open.
func TestStore_Counts(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", specs())
	s.State(client.StateConnected, "", 0)
	register(s, "1")
	now := time.Now()
	for i := 0; i < 3; i++ {
		s.Connection("a1", now, "203.0.113.7:1")
	}
	// Two of the three ended; the calls for that were dropped.
	s.Counts("a1", 1, 3)
	if a := s.Snapshot().Services[0]; a.Open != 1 || a.Total != 3 {
		t.Fatalf("open %d total %d", a.Open, a.Total)
	}
	// From here on the single calls add lines and do not count.
	s.Connection("a1", now, "203.0.113.7:1")
	s.ConnectionClosed("a1")
	s.ConnectionClosed("a1")
	if a := s.Snapshot().Services[0]; a.Open != 1 || a.Total != 3 || len(a.Recent) != 4 {
		t.Fatalf("after single calls: open %d total %d recent %d", a.Open, a.Total, len(a.Recent))
	}
	s.Counts("a1", 0, 4)
	s.Counts("unknown", 9, 9)
	s.Counts("b1", -1, -5) // nonsense is not shown
	m := s.Snapshot()
	if a := m.Services[0]; a.Open != 0 || a.Total != 4 {
		t.Fatalf("open %d total %d", a.Open, a.Total)
	}
	if b := m.Services[1]; b.Open != 0 || b.Total != 0 {
		t.Fatalf("the other service: %+v", b)
	}

	// A new session counts from nothing; the total of the service goes on.
	s.State(client.StateReconnecting, "gone", time.Second)
	if a := s.Snapshot().Services[0]; a.Open != 0 || a.Total != 4 {
		t.Fatalf("after the session ended: open %d total %d", a.Open, a.Total)
	}
	s.State(client.StateConnected, "", 0)
	register(s, "2")
	s.Counts("a1", 5, 9) // the session before
	s.Connection("a2", now, "203.0.113.7:1")
	if a := s.Snapshot().Services[0]; a.Open != 1 || a.Total != 5 {
		t.Fatalf("first connection of the new session: open %d total %d", a.Open, a.Total)
	}
	s.Counts("a2", 2, 2)
	if a := s.Snapshot().Services[0]; a.Open != 2 || a.Total != 6 {
		t.Fatalf("new session: open %d total %d", a.Open, a.Total)
	}
}
