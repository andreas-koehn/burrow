package server

import (
	"context"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/proto"
)

// OpenTunnelStream opens a new yamux stream to the client that owns tn by
// replicating the same pairing semantics as the internal bridgeVisitor helper.
// It is exported so the proxy dialer adapter (cmd/server/proxy_wiring.go) can
// open per-request streams without duplicating the pendingStreams logic.
//
// Sequence (mirrors bridgeVisitor exactly, minus the bridge.Pipe call):
//  1. Generate a unique streamID.
//  2. Register a waiter via tun.sess.Pending().Await.
//  3. Inside the Await callback (runs after waiter is registered, before the
//     select), send a MsgNewConnection control message to the client.
//  4. Block until the client opens a yamux stream carrying that streamID in its
//     StreamHeader, or until the 10 s timeout.
//
// The returned net.Conn is the raw *yamux.Stream; the caller is responsible for
// closing it after the proxied request completes.
//
// Returns proxy.ErrNotFound-equivalent when tn is nil; returns a
// timeout/control error when the client does not respond in time.
// The ctx parameter is accepted for future use (cancellation); it is not yet
// wired into Await (which uses a fixed 10 s wall clock timeout).
func (s *Server) OpenTunnelStream(_ context.Context, tn *Tunnel) (net.Conn, error) {
	streamID := uuid.NewString()
	stream, err := tn.sess.Pending().Await(streamID, 10*time.Second, func() error {
		return tn.sess.SendControl(proto.MsgNewConnection, proto.NewConnection{
			TunnelID: tn.ID,
			StreamID: streamID,
			SourceIP: "", // proxy fills headers; SourceIP is informational
		})
	})
	if err != nil {
		return nil, err
	}
	// Everything that goes through an http tunnel goes through this stream:
	// the proxy's requests and the AI gateway's. Counting here is what makes
	// the tunnel's bytes_in/bytes_out real for http tunnels.
	return &countingStream{Conn: stream, in: &tn.BytesIn, out: &tn.BytesOut}, nil
}
