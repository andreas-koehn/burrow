package client

import (
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/bridge"
	"github.com/ankoehn/burrow/internal/proto"
)

// handleNewConnection opens a data stream, announces it, dials the local
// target for the tunnel, and bridges the two until either side closes.
func (c *Client) handleNewConnection(sess *yamux.Session, nc proto.NewConnection, localAddr string) {
	if c.events != nil {
		at := time.Now()
		c.events.emit(func(o Observer) { o.Connection(nc.TunnelID, at, nc.SourceIP) })
		defer c.events.emit(func(o Observer) { o.ConnectionClosed(nc.TunnelID) })
	}
	st, err := sess.OpenStream()
	if err != nil {
		return
	}
	defer st.Close()
	if err := proto.WriteMessage(st, proto.MsgStreamOpen, proto.StreamHeader{
		StreamID: nc.StreamID, TunnelID: nc.TunnelID,
	}); err != nil {
		return
	}
	local, err := net.Dial("tcp", localAddr)
	if err != nil {
		c.log.Warn("local dial failed", "tunnel_id", nc.TunnelID, "local", localAddr, "err", err)
		c.localTarget(localAddr, false)
		return
	}
	defer local.Close()
	c.localTarget(localAddr, true)
	var dummyIn, dummyOut atomicCounter
	bridge.Pipe(local, st, &dummyIn.v, &dummyOut.v)
}
