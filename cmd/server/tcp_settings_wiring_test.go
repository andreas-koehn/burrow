package main

// tcp complete (E2/E4): the PROXY protocol and idle timeout of a tcp service
// reach the relay at registration and on every refresh.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// The relay and the store agree on the idle timeout range.
func TestTCPIdleTimeout_OneRange(t *testing.T) {
	if time.Duration(store.MinTCPIdleTimeoutS)*time.Second != server.MinTCPIdleTimeout ||
		time.Duration(store.MaxTCPIdleTimeoutS)*time.Second != server.MaxTCPIdleTimeout {
		t.Fatalf("store %d..%d s, relay %v..%v", store.MinTCPIdleTimeoutS, store.MaxTCPIdleTimeoutS,
			server.MinTCPIdleTimeout, server.MaxTCPIdleTimeout)
	}
}

func TestTCPSettings_ResolverAndLoaderReadThem(t *testing.T) {
	x, uid := optionsDB(t)
	ctx := context.Background()
	res := serviceResolverAdapter{db: x}
	r, err := res.ResolveTCP(ctx, uid, "web", server.ResolveOptions{Access: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ProxyProtocol != "" || r.IdleTimeout != 0 {
		t.Fatalf("new service: %+v", r)
	}
	if err := x.SetServiceTCPSettings(ctx, r.ServiceID, "v2", 900); err != nil {
		t.Fatal(err)
	}
	again, err := res.ResolveTCP(ctx, uid, "web", server.ResolveOptions{})
	if err != nil || again.ProxyProtocol != "v2" || again.IdleTimeout != 15*time.Minute {
		t.Fatalf("resolved %+v %v", again, err)
	}
	acc, err := tcpAccessAdapter{db: x}.TCPAccess(ctx, r.ServiceID)
	if err != nil || acc.ProxyProtocol != "v2" || acc.IdleTimeout != 15*time.Minute {
		t.Fatalf("access %+v %v", acc, err)
	}
}

// The whole relay: a PUT of the tcp settings reaches the live tunnel without
// a new registration. The next visitor arrives at the target behind a PROXY
// v2 header that names it, the settings read back, and the connection log
// says what each connection spoke.
func TestServe_TCPSettings(t *testing.T) {
	r := bootObsRelay(t)
	sig := []byte("\r\n\r\n\x00\r\nQUIT\n")
	var mu sync.Mutex
	var sources []string
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				first := make([]byte, 12)
				if _, err := io.ReadFull(c, first); err != nil {
					return
				}
				src := "none"
				if bytes.Equal(first, sig) {
					rest := make([]byte, 4)
					if _, err := io.ReadFull(c, rest); err != nil {
						return
					}
					addrs := make([]byte, binary.BigEndian.Uint16(rest[2:4]))
					if _, err := io.ReadFull(c, addrs); err != nil || rest[1] != 0x11 {
						return
					}
					src = fmt.Sprintf("%s:%d", net.IP(addrs[0:4]), binary.BigEndian.Uint16(addrs[8:10]))
					first = nil
				}
				mu.Lock()
				sources = append(sources, src)
				mu.Unlock()
				_ = c.SetReadDeadline(time.Time{})
				_, _ = c.Write(first)
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	var token struct {
		Token string `json:"token"`
	}
	r.mustAdmin(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "tcp-settings"}, http.StatusCreated, &token)
	stop := r.connectWith(token.Token, client.TunnelSpec{Name: "web", Type: "tcp", LocalAddr: target.Addr().String()})
	defer stop()
	var id string
	r.eventually("the service is served", func() bool {
		var rows []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Connected bool   `json:"connected"`
		}
		r.mustAdmin(http.MethodGet, "/api/v1/services", nil, http.StatusOK, &rows)
		for _, s := range rows {
			if s.Name == "web" && s.Connected {
				id = s.ID
			}
		}
		return id != ""
	})
	tunnel := r.tunnels()["web"].ID
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(r.tcpPort))
	banner := "SSH-2.0-burrow-test\r\n"
	visit := func() string {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte(banner)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(banner))
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != banner {
			t.Fatalf("echo %q %v", buf, err)
		}
		return conn.LocalAddr().String()
	}
	visit()

	type settings struct {
		ProxyProtocol       string `json:"proxy_protocol"`
		IdleTimeoutS        int    `json:"idle_timeout_s"`
		ClientProxyProtocol string `json:"client_proxy_protocol"`
	}
	var got settings
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-settings", map[string]any{"proxy_protocol": "v2", "idle_timeout_s": 60}, http.StatusOK, &got)
	if got != (settings{ProxyProtocol: "v2", IdleTimeoutS: 60}) {
		t.Fatalf("PUT answer %+v", got)
	}
	visitor := visit()
	mu.Lock()
	seen := append([]string(nil), sources...)
	mu.Unlock()
	if len(seen) != 2 || seen[0] != "none" || seen[1] != visitor {
		t.Fatalf("the target saw %v, the second visitor was %s", seen, visitor)
	}
	if tn := r.tunnels()["web"].ID; tn != tunnel {
		t.Fatalf("the client registered again (%s → %s)", tunnel, tn)
	}
	got = settings{}
	r.mustAdmin(http.MethodGet, "/api/v1/services/"+id+"/tcp-settings", nil, http.StatusOK, &got)
	if got != (settings{ProxyProtocol: "v2", IdleTimeoutS: 60}) {
		t.Fatalf("GET %+v", got)
	}
	r.mustAdmin(http.MethodPut, "/api/v1/services/"+id+"/tcp-settings", map[string]any{"idle_timeout_s": 30}, http.StatusBadRequest, nil)
	r.eventually("rows that say ssh", func() bool {
		var rows []struct {
			Protocol string `json:"protocol"`
		}
		r.mustAdmin(http.MethodGet, "/api/v1/connection-logs?kind=tcp_proxy&service_id="+id, nil, http.StatusOK, &rows)
		n := 0
		for _, row := range rows {
			if row.Protocol == "ssh" {
				n++
			}
		}
		return n == 2
	})
}
