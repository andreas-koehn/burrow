package server

import (
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/proto"
)

// waitNoSessions waits until the relay holds no session.
func waitNoSessions(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(s.SnapshotSessions()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the session never left the registry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Closing by token ends the session the way a lost connection does: the
// tunnel is gone, the last record is written and the client's control stream
// ends.
func TestCloseByTokenEndsTheSessionAndItsTunnels(t *testing.T) {
	rec := &sessionRecorderFake{}
	c := connectRecorded(t, recordedAuth, rec) // logs in with token "tok-1"
	c.register(t, "web")
	if _, ok := c.srv.LookupHTTPTunnel("abc123"); !ok {
		t.Fatal("the tunnel is not up")
	}
	before := len(rec.snapshot())

	if n := c.srv.CloseByToken("tok-2"); n != 0 {
		t.Fatalf("closing another token closed %d sessions", n)
	}
	if n := c.srv.CloseByToken(""); n != 0 {
		t.Fatalf("closing the empty id closed %d sessions", n)
	}
	if got := len(c.srv.SnapshotSessions()); got != 1 {
		t.Fatalf("%d sessions after closing other tokens, want 1", got)
	}

	if n := c.srv.CloseByToken("tok-1"); n != 1 {
		t.Fatalf("CloseByToken = %d, want 1", n)
	}
	waitNoSessions(t, c.srv)
	if _, ok := c.srv.LookupHTTPTunnel("abc123"); ok {
		t.Error("the tunnel outlived its session")
	}
	calls := rec.waitCalls(t, before+1)
	if last := calls[len(calls)-1]; last.tokenID != "tok-1" || len(last.rec.Served) != 1 {
		t.Errorf("last record = %+v, want the one of the closed session", last)
	}
	// The client sees its control stream end.
	_ = c.ctrl.SetReadDeadline(time.Now().Add(3 * time.Second))
	var env proto.Envelope
	if err := proto.ReadFrame(c.ctrl, &env); err == nil {
		t.Errorf("the control stream still answers: %v", env.Type)
	}
	// Nothing is left to close.
	if n := c.srv.CloseByToken("tok-1"); n != 0 {
		t.Errorf("a second CloseByToken = %d, want 0", n)
	}
}

// A session whose authenticator named no token is never matched, whatever
// id is asked for.
func TestCloseByTokenLeavesSessionsWithoutATokenID(t *testing.T) {
	c := connectRecorded(t, fakeAuth{uid: "u1"}, nil)
	c.register(t, "web")
	if n := c.srv.CloseByToken(""); n != 0 {
		t.Fatalf("CloseByToken(\"\") = %d, want 0", n)
	}
	if got := len(c.srv.SnapshotSessions()); got != 1 {
		t.Fatalf("%d sessions, want the one without a token id", got)
	}
}

// A registry entry without a yamux session (tests build such) is skipped.
func TestCloseByTokenSkipsASessionWithoutYamux(t *testing.T) {
	r := NewRegistry()
	r.AddSession(&ClientSession{SessionID: "s1", TokenID: "tok-1"})
	if n := r.CloseByToken("tok-1"); n != 0 {
		t.Fatalf("CloseByToken = %d, want 0", n)
	}
}
