package main

// Native tcp, P02: the access modes of tcp services through the whole
// relay. The resolver creates a service in the mode a client asks for, the
// loader reads the ip policy, and the API's changes reach the live tunnel.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/proto"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

var (
	_ server.TCPAccessLoader  = tcpAccessAdapter{}
	_ api.TCPServiceRefresher = (*server.Server)(nil)
	_ api.IPGrantStore        = (*db.DB)(nil)
)

// The relay and the store agree on the most visitors a service may set.
func TestTCPMaxConnections_OneBound(t *testing.T) {
	if store.MaxTCPMaxConnections != server.MaxTCPMaxConnections {
		t.Fatalf("store %d, relay %d", store.MaxTCPMaxConnections, server.MaxTCPMaxConnections)
	}
}

// A client's access wish creates the service in that mode; an existing
// service keeps its mode; a mode that is no tcp mode is refused.
func TestResolveTCP_AccessModes(t *testing.T) {
	x, uid := optionsDB(t)
	a := serviceResolverAdapter{db: x}
	ctx := context.Background()
	r, err := a.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{Access: "private"})
	if err != nil || !r.Created || r.AccessMode != "private" {
		t.Fatalf("private: %+v %v", r, err)
	}
	if row := servicesOf(t, x, uid)["rdp"]; row.AccessMode != "private" {
		t.Fatalf("row %+v", row)
	}
	if err := x.SetServiceMaxConnections(ctx, r.ServiceID, 7); err != nil {
		t.Fatal(err)
	}
	again, err := a.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{Access: "open"})
	if err != nil || again.AccessMode != "private" || again.MaxConnections != 7 {
		t.Fatalf("existing: %+v %v", again, err)
	}
	if ip, err := a.ResolveTCP(ctx, uid, "jump", server.ResolveOptions{Access: "ip"}); err != nil || ip.AccessMode != "ip" {
		t.Fatalf("ip: %+v %v", ip, err)
	}
	_, err = a.ResolveTCP(ctx, uid, "web2", server.ResolveOptions{Access: "api_key"})
	var re *server.RefusalError
	if !errors.As(err, &re) || re.Code != proto.CodeAccessInvalid {
		t.Fatalf("api_key: %v", err)
	}
	if _, ok := servicesOf(t, x, uid)["web2"]; ok {
		t.Fatal("a refused registration created a service")
	}
}

