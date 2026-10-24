package grpcwire

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		ct   string
		want Class
	}{
		{"application/grpc", ClassGRPC},
		{"application/grpc+proto", ClassGRPC},
		{"application/grpc+json", ClassGRPC},
		{"application/grpc; charset=utf-8", ClassGRPC},
		{"Application/GRPC", ClassGRPC},
		{" application/grpc ", ClassGRPC},
		{"application/grpc-web", ClassGRPCWeb},
		{"application/grpc-web+proto", ClassGRPCWeb},
		{"application/grpc-web-text", ClassGRPCWeb},
		{"application/grpc-web-text+proto", ClassGRPCWeb},
		{"application/connect+proto", ClassConnectStream},
		{"application/connect+json", ClassConnectStream},
		{"application/json", ClassNone},
		{"application/grpcx", ClassNone},
		{"application/grp", ClassNone},
		{"text/plain", ClassNone},
		{"", ClassNone},
	} {
		r := httptest.NewRequest("POST", "/pkg.Svc/Method", nil)
		if c.ct != "" {
			r.Header.Set("Content-Type", c.ct)
		}
		if got := Classify(r); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.ct, got, c.want)
		}
	}
}

func TestClass_Shaped(t *testing.T) {
	for c, want := range map[Class]bool{ClassNone: false, ClassGRPC: true, ClassGRPCWeb: true, ClassConnectStream: false} {
		if c.GRPCShaped() != want {
			t.Errorf("%v.GRPCShaped() = %v", c, !want)
		}
	}
	if ClassGRPCWeb.String() != "grpc-web" || ClassGRPC.String() != "grpc" || ClassNone.String() != "none" {
		t.Error("String")
	}
}

