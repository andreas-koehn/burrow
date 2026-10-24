package client

import (
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/bridge"
	"github.com/ankoehn/burrow/internal/proto"
)

// handleNewConnection opens a data stream, announces it, dials the local
// target for the tunnel, and bridges the two until either side closes. With
// proxyV2 the target is first sent a PROXY protocol v2 header that names the
// visitor (or the forwarder) and the address it connected to.
func (c *Client) handleNewConnection(sess *yamux.Session, nc proto.NewConnection, localAddr string, proxyV2 bool) {
	if c.events != nil {
		at := time.Now()
		c.events.emitLine(func(o Observer) { o.Connection(nc.TunnelID, at, nc.SourceIP) })
		c.countConnection(nc.TunnelID, 1)
		defer func() {
			c.events.emitLine(func(o Observer) { o.ConnectionClosed(nc.TunnelID) })
			c.countConnection(nc.TunnelID, -1)
		}()
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
	if proxyV2 {
		_ = local.SetWriteDeadline(time.Now().Add(proxyHeaderTimeout))
		if _, err := local.Write(proxyV2Header(nc.SourceIP, nc.DestAddr)); err != nil {
			c.log.Warn("proxy protocol header not written", "tunnel_id", nc.TunnelID, "local", localAddr, "err", err)
			return
		}
		_ = local.SetWriteDeadline(time.Time{})
	}
	var dummyIn, dummyOut atomicCounter
	bridge.Pipe(local, st, &dummyIn.v, &dummyOut.v)
}
