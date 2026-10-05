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