func TestParseTimeout(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{"1H", time.Hour},
		{"2M", 2 * time.Minute},
		{"30S", 30 * time.Second},
		{"150m", 150 * time.Millisecond},
		{"250u", 250 * time.Microsecond},
		{"999n", 999 * time.Nanosecond},
		{"0S", 0},
		{"00000001S", time.Second},
		{"99999999n", 99999999 * time.Nanosecond},
		{"99999999H", 1<<63 - 1}, // clamped
	} {
		got, err := ParseTimeout(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseTimeout(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "S", "1", "1s", "1h", "-1S", "+1S", "1.5S", "1 S", "1SS", "123456789S", "1X", "aS", " 1S", "1S "} {
		if _, err := ParseTimeout(bad); err == nil {
			t.Errorf("ParseTimeout(%q) accepted", bad)
		}
	}
}

func TestFormatTimeout(t *testing.T) {
	for _, c := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0n"},
		{-time.Second, "0n"},
		{1, "1n"},
		{99999999, "99999999n"},
		{100000000, "100000u"},
		{time.Second, "1000000u"},
		{1500 * time.Microsecond, "1500000n"},
		{time.Minute, "60000000u"},
		{10 * time.Hour, "36000000m"},
		{1000 * time.Hour, "3600000S"},
		{100000 * time.Hour, "6000000M"},
		{1<<63 - 1, "2562048H"},
	} {
		if got := FormatTimeout(c.in); got != c.want {
			t.Errorf("FormatTimeout(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A formatted timeout parses back to at least the duration, and is never
// longer than 8 digits.
func TestTimeout_RoundTrip(t *testing.T) {
	for _, d := range []time.Duration{0, 1, 999, 100000001, 1234567891, time.Second + 1, 17 * time.Minute, 5*time.Hour + 1, 24 * time.Hour, 1<<63 - 1} {
		s := FormatTimeout(d)
		if len(s) > 9 {
			t.Errorf("%v: %q is too long", d, s)
		}
		got, err := ParseTimeout(s)
		if err != nil {
			t.Errorf("%v: %q: %v", d, s, err)
			continue
		}
		if got < d {
			t.Errorf("%v -> %q -> %v is shorter", d, s, got)
		}
		if d > 0 && d < 1<<62 && got > d+d/1000+time.Hour {
			t.Errorf("%v -> %q -> %v is far longer", d, s, got)
		}
	}
}

func TestStatusForHTTP(t *testing.T) {
	for _, c := range []struct {
		http int
		want Code
	}{
		{400, CodeInternal},
		{401, CodeUnauthenticated},
		{403, CodePermissionDenied},
		{404, CodeUnimplemented},
		{429, CodeResourceExhausted},
		{502, CodeUnavailable},
		{503, CodeUnavailable},
		{504, CodeUnavailable},
		{500, CodeUnknown},
		{405, CodeUnknown},
		{418, CodeUnknown},
		{302, CodeUnknown},
		{413, CodeUnknown},
	} {
		if got := StatusForHTTP(c.http); got != c.want {
			t.Errorf("StatusForHTTP(%d) = %v, want %v", c.http, got, c.want)
		}
	}
	if int(CodeResourceExhausted) != 8 || int(CodeUnavailable) != 14 || int(CodeDeadlineExceeded) != 4 || int(CodeInternal) != 13 {
		t.Error("code numbers")
	}
	if CodeUnavailable.String() != "UNAVAILABLE" || Code(99).String() != "CODE_99" {
		t.Errorf("names: %q %q", CodeUnavailable, Code(99))
	}
}

func TestMessageEncoding(t *testing.T) {
	for _, c := range []struct{ in, enc string }{
		{"", ""},
		{"plain text", "plain text"},
		{"100%", "100%25"},
		{"%41", "%2541"},
		{"a\nb", "a%0Ab"},
		{"grüße", "gr%C3%BC%C3%9Fe"},
		{"日本", "%E6%97%A5%E6%9C%AC"},
		{"😀", "%F0%9F%98%80"},
		{"\x7f~", "%7F~"},
	} {
		if got := EncodeMessage(c.in); got != c.enc {
			t.Errorf("EncodeMessage(%q) = %q, want %q", c.in, got, c.enc)
		}
		if got := DecodeMessage(c.enc); got != c.in {
			t.Errorf("DecodeMessage(%q) = %q, want %q", c.enc, got, c.in)
		}
	}
	// Round trip, including bytes that are not valid UTF-8.
	for _, s := range []string{"a%b%%25", "über 50% voll", "line1\r\nline2\ttab", "\xff\xfe", strings.Repeat("é%", 100), "😀 %F0 done"} {
		if got := DecodeMessage(EncodeMessage(s)); got != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
		for _, r := range EncodeMessage(s) {
			if r < 0x20 || r > 0x7e {
				t.Errorf("%q encodes to a non-printable %q", s, r)
			}
		}
	}
	// A decoder keeps what is not a valid escape.
	for in, want := range map[string]string{"50%": "50%", "%zz": "%zz", "%4": "%4", "%41": "A", "a%2": "a%2"} {
		if got := DecodeMessage(in); got != want {
			t.Errorf("DecodeMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteTrailersOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Retry-After", "7")
	WriteTrailersOnly(rec, "", CodeResourceExhausted, "slow down: 100% über")
	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if res.Header.Get("Content-Type") != "application/grpc" {
		t.Errorf("content-type %q", res.Header.Get("Content-Type"))
	}
	if res.Header.Get("Grpc-Status") != "8" {
		t.Errorf("grpc-status %q", res.Header.Get("Grpc-Status"))
	}
	if got := res.Header.Get("Grpc-Message"); got != "slow down: 100%25 %C3%BCber" {
		t.Errorf("grpc-message %q", got)
	}
	if res.Header.Get("Retry-After") != "7" {
		t.Error("Retry-After dropped")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body %q", rec.Body.String())
	}
	if len(res.Trailer) != 0 {
		t.Errorf("trailers %v", res.Trailer)
	}

	// The content type is echoed; stale body headers are removed; no message
	// means no grpc-message.
	rec = httptest.NewRecorder()
	rec.Header().Set("Content-Length", "12")
	rec.Header().Set("Content-Encoding", "gzip")
	rec.Header().Set("Grpc-Message", "old")
	WriteTrailersOnly(rec, "application/grpc+proto", CodeUnavailable, "")
	res = rec.Result()
	if res.Header.Get("Content-Type") != "application/grpc+proto" || res.Header.Get("Content-Length") != "" || res.Header.Get("Content-Encoding") != "" {
		t.Errorf("headers %v", res.Header)
	}
	if _, ok := res.Header["Grpc-Message"]; ok {
		t.Error("stale grpc-message kept")
	}
}

func TestStatus(t *testing.T) {
	for _, c := range []struct {
		name string
		h    http.Header
		want Code
		ok   bool
	}{
		{"none", http.Header{}, 0, false},
		{"header", http.Header{"Grpc-Status": {"14"}}, CodeUnavailable, true},
		{"ok", http.Header{"Grpc-Status": {"0"}}, CodeOK, true},
		{"prefixed", http.Header{http.TrailerPrefix + "Grpc-Status": {"4"}}, CodeDeadlineExceeded, true},
		{"spaces", http.Header{"Grpc-Status": {" 13 "}}, CodeInternal, true},
		{"unknown number", http.Header{"Grpc-Status": {"99"}}, CodeUnknown, true},
		{"not a number", http.Header{"Grpc-Status": {"abc"}}, 0, false},
		{"negative", http.Header{"Grpc-Status": {"-1"}}, 0, false},
		{"empty", http.Header{"Grpc-Status": {""}}, 0, false},
		{"huge", http.Header{"Grpc-Status": {"99999999999999999999"}}, 0, false},
	} {
		got, ok := Status(c.h)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: Status = %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
	// A real handler: announced trailer and TrailerPrefix trailer.
	for name, h := range map[string]http.HandlerFunc{
		"announced": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Trailer", "Grpc-Status")
			w.WriteHeader(200)
			w.Header().Set("Grpc-Status", "5")
		},
		"prefix": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			w.Header().Set(http.TrailerPrefix+"Grpc-Status", "5")
		},
		"trailers-only": func(w http.ResponseWriter, _ *http.Request) {
			WriteTrailersOnly(w, "", CodeNotFound, "x")
		},
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", "/", nil))
		resp := rec.Result()
		code, ok := ResponseStatus(resp.Header, resp.Trailer)
		if !ok || code != CodeNotFound {
			t.Errorf("%s: ResponseStatus = %v, %v", name, code, ok)
		}
	}
}

func TestResponseStatus_Order(t *testing.T) {
	// Headers (trailers-only) win over trailers.
	h := http.Header{"Grpc-Status": {"3"}}
	tr := http.Header{"Grpc-Status": {"5"}}
	if c, ok := ResponseStatus(h, tr); !ok || c != CodeInvalidArgument {
		t.Errorf("got %v, %v", c, ok)
	}
	if c, ok := ResponseStatus(http.Header{}, tr); !ok || c != CodeNotFound {
		t.Errorf("got %v, %v", c, ok)
	}
	if _, ok := ResponseStatus(nil, nil); ok {
		t.Error("nil maps")
	}
	// A garbage header does not hide a valid trailer.
	if c, ok := ResponseStatus(http.Header{"Grpc-Status": {"x"}}, tr); !ok || c != CodeNotFound {
		t.Errorf("got %v, %v", c, ok)
	}
}

func TestMessage(t *testing.T) {
	h := http.Header{"Grpc-Message": {"caf%C3%A9 100%25"}}
	if got := Message(h); got != "café 100%" {
		t.Errorf("Message = %q", got)
	}
	h = http.Header{http.TrailerPrefix + "Grpc-Message": {"x%20y"}}
	if got := Message(h); got != "x y" {
		t.Errorf("prefixed Message = %q", got)
	}
	if Message(nil) != "" {
		t.Error("nil")
	}
}

func FuzzCodecs(f *testing.F) {
	f.Add("1S", "a%b")
	f.Fuzz(func(t *testing.T, to, msg string) {
		if d, err := ParseTimeout(to); err == nil && d < 0 {
			t.Fatalf("negative %v", d)
		}
		if DecodeMessage(EncodeMessage(msg)) != msg {
			t.Fatalf("round trip %q", msg)
		}
		_ = DecodeMessage(msg)
	})
}
