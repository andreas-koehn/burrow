package main

import (
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/server"
)

// The proxy gets the service's upstream protocol and TLS settings; with no
// client value the stored setting applies, and none at all is http1.
func TestApplyUpstream(t *testing.T) {
	tn := &server.Tunnel{IsHTTP: true}
	var r proxy.Resolved
	applyUpstream(&r, db.Service{UpstreamProtocol: proto.UpstreamH2, UpstreamTLSVerify: true,
		UpstreamTLSServerName: "app.internal", UpstreamTLSCAPEM: "pem"}, tn)
	if r.UpstreamProtocol != proto.UpstreamH2 || r.UpstreamClientSet || !r.UpstreamTLSVerify ||
		r.UpstreamTLSServerName != "app.internal" || string(r.UpstreamTLSCAPEM) != "pem" {
		t.Fatalf("stored settings: %+v", r)
	}
	r = proxy.Resolved{}
	applyUpstream(&r, db.Service{}, tn)
	if r.UpstreamProtocol != proto.UpstreamHTTP1 || r.UpstreamTLSCAPEM != nil {
		t.Fatalf("defaults: %+v", r)
	}
}
