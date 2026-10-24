package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The message shapes before slug, access and the error codes were added.
// They stand for a peer built from the older code.
type (
	oldAuthRequest struct {
		ProtocolVersion int    `json:"protocol_version"`
		Token           string `json:"token"`
		ClientVersion   string `json:"client_version"`
		OS              string `json:"os"`
		Arch            string `json:"arch"`
		Hostname        string `json:"hostname,omitempty"`
	}
	oldAuthResponse struct {
		OK        bool   `json:"ok"`
		SessionID string `json:"session_id,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	oldTunnelRegister struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		RemotePort int    `json:"remote_port"`
		LocalAddr  string `json:"local_addr"`
	}
	oldTunnelRegisterResponse struct {
		OK         bool   `json:"ok"`
		TunnelID   string `json:"tunnel_id,omitempty"`
		RemotePort int    `json:"remote_port,omitempty"`
		URL        string `json:"url,omitempty"`
		Hostname   string `json:"hostname,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	oldError struct {
		Message string `json:"message"`
	}
)

func TestProtocolVersionIsStillOne(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d; the additions are optional fields, the version stays 1", ProtocolVersion)
	}
}

// A message that uses none of the additions is, byte for byte, what the older
// code wrote.
func TestWithoutTheAdditionsTheBytesAreUnchanged(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"auth request", AuthRequest{ProtocolVersion: 1, Token: "bur_test_0000", ClientVersion: "0.6.0", OS: "linux", Arch: "amd64"},
			`{"protocol_version":1,"token":"bur_test_0000","client_version":"0.6.0","os":"linux","arch":"amd64"}`},
		{"auth request, no capabilities", AuthRequest{ProtocolVersion: 1, Capabilities: []string{}},
			`{"protocol_version":1,"token":"","client_version":"","os":"","arch":""}`},
		{"auth response", AuthResponse{OK: true, SessionID: "s1"}, `{"ok":true,"session_id":"s1"}`},
		{"auth response, no capabilities", AuthResponse{OK: true, SessionID: "s1", Capabilities: []string{}}, `{"ok":true,"session_id":"s1"}`},
		{"tcp register without access", TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389"},
			`{"name":"rdp","type":"tcp","remote_port":0,"local_addr":"127.0.0.1:3389"}`},
		{"tcp register with a default", TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", AccessDefault: "private"},
			`{"name":"rdp","type":"tcp","remote_port":0,"local_addr":"127.0.0.1:3389","access_default":"private"}`},
		{"auth refusal", AuthResponse{Error: "invalid token"}, `{"ok":false,"error":"invalid token"}`},
		{"register", TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"},
			`{"name":"web","type":"http","remote_port":0,"local_addr":"127.0.0.1:3000"}`},
		{"register response", TunnelRegisterResponse{OK: true, TunnelID: "t1", URL: "https://b.example/svc/abc234/"},
			`{"ok":true,"tunnel_id":"t1","url":"https://b.example/svc/abc234/"}`},
		{"register refusal", TunnelRegisterResponse{Error: "http tunnels not configured"},
			`{"ok":false,"error":"http tunnels not configured"}`},
		{"error", Error{Message: "boom"}, `{"message":"boom"}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.val)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
}

// An older peer reads a message that carries the additions and sees its own
// fields unchanged; DecodePayload does not refuse a field it does not know.
func TestOlderPeerReadsNewMessages(t *testing.T) {
	decode := func(v any, into any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := DecodePayload(Envelope{Payload: raw}, into); err != nil {
			t.Fatalf("decoding %s: %v", raw, err)
		}
	}
	var ar oldAuthRequest
	decode(AuthRequest{ProtocolVersion: 1, Token: "bur_test_0000", ClientVersion: "0.7.0", OS: "linux", Arch: "arm64",
		Hostname: "h", Capabilities: []string{"request_summaries"}}, &ar)
	if ar != (oldAuthRequest{1, "bur_test_0000", "0.7.0", "linux", "arm64", "h"}) {
		t.Errorf("auth request: %+v", ar)
	}
	var resp oldAuthResponse
	decode(AuthResponse{OK: true, SessionID: "s", RelayVersion: "0.7.0", Capabilities: []string{CapTCPAccess, CapForward}}, &resp)
	if resp != (oldAuthResponse{OK: true, SessionID: "s"}) {
		t.Errorf("auth response: %+v", resp)
	}
	var refused oldAuthResponse
	decode(AuthResponse{Error: "invalid token", Code: CodeInvalidToken}, &refused)
	if refused != (oldAuthResponse{Error: "invalid token"}) {
		t.Errorf("auth refusal: %+v", refused)
	}
	var tr oldTunnelRegister
	decode(TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app", Access: "burrow_login"}, &tr)
	if tr != (oldTunnelRegister{"web", "http", 0, "127.0.0.1:3000"}) {
		t.Errorf("register: %+v", tr)
	}
	var rr oldTunnelRegisterResponse
	decode(TunnelRegisterResponse{OK: true, TunnelID: "t", URL: "u", AccessMode: "open", Created: true,
		DashboardURL: "d", Ignored: []string{"access", "slug"}}, &rr)
	if rr != (oldTunnelRegisterResponse{OK: true, TunnelID: "t", URL: "u"}) {
		t.Errorf("register response: %+v", rr)
	}
	var tcp oldTunnelRegisterResponse
	decode(TunnelRegisterResponse{OK: true, TunnelID: "t", AccessMode: "private", Created: true, Slug: "k7p2qx"}, &tcp)
	if tcp != (oldTunnelRegisterResponse{OK: true, TunnelID: "t"}) {
		t.Errorf("private tcp register response: %+v", tcp)
	}
	var tcpReg oldTunnelRegister
	decode(TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"}, &tcpReg)
	if tcpReg != (oldTunnelRegister{"rdp", "tcp", 0, "127.0.0.1:3389"}) {
		t.Errorf("tcp register: %+v", tcpReg)
	}
	// An older relay ignores the default for a new service; it creates the
	// service open, as it always has (the client guards against that).
	var tcpDefault oldTunnelRegister
	decode(TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", AccessDefault: "private"}, &tcpDefault)
	if tcpDefault != (oldTunnelRegister{"rdp", "tcp", 0, "127.0.0.1:3389"}) {
		t.Errorf("tcp register with a default: %+v", tcpDefault)
	}
	var e oldError
	decode(Error{Message: "boom", Code: CodeInternal}, &e)
	if e != (oldError{"boom"}) {
		t.Errorf("error: %+v", e)
	}
}

// A message of an older peer decodes with the additions at their zero value.
func TestNewPeerReadsOlderMessages(t *testing.T) {
	var rr TunnelRegisterResponse
	if err := DecodePayload(Envelope{Payload: json.RawMessage(`{"ok":true,"tunnel_id":"t1","url":"https://b.example/svc/abc234/"}`)}, &rr); err != nil {
		t.Fatal(err)
	}
	want := TunnelRegisterResponse{OK: true, TunnelID: "t1", URL: "https://b.example/svc/abc234/"}
	if !reflect.DeepEqual(rr, want) {
		t.Errorf("register response: %+v", rr)
	}
	// An older client sends no default for a new service.
	var reg TunnelRegister
	if err := DecodePayload(Envelope{Payload: json.RawMessage(`{"name":"rdp","type":"tcp","remote_port":0,"local_addr":"127.0.0.1:3389"}`)}, &reg); err != nil {
		t.Fatal(err)
	}
	if reg != (TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389"}) {
		t.Errorf("register of an older client: %+v", reg)
	}
	var ar AuthResponse
	if err := DecodePayload(Envelope{Payload: json.RawMessage(`{"ok":false,"error":"invalid token"}`)}, &ar); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ar, AuthResponse{Error: "invalid token"}) {
		t.Errorf("auth response: %+v", ar)
	}
	// An older relay announces no capabilities: the client sees none.
	var ok AuthResponse
	if err := DecodePayload(Envelope{Payload: json.RawMessage(`{"ok":true,"session_id":"s1","relay_version":"0.12.0"}`)}, &ok); err != nil {
		t.Fatal(err)
	}
	if ok.Capabilities != nil {
		t.Errorf("capabilities of an older relay: %v", ok.Capabilities)
	}
}

// The relay capabilities travel as they are named; they are part of the wire
// contract.
func TestRelayCapabilityNames(t *testing.T) {
	if CapTCPAccess != "tcp_access" || CapForward != "forward" || CodeRateLimited != "rate_limited" {
		t.Fatalf("names changed: %q %q %q", CapTCPAccess, CapForward, CodeRateLimited)
	}
	b, err := json.Marshal(AuthResponse{OK: true, SessionID: "s", Capabilities: []string{CapTCPAccess}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"ok":true,"session_id":"s","capabilities":["tcp_access"]}` {
		t.Errorf("got %s", b)
	}
}

