package proxy

import (
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
)

// A tunnel stream that is dialled after the request is done (a dial the
// transport started for a request that was given up) is closed at once:
// nothing else would close it.
func TestUpstreamTransport_TrackAfterDoneCloses(t *testing.T) {
	u := &upstreamTransport{rt: &http.Transport{}, tracked: true}
	before, peer := net.Pipe()
	defer peer.Close()
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	u.track(before)
	u.done()
	if _, err := before.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("a stream tracked before done is open: %v", err)
	}
	late, peer2 := net.Pipe()
	defer peer2.Close()
	u.track(late)
	if _, err := late.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("a stream tracked after done is open: %v", err)
	}
	if len(u.conns) != 0 {
		t.Fatalf("%d streams kept after done", len(u.conns))
	}
}