// The loader reads the mode and the cap, and for mode ip the CIDR lists and
// the grants that have not expired.
func TestTCPAccessAdapter(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	res := serviceResolverAdapter{db: x}
	r, err := res.ResolveTCP(ctx, uid, "rdp", server.ResolveOptions{Access: "ip"})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.SetServiceIPGeo(ctx, db.ServiceIPGeoConfig{ServiceID: r.ServiceID, AllowCIDRs: []string{"192.0.2.0/24"},
		BlockCIDRs: []string{"192.0.2.66"}, AllowCountries: []string{"DE"}, BlockCountries: []string{}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for i, exp := range []time.Time{now.Add(time.Hour), now.Add(-time.Minute)} {
		if err := x.CreateIPGrant(ctx, db.IPGrant{ID: fmt.Sprintf("g%d", i), ServiceID: r.ServiceID, CIDR: fmt.Sprintf("203.0.113.%d/32", i), ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}
	a := tcpAccessAdapter{db: x, now: func() time.Time { return now }}
	acc, err := a.TCPAccess(ctx, r.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Mode != "ip" || len(acc.AllowCIDRs) != 1 || acc.BlockCIDRs[0] != "192.0.2.66" || len(acc.Grants) != 1 ||
		acc.Grants[0].CIDR != "203.0.113.0/32" || !acc.Grants[0].ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("access %+v", acc)
	}
	web, err := x.GetOrCreateService(ctx, uid, "web", "http")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.TCPAccess(ctx, web.ID); err == nil {
		t.Fatal("an http service has a tcp access")
	}
	if _, err := a.TCPAccess(ctx, "missing"); err == nil {
		t.Fatal("a missing service has a tcp access")
	}
}

// Through the whole relay: a tcp service is switched to mode ip in the
// dashboard, its client registers again, and a visitor is refused until a
// just-in-time grant for its address is made; revoking it refuses again;
// mode private leaves no port at all. Every refusal is in the connection
// log.
func TestServe_TCPAccessModes(t *testing.T) {
	r := bootObsRelay(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	var token struct {
		Token string `json:"token"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "tcp-access"}, http.StatusCreated, &token)
	stop := r.connectWith(token.Token, client.TunnelSpec{Name: "rdp", Type: "tcp", LocalAddr: echo.Addr().String()})
	defer stop()

	type svc struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		AccessMode string `json:"access_mode"`
		Connected  bool   `json:"connected"`
		RemotePort int    `json:"remote_port"`
	}
	find := func() svc {
		var rows []svc
		r.mustAdmin(http.MethodGet, "/api/v1/services", nil, http.StatusOK, &rows)
		for _, s := range rows {
			if s.Name == "rdp" {
				return s
			}
		}
		return svc{}
	}
	var s svc
	r.eventually("the service is served", func() bool { s = find(); return s.Connected })
	firstTunnel := r.tunnels()["rdp"].ID
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.tcpPort))
	echoes := func() bool {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return false
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte("ping\n")); err != nil {
			return false
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		return err == nil && line == "ping\n"
	}
	if !echoes() {
		t.Fatal("open: no echo")
	}

	// Mode ip: the session is closed, and the client comes back in it.
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+s.ID+"/access-mode", map[string]string{"access_mode": "ip"}, http.StatusNoContent, nil)
	r.eventually("the client registers again in mode ip", func() bool {
		tn, ok := r.tunnels()["rdp"]
		return ok && tn.ID != firstTunnel
	})
	if s = find(); s.AccessMode != "ip" || !s.Connected || s.RemotePort != r.tcpPort {
		t.Fatalf("mode ip: %+v", s)
	}
	if echoes() {
		t.Fatal("mode ip without a grant let a visitor through")
	}
	type logRow struct {
		Status   string `json:"status"`
		Reason   string `json:"reason"`
		SourceIP string `json:"source_ip"`
	}
	r.eventually("a rejected row", func() bool {
		var rows []logRow
		r.mustAdmin(http.MethodGet, "/api/v1/connection-logs?kind=tcp_proxy&service_id="+s.ID, nil, http.StatusOK, &rows)
		for _, row := range rows {
			if row.Status == "rejected" && row.Reason == "ip_geo" && row.SourceIP == "127.0.0.1" {
				return true
			}
		}
		return false
	})

	// "Allow my IP": the dashboard sees this test at 127.0.0.1, as the
	// relay sees its visitors.
	var grant struct {
		ID      string `json:"id"`
		CIDR    string `json:"cidr"`
		Family  string `json:"family"`
		Warning string `json:"warning"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/services/"+s.ID+"/ip-grants", map[string]any{"ttl_hours": 1}, http.StatusCreated, &grant)
	if grant.CIDR != "127.0.0.1/32" || grant.Family != "ipv4" || grant.Warning == "" {
		t.Fatalf("grant %+v", grant)
	}
	if !echoes() {
		t.Fatal("a granted visitor was refused")
	}
	r.mustAdmin(http.MethodDelete, "/api/v1/services/"+s.ID+"/ip-grants/"+grant.ID, nil, http.StatusNoContent, nil)
	if echoes() {
		t.Fatal("a revoked grant still lets the visitor in")
	}
	// The permanent list does the same.
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+s.ID+"/ip-geo", map[string]any{"allow_cidrs": []string{"127.0.0.0/8"}}, http.StatusNoContent, nil)
	if !echoes() {
		t.Fatal("the permanent allow list was not applied")
	}

	// Mode private: no port.
	second := r.tunnels()["rdp"].ID
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+s.ID+"/access-mode", map[string]string{"access_mode": "private"}, http.StatusNoContent, nil)
	r.eventually("the client registers again in mode private", func() bool {
		tn, ok := r.tunnels()["rdp"]
		return ok && tn.ID != second
	})
	if s = find(); s.AccessMode != "private" || !s.Connected || s.RemotePort != 0 {
		t.Fatalf("mode private: %+v", s)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("something listens on the port of a private service")
	}
	// An http mode is refused for it.
	if code, body := r.admin(http.MethodPut, "/api/v1/services/"+s.ID+"/access-mode", map[string]string{"access_mode": "api_key"}); code != http.StatusConflict {
		t.Fatalf("api_key on tcp: %d %s", code, body)
	}
}
