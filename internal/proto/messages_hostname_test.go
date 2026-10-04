package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTunnelRegisterResponseHostnameRoundTrips(t *testing.T) {
	in := TunnelRegisterResponse{OK: true, TunnelID: "t1", Hostname: "k7p2qx.tunnels.example.com"}
	b, _ := json.Marshal(in)
	if !strings.Contains(string(b), `"hostname":"k7p2qx.tunnels.example.com"`) {
		t.Fatalf("hostname not serialized: %s", b)
	}
	var out TunnelRegisterResponse
	if err := json.Unmarshal(b, &out); err != nil || out.Hostname != in.Hostname {
		t.Fatalf("round-trip: %+v err=%v", out, err)
	}
	// omitempty: a tcp response carries no hostname key
	b2, _ := json.Marshal(TunnelRegisterResponse{OK: true, RemotePort: 9000})
	if strings.Contains(string(b2), "hostname") {
		t.Fatalf("hostname should be omitted: %s", b2)
	}
}

func TestAuthRequestHostnameReserved(t *testing.T) {
	// omitempty: absent when zero
	b, err := json.Marshal(AuthRequest{Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hostname") {
		t.Fatalf("hostname must be omitted when empty: %s", b)
	}
	// round-trips when set
	var got AuthRequest
	if err := json.Unmarshal([]byte(`{"token":"t","hostname":"box-1"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "box-1" {
		t.Fatalf("hostname not decoded: %+v", got)
	}
}

func TestTunnelRegisterResponse_URLRoundTrip(t *testing.T) {
	in := TunnelRegisterResponse{OK: true, TunnelID: "t1", URL: "https://burrow.example.com/svc/p7baeh/"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"url":"https://burrow.example.com/svc/p7baeh/"`) || strings.Contains(string(b), "hostname") {
		t.Fatalf("wire form = %s", b)
	}
	var out TunnelRegisterResponse
	if err := json.Unmarshal(b, &out); err != nil || out.URL != in.URL {
		t.Fatalf("round trip: %v %+v", err, out)
	}
}
