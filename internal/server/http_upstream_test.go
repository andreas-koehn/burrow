package server

import (
	"slices"
	"testing"

	"github.com/ankoehn/burrow/internal/proto"
)

// An http register may name the upstream protocol: it applies to that tunnel
// and is shown as the client's; one that names none leaves the service's
// setting. An unknown name is refused, a tcp register's is not looked at.
func TestHTTPUpstream_ClientNamesTheProtocol(t *testing.T) {
	r := newObservedRelay(t, 19591, nil)
	c := r.client(nil)
	if rr := c.register(t, proto.TunnelRegister{Name: "api", Type: "http", LocalAddr: "127.0.0.1:50051", UpstreamProtocol: proto.UpstreamH2C}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	if got := r.srv.ClientUpstreamProtocol("svc-1"); got != proto.UpstreamH2C {
		t.Fatalf("client protocol = %q", got)
	}
	tn, ok := r.srv.LookupHTTPTunnelByServiceID("svc-1")
	if !ok {
		t.Fatal("no tunnel")
	}
	if got := EffectiveUpstreamProtocol(tn, proto.UpstreamH2); got != proto.UpstreamH2C {
		t.Errorf("effective with a client value = %q, want the client's", got)
	}

	rr := c.register(t, proto.TunnelRegister{Name: "bad", Type: "http", LocalAddr: "x", UpstreamProtocol: "http2"})
	if rr.OK || rr.Code != proto.CodeBadRequest {
		t.Fatalf("unknown protocol: %+v", rr)
	}
	if got := r.srv.ClientUpstreamProtocol("nope"); got != "" {
		t.Errorf("no tunnel: %q", got)
	}
}

func TestHTTPUpstream_Effective(t *testing.T) {
	plain := &Tunnel{IsHTTP: true}
	for _, c := range []struct {
		tn           *Tunnel
		stored, want string
	}{
		{plain, "", proto.UpstreamHTTP1},
		{plain, proto.UpstreamH2, proto.UpstreamH2},
		{&Tunnel{IsHTTP: true, clientUpstream: proto.UpstreamHTTP1}, proto.UpstreamH2C, proto.UpstreamHTTP1},
		{&Tunnel{clientUpstream: proto.UpstreamH2C}, proto.UpstreamHTTPS, proto.UpstreamHTTPS}, // tcp: never the client's
		{nil, "", proto.UpstreamHTTP1},
	} {
		if got := EffectiveUpstreamProtocol(c.tn, c.stored); got != c.want {
			t.Errorf("%+v stored %q: got %q want %q", c.tn, c.stored, got, c.want)
		}
	}
}

// A relay with http services announces upstream_protocol.
func TestHTTPUpstream_Capability(t *testing.T) {
	r := newObservedRelay(t, 19592, nil)
	conn, ar := r.dial("secret", "")
	_ = conn.Close()
	if !ar.OK || !slices.Contains(ar.Capabilities, proto.CapUpstreamProtocol) {
		t.Fatalf("capabilities: %+v", ar)
	}
}
