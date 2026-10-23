package view

import (
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/client"
)

var _ client.ForwardObserver = (*Store)(nil)

func TestStore_Forwards(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", nil)
	s.AddForwards([]client.ForwardSpec{{Target: "rdp", Addr: "127.0.0.1:13389"}})
	if m := s.Snapshot(); len(m.Forwards) != 1 || m.Forwards[0].Target != "rdp" || m.Forwards[0].Addr != "127.0.0.1:13389" {
		t.Fatalf("forwards %+v", m.Forwards)
	}
	s.Forward(client.ForwardState{Index: 0, Target: "rdp", Addr: "127.0.0.1:13389", Connected: true, Open: 1, Total: 4})
	f := s.Snapshot().Forwards[0]
	if f.Open != 1 || f.Total != 4 || f.Problem != "" || f.Stopped {
		t.Fatalf("forward %+v", f)
	}
	// A state for a forward the store does not know is dropped.
	s.Forward(client.ForwardState{Index: 3, Target: "x"})
	if n := len(s.Snapshot().Forwards); n != 1 {
		t.Fatalf("%d forwards", n)
	}
}

func TestStore_APrivateTCPServiceShowsTheForwardHint(t *testing.T) {
	s := NewStore("burrow.example.com", "v0.6.0", []client.TunnelSpec{{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"}})
	s.State(client.StateConnected, "", 0)
	s.Registered(client.RegisteredTunnel{TunnelID: "t", Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", AccessMode: "private", Slug: "t0123456789"})
	if got := s.Snapshot().Services[0].Public; got != "private · burrow forward t0123456789" {
		t.Fatalf("public = %q", got)
	}
}

func TestRender_Forwards(t *testing.T) {
	m := connected()
	m.Forwards = []Forward{
		{Target: "rdp", Addr: "127.0.0.1:13389", Open: 1, Total: 3},
		{Target: "db\x1b[2J", Addr: "127.0.0.1:15432", Problem: "db is offline: no client serves it right now"},
		{Target: "nope", Addr: "127.0.0.1:2222", Problem: "tcp service \"nope\" not found", Stopped: true},
	}
	out := strings.Join(Render(m, 80, false), "\n")
	for _, want := range []string{
		"  forward rdp   127.0.0.1:13389",
		"1 open, 3 total",
		"! db is offline",
		"stopped: tcp service \"nope\" not found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatal("control characters in the view")
	}
	for _, w := range []int{20, 40} {
		for _, l := range Render(m, w, false) {
			if length(l) > w-1 {
				t.Fatalf("width %d: %q", w, l)
			}
		}
	}
}
