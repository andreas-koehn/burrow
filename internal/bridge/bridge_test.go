package bridge

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/testutil"
)

// tcpPipe creates a pair of connected TCP connections that support CloseWrite.
// net.Pipe() returns *net.pipe which does not implement CloseWrite on Go 1.26.
func tcpPipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-ch
	return client, server
}

func TestPipeCopiesBothWaysAndCounts(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	a1, a2 := tcpPipe(t) // "visitor" side: write to a1, Pipe reads a2
	b1, b2 := tcpPipe(t) // "target" side: Pipe writes b1, read b2
	var in, out atomic.Uint64
	done := make(chan struct{})
	go func() { Pipe(a2, b1, &in, &out); close(done) }()

	_, _ = a1.Write([]byte("ping"))
	got := make([]byte, 4)
	_ = b2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(b2, got); err != nil || string(got) != "ping" {
		t.Fatalf("target got %q err=%v", got, err)
	}
	_, _ = b2.Write([]byte("pong!"))
	r := make([]byte, 5)
	_ = a1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(a1, r); err != nil || string(r) != "pong!" {
		t.Fatalf("visitor got %q err=%v", r, err)
	}
	_ = a1.Close()
	_ = b2.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pipe did not return after both sides closed")
	}
	if in.Load() != 4 || out.Load() != 5 {
		t.Fatalf("counters in=%d out=%d (want 4/5)", in.Load(), out.Load())
	}
}

func TestPipeClosesBothWhenOneEnds(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	a2, aPeer := tcpPipe(t)
	b1, bPeer := tcpPipe(t)
	var in, out atomic.Uint64
	done := make(chan struct{})
	go func() { Pipe(a2, b1, &in, &out); close(done) }()
	_ = aPeer.Close() // one side ends → Pipe must close the other and return
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pipe did not return when one side closed")
	}
	if _, err := bPeer.Read(make([]byte, 1)); err == nil {
		t.Fatal("other side was not closed")
	}
}

// PipeSides says which conn ended the pipe: the one that closed first.
func TestPipeSidesNamesTheSideThatEnded(t *testing.T) {
	defer testutil.AssertNoGoroutineLeak(t)()
	for _, c := range []struct {
		name string
		want Side
	}{{"a closes", SideA}, {"b closes", SideB}} {
		t.Run(c.name, func(t *testing.T) {
			a1, a2 := tcpPipe(t)
			b1, b2 := tcpPipe(t)
			defer a1.Close()
			defer b2.Close()
			var in, out atomic.Uint64
			got := make(chan Side, 1)
			go func() { got <- PipeSides(a2, b1, &in, &out) }()
			if c.want == SideA {
				_ = a1.Close()
			} else {
				_ = b2.Close()
			}
			select {
			case s := <-got:
				if s != c.want {
					t.Fatalf("side %d, want %d", s, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("PipeSides did not return")
			}
		})
	}
}