// The forward stream is new: its messages travel as they are named, and a
// refusal carries nothing it does not need.
func TestForwardMessages(t *testing.T) {
	if MsgForwardOpen != "forward_open" || MsgForwardOpenResp != "forward_open_response" {
		t.Fatalf("message names changed: %q %q", MsgForwardOpen, MsgForwardOpenResp)
	}
	if CodeNotFound != "not_found" || CodeOffline != "offline" {
		t.Fatalf("code names changed: %q %q", CodeNotFound, CodeOffline)
	}
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"open", ForwardOpen{Target: "rdp"}, `{"target":"rdp"}`},
		{"refusal", ForwardOpenResponse{Code: CodeNotFound, Error: "no such service"},
			`{"ok":false,"code":"not_found","error":"no such service"}`},
		{"accepted", ForwardOpenResponse{OK: true, ServiceID: "s1", Name: "rdp"},
			`{"ok":true,"service_id":"s1","name":"rdp"}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.val)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
	// A forward_open is a frame like any other: a relay that reads it as a
	// data stream header finds no stream id, and closes the stream.
	var sh StreamHeader
	raw, _ := json.Marshal(ForwardOpen{Target: "rdp"})
	if err := DecodePayload(Envelope{Type: MsgForwardOpen, Payload: raw}, &sh); err != nil || sh.StreamID != "" {
		t.Errorf("as a stream header: %+v, %v", sh, err)
	}
}

// new_connection may tell the home client to write a PROXY protocol v2 header
// and the address the visitor reached (tcp complete, E2); a tcp register may
// say that the client writes one by itself. All of it is optional: without it
// the bytes are what they were, and an older peer reads its own fields.
func TestProxyProtocolFields(t *testing.T) {
	for _, c := range []struct {
		name string
		val  any
		want string
	}{
		{"new_connection without proxy protocol", NewConnection{TunnelID: "t", StreamID: "s", SourceIP: "203.0.113.7:50000"},
			`{"tunnel_id":"t","stream_id":"s","source_ip":"203.0.113.7:50000"}`},
		{"new_connection with proxy protocol", NewConnection{TunnelID: "t", StreamID: "s", SourceIP: "203.0.113.7:50000",
			DestAddr: "198.51.100.1:9001", ProxyProtocol: ProxyProtocolV2},
			`{"tunnel_id":"t","stream_id":"s","source_ip":"203.0.113.7:50000","dest_addr":"198.51.100.1:9001","proxy_protocol":"v2"}`},
		{"tcp register with proxy protocol", TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "127.0.0.1:8443", ProxyProtocol: ProxyProtocolV2},
			`{"name":"web","type":"tcp","remote_port":0,"local_addr":"127.0.0.1:8443","proxy_protocol":"v2"}`},
	} {
		b, err := json.Marshal(c.val)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
	type oldNewConnection struct {
		TunnelID string `json:"tunnel_id"`
		StreamID string `json:"stream_id"`
		SourceIP string `json:"source_ip"`
	}
	raw, _ := json.Marshal(NewConnection{TunnelID: "t", StreamID: "s", SourceIP: "forward:192.0.2.4:4000", DestAddr: "x", ProxyProtocol: ProxyProtocolV2})
	var old oldNewConnection
	if err := DecodePayload(Envelope{Payload: raw}, &old); err != nil || old != (oldNewConnection{"t", "s", "forward:192.0.2.4:4000"}) {
		t.Errorf("older client: %+v %v", old, err)
	}
	raw, _ = json.Marshal(TunnelRegister{Name: "web", Type: "tcp", LocalAddr: "x", ProxyProtocol: ProxyProtocolV2})
	var oldReg oldTunnelRegister
	if err := DecodePayload(Envelope{Payload: raw}, &oldReg); err != nil || oldReg != (oldTunnelRegister{"web", "tcp", 0, "x"}) {
		t.Errorf("older relay: %+v %v", oldReg, err)
	}
	var nc NewConnection
	if err := DecodePayload(Envelope{Payload: json.RawMessage(`{"tunnel_id":"t","stream_id":"s","source_ip":"1.2.3.4:5"}`)}, &nc); err != nil ||
		nc != (NewConnection{TunnelID: "t", StreamID: "s", SourceIP: "1.2.3.4:5"}) {
		t.Errorf("older relay's new_connection: %+v %v", nc, err)
	}
	if ProxyProtocolV2 != "v2" {
		t.Errorf("ProxyProtocolV2 = %q", ProxyProtocolV2)
	}
}

// An http register may name the upstream protocol, and a relay of this round
// announces upstream_protocol (HTTP/2 and gRPC, H4). Both are optional:
// without them the bytes are what they were, and an older relay reads its own
// fields of a new register.
func TestUpstreamProtocolFields(t *testing.T) {
	if CapUpstreamProtocol != "upstream_protocol" {
		t.Fatalf("capability name changed: %q", CapUpstreamProtocol)
	}
	if UpstreamHTTP1 != "http1" || UpstreamHTTPS != "https" || UpstreamH2C != "h2c" || UpstreamH2 != "h2" {
		t.Fatalf("protocol names changed: %q %q %q %q", UpstreamHTTP1, UpstreamHTTPS, UpstreamH2C, UpstreamH2)
	}
	for _, c := range []struct {
		name string
		val  any
		want string
	}{
		{"http register without a protocol", TunnelRegister{Name: "web", Type: "http", LocalAddr: "127.0.0.1:3000"},
			`{"name":"web","type":"http","remote_port":0,"local_addr":"127.0.0.1:3000"}`},
		{"http register with a protocol", TunnelRegister{Name: "api", Type: "http", LocalAddr: "127.0.0.1:50051", UpstreamProtocol: UpstreamH2C},
			`{"name":"api","type":"http","remote_port":0,"local_addr":"127.0.0.1:50051","upstream_protocol":"h2c"}`},
		{"capability", AuthResponse{OK: true, SessionID: "s", Capabilities: []string{CapUpstreamProtocol}},
			`{"ok":true,"session_id":"s","capabilities":["upstream_protocol"]}`},
	} {
		b, err := json.Marshal(c.val)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
	raw, _ := json.Marshal(TunnelRegister{Name: "api", Type: "http", LocalAddr: "x", UpstreamProtocol: UpstreamH2})
	var oldReg oldTunnelRegister
	if err := DecodePayload(Envelope{Payload: raw}, &oldReg); err != nil || oldReg != (oldTunnelRegister{"api", "http", 0, "x"}) {
		t.Errorf("older relay: %+v %v", oldReg, err)
	}
	var tr TunnelRegister
	if err := DecodePayload(Envelope{Payload: json.RawMessage(`{"name":"web","type":"http","remote_port":0,"local_addr":"x"}`)}, &tr); err != nil || tr.UpstreamProtocol != "" {
		t.Errorf("older client's register: %+v %v", tr, err)
	}
	for _, s := range []string{UpstreamHTTP1, UpstreamHTTPS, UpstreamH2C, UpstreamH2} {
		if !ValidUpstreamProtocol(s) {
			t.Errorf("%q not valid", s)
		}
	}
	for _, s := range []string{"", "HTTP1", "http2", "grpc", "h3"} {
		if ValidUpstreamProtocol(s) {
			t.Errorf("%q valid", s)
		}
	}
}

// RequestSummary.grpc_status is optional: absent for a request that is no
// gRPC call, so an older client reads the summary as before, and a summary
// from an older relay decodes with no status.
func TestRequestSummaryGRPCStatus(t *testing.T) {
	plain := RequestSummary{TunnelID: "t", Time: "x", Method: "GET", Path: "/", Status: 200, DurationMs: 3}
	b, _ := json.Marshal(plain)
	if want := `{"tunnel_id":"t","time":"x","method":"GET","path":"/","status":200,"duration_ms":3}`; string(b) != want {
		t.Fatalf("plain summary\n got %s\nwant %s", b, want)
	}
	ok := 0
	call := plain
	call.Method, call.Path, call.GRPCStatus = "POST", "/pkg.S/M", &ok
	b, _ = json.Marshal(call)
	if want := `{"tunnel_id":"t","time":"x","method":"POST","path":"/pkg.S/M","status":200,"duration_ms":3,"grpc_status":0}`; string(b) != want {
		t.Fatalf("gRPC summary\n got %s\nwant %s", b, want)
	}
	var old struct {
		TunnelID string `json:"tunnel_id"`
		Status   int    `json:"status"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.Status != 200 {
		t.Fatalf("older client: %+v %v", old, err)
	}
	var back RequestSummary
	if err := json.Unmarshal([]byte(`{"tunnel_id":"t","status":200}`), &back); err != nil || back.GRPCStatus != nil {
		t.Fatalf("older relay's summary: %+v %v", back, err)
	}
}
