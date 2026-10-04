package main

import (
	"context"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
)

type fakeSnapshotter struct{ sessions []server.SessionSnapshot }

func (f fakeSnapshotter) SnapshotSessions() []server.SessionSnapshot { return f.sessions }

type fakeTunnelGetter struct{ rows map[string]db.Tunnel }

func (f fakeTunnelGetter) GetTunnel(_ context.Context, id string) (db.Tunnel, error) {
	r, ok := f.rows[id]
	if !ok {
		return db.Tunnel{}, db.ErrNotFound
	}
	return r, nil
}

func TestClientsAdapter(t *testing.T) {
	snap := fakeSnapshotter{sessions: []server.SessionSnapshot{{
		SessionID: "c1", UserID: "u1", RemoteAddr: "1.1.1.1:5",
		OS: "linux", Arch: "amd64", ClientVersion: "9.9", Token: "laptop",
		Tunnels: []server.TunnelView{
			{ID: "tn1", Name: "web", Type: "tcp", RemotePort: 9000, LocalAddr: "127.0.0.1:3000", BytesIn: 5, BytesOut: 7, Connected: true},
			{ID: "tn2", Name: "ssh", Type: "tcp", RemotePort: 9001, LocalAddr: "127.0.0.1:22", BytesIn: 1, BytesOut: 2, Connected: true},
		},
	}}}
	tg := fakeTunnelGetter{rows: map[string]db.Tunnel{
		"tn1": {ID: "tn1", TotalBytesIn: 100, TotalBytesOut: 40, AccessMode: "api_key"},
		"tn2": {ID: "tn2", TotalBytesIn: 10, TotalBytesOut: 3, AccessMode: "open"},
	}}
	a := clientsAdapter{srv: snap, st: tg}

	list := a.ListClients()
	if len(list) != 1 {
		t.Fatalf("want 1 client, got %d", len(list))
	}
	cv := list[0]
	if cv.SessionID != "c1" || cv.TokenName != "laptop" || cv.OS != "linux" || cv.ServiceCount != 2 {
		t.Fatalf("client view: %+v", cv)
	}
	if cv.TotalBytesIn != 110 || cv.TotalBytesOut != 43 {
		t.Fatalf("aggregate totals wrong: %+v", cv)
	}

	cd, ok := a.GetClient("c1")
	if !ok || len(cd.Services) != 2 {
		t.Fatalf("detail: ok=%v services=%d", ok, len(cd.Services))
	}
	var tn1 *struct {
		live  uint64
		total int64
		mode  string
	}
	for _, s := range cd.Services {
		if s.ID == "tn1" {
			tn1 = &struct {
				live  uint64
				total int64
				mode  string
			}{s.BytesIn, s.TotalBytesIn, s.AccessMode}
		}
	}
	if tn1 == nil || tn1.live != 5 || tn1.total != 100 || tn1.mode != "api_key" {
		t.Fatalf("tn1 service view wrong: %+v", tn1)
	}

	if _, ok := a.GetClient("nope"); ok {
		t.Fatal("GetClient(nope) must be !ok")
	}
}

func TestClientsAdapterUsesDurableServiceMode(t *testing.T) {
	snap := fakeSnapshotter{sessions: []server.SessionSnapshot{{
		SessionID: "c1", UserID: "u1", Token: "laptop",
		Tunnels: []server.TunnelView{
			{ID: "tn1", Name: "ollama", Type: "http", ServiceID: "svc-ollama", Connected: true},
			{ID: "tn2", Name: "ssh", Type: "tcp", RemotePort: 9001, Connected: true},
		},
	}}}
	tg := fakeTunnelGetter{rows: map[string]db.Tunnel{
		"tn1": {ID: "tn1", AccessMode: "open"},
		"tn2": {ID: "tn2", AccessMode: "open"},
	}}
	a := clientsAdapter{
		srv:      snap,
		st:       tg,
		services: fakeServiceModeGetter{modes: map[string]string{"svc-ollama": "api_key"}},
	}
	cd, ok := a.GetClient("c1")
	if !ok || len(cd.Services) != 2 {
		t.Fatalf("detail: ok=%v services=%d", ok, len(cd.Services))
	}
	if cd.Services[0].AccessMode != "api_key" || cd.Services[0].ServiceID != "svc-ollama" {
		t.Fatalf("http service: want api_key/svc-ollama, got %+v", cd.Services[0])
	}
	if cd.Services[1].AccessMode != "open" || cd.Services[1].ServiceID != "" {
		t.Fatalf("tcp service: want open/empty service id, got %+v", cd.Services[1])
	}
}
