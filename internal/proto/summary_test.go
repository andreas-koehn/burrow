package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The message as it travels, written out: a peer built from other code reads
// and writes exactly this.
func TestRequestSummary_OnTheWire(t *testing.T) {
	if MsgRequestSummary != "request_summary" || CapRequestSummaries != "request_summaries" {
		t.Fatalf("names: %q %q", MsgRequestSummary, CapRequestSummaries)
	}
	const wire = `{"tunnel_id":"t1","time":"2026-10-05T14:02:11Z","method":"GET","path":"/api/users","status":200,"duration_ms":12}`
	b, err := json.Marshal(RequestSummary{TunnelID: "t1", Time: "2026-10-05T14:02:11Z", Method: "GET", Path: "/api/users", Status: 200, DurationMs: 12})
	if err != nil || string(b) != wire {
		t.Fatalf("written:\n got %s\nwant %s (%v)", b, wire, err)
	}
	var s RequestSummary
	// A field this code does not know is not a reason to refuse the message.
	if err := DecodePayload(Envelope{Payload: json.RawMessage(strings.Replace(wire, `}`, `,"later":"x"}`, 1))}, &s); err != nil {
		t.Fatal(err)
	}
	if s != (RequestSummary{TunnelID: "t1", Time: "2026-10-05T14:02:11Z", Method: "GET", Path: "/api/users", Status: 200, DurationMs: 12}) {
		t.Fatalf("read: %+v", s)
	}
	// The message has these six fields and no other: nothing of the request's
	// content has a place in it.
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(b, &keys)
	for k := range keys {
		switch k {
		case "tunnel_id", "time", "method", "path", "status", "duration_ms":
		default:
			t.Errorf("field %q", k)
		}
	}
	if len(keys) != 6 {
		t.Errorf("%d fields", len(keys))
	}
}

// A client that announces the capability adds one field to the auth request;
// one that does not writes the request of before.
func TestAuthRequest_Capability(t *testing.T) {
	b, _ := json.Marshal(AuthRequest{ProtocolVersion: 1, Token: "bur_test_0000", ClientVersion: "0.7.0", OS: "linux", Arch: "amd64",
		Capabilities: []string{CapRequestSummaries}})
	if want := `{"protocol_version":1,"token":"bur_test_0000","client_version":"0.7.0","os":"linux","arch":"amd64","capabilities":["request_summaries"]}`; string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

func TestSummaryPath(t *testing.T) {
	long := "/" + strings.Repeat("a", 300)
	cases := []struct{ name, in, want string }{
		{"plain", "/api/users", "/api/users"},
		{"empty", "", "/"},
		{"non-ASCII stays", "/grüße/日本", "/grüße/日本"},
		{"escape", "/a\x1b[2Jb", "/a?[2Jb"},
		{"line breaks and tab", "/a\r\nb\tc", "/a??b?c"},
		{"NUL and DEL", "/a\x00b\x7f", "/a?b?"},
		{"C1 control", "/a\u009bb", "/a?b"},
		{"right-to-left override", "/a\u202eb", "/a?b"},
		{"isolates", "/a\u2066b\u2069", "/a?b?"},
		{"zero width", "/a\u200bb\u200d\ufeff", "/a?b??"},
		{"line separator", "/a\u2028b\u2029", "/a?b?"},
		{"not UTF-8", "/a\xffb\xc3", "/a?b?"},
		{"too long", long, long[:256]},
	}
	for _, c := range cases {
		if got := SummaryPath(c.in); got != c.want {
			t.Errorf("%s: SummaryPath(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	// The cut never leaves half a character, wherever it falls.
	for pad := 0; pad < 4; pad++ {
		in := "/" + strings.Repeat("a", 250+pad) + strings.Repeat("日", 10)
		got := SummaryPath(in)
		if len(got) > MaxSummaryPath || len(got) < MaxSummaryPath-2 || !utf8.ValidString(got) || !strings.HasPrefix(in, got) {
			t.Errorf("pad %d: %d bytes, valid %v: %q", pad, len(got), utf8.ValidString(got), got)
		}
	}
	// Replacing does not make room for more than the limit either.
	if got := SummaryPath(strings.Repeat("\u202e", 400)); got != strings.Repeat("?", 256) {
		t.Errorf("400 overrides: %d bytes", len(got))
	}
	// Whatever goes in, what comes out is short, valid and without the
	// characters a terminal acts on; and doing it twice changes nothing.
	for _, in := range []string{"\x1b]0;title\x07", strings.Repeat("\xff", 1000), "/\u200e\u200f\u061c", "/ok"} {
		got := SummaryPath(in)
		if len(got) > MaxSummaryPath || !utf8.ValidString(got) || SummaryPath(got) != got {
			t.Errorf("SummaryPath(%q) = %q", in, got)
		}
		for _, r := range got {
			if unprintable(r) {
				t.Errorf("SummaryPath(%q) kept %U", in, r)
			}
		}
	}
}

func TestSummaryMethod(t *testing.T) {
	cases := []struct{ in, want string }{
		{"GET", "GET"}, {"M-SEARCH", "M-SEARCH"}, {"", ""},
		{"GE\x1bT", "GE?T"}, {"G T", "G?T"}, {"GÉT", "G?T"},
		{strings.Repeat("A", 40), strings.Repeat("A", 16)},
	}
	for _, c := range cases {
		if got := SummaryMethod(c.in); got != c.want {
			t.Errorf("SummaryMethod(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
