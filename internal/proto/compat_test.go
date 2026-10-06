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
	decode(AuthResponse{OK: true, SessionID: "s", RelayVersion: "0.7.0", UserEmail: "a@b.example"}, &resp)
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
	var ar AuthResponse
	if err := DecodePayload(Envelope{Payload: json.RawMessage(`{"ok":false,"error":"invalid token"}`)}, &ar); err != nil {
		t.Fatal(err)
	}
	if ar != (AuthResponse{Error: "invalid token"}) {
		t.Errorf("auth response: %+v", ar)
	}
}
