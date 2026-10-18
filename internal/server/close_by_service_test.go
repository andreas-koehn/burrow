package server

import "testing"

// Closing by service ends the session that serves it, as CloseByToken does,
// and no other.
func TestCloseByServiceEndsTheSessionThatServesIt(t *testing.T) {
	c := connectRecorded(t, recordedAuth, nil)
	c.register(t, "web") // the test resolver names its service "svc-1"
	if n := c.srv.CloseByService("svc-other"); n != 0 {
		t.Fatalf("closing another service closed %d sessions", n)
	}
	if n := c.srv.CloseByService(""); n != 0 {
		t.Fatalf("closing the empty id closed %d sessions", n)
	}
	if got := len(c.srv.SnapshotSessions()); got != 1 {
		t.Fatalf("%d sessions, want 1", got)
	}
	if n := c.srv.CloseByService("svc-1"); n != 1 {
		t.Fatalf("CloseByService = %d, want 1", n)
	}
	waitNoSessions(t, c.srv)
	if _, ok := c.srv.LookupHTTPTunnel("abc123"); ok {
		t.Error("the tunnel outlived its session")
	}
}
