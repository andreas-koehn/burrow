package api

// The http settings of a service and the ingress status through the API
// (HTTP/2, gRPC and SNI on 443, H13): the upstream protocol and its TLS
// settings are read and changed on one route; the CA is write only; the
// client's own protocol and the effective one are read only.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// liveUpstream is the relay as the http settings route reads the protocol a
// client named at registration.
type liveUpstream struct {
	liveTCP
	clientUpstream map[string]string
}

func (l *liveUpstream) ClientUpstreamProtocol(serviceID string) string {
	return l.clientUpstream[serviceID]
}

func decodeHTTPSettings(t *testing.T, r *http.Response) httpSettingsResp {
	t.Helper()
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	var s httpSettingsResp
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHTTPSettings_GetAndPut(t *testing.T) {
	live := &liveUpstream{clientUpstream: map[string]string{}}
	a := newTCPAPI(t, func(d *Deps) { d.Connections = live })
	a.svcs.httpSettings = store.HTTPSettings{UpstreamProtocol: "http1"}

	got := decodeHTTPSettings(t, a.c.get(t, "/api/v1/services/web/http-settings"))
	if got.UpstreamProtocol != "http1" || got.ClientUpstreamProtocol != "" || got.EffectiveUpstreamProtocol != "http1" {
		t.Fatalf("GET %+v", got)
	}
	// The CA is never in the read, only its fingerprint.
	body := readBody(t, a.c.get(t, "/api/v1/services/web/http-settings"))
	for _, k := range []string{"upstream_tls_verify", "upstream_tls_server_name", "upstream_tls_ca_sha256", "grpc_max_duration_s", "client_upstream_protocol", "effective_upstream_protocol"} {
		if !strings.Contains(body, `"`+k+`"`) {
			t.Fatalf("no %s in %s", k, body)
		}
	}
	if strings.Contains(body, "upstream_tls_ca_pem") {
		t.Fatalf("the CA is read back: %s", body)
	}

	// One field alone keeps the others; the answer is the settings after.
	got = decodeHTTPSettings(t, a.c.put(t, "/api/v1/services/web/http-settings", map[string]any{"upstream_protocol": "h2c"}))
	u := a.svcs.lastHTTPUpdate
	if got.UpstreamProtocol != "h2c" || got.EffectiveUpstreamProtocol != "h2c" ||
		u.UpstreamProtocol == nil || *u.UpstreamProtocol != "h2c" || u.UpstreamTLSVerify != nil || u.UpstreamTLSCAPEM != nil || u.GRPCMaxDurationS != nil {
		t.Fatalf("PUT %+v update %+v", got, u)
	}
	got = decodeHTTPSettings(t, a.c.put(t, "/api/v1/services/web/http-settings", map[string]any{
		"upstream_protocol": "h2", "upstream_tls_verify": true, "upstream_tls_server_name": "app.internal",
		"upstream_tls_ca_pem": "PEM", "grpc_max_duration_s": 600,
	}))
	if got.UpstreamProtocol != "h2" || !got.UpstreamTLSVerify || got.UpstreamTLSServerName != "app.internal" ||
		got.UpstreamTLSCASHA256 == "" || got.GRPCMaxDurationS != 600 {
		t.Fatalf("PUT all %+v", got)
	}
	if u := a.svcs.lastHTTPUpdate; u.UpstreamTLSCAPEM == nil || *u.UpstreamTLSCAPEM != "PEM" {
		t.Fatalf("the CA did not reach the store: %+v", u)
	}

	// A client that named a protocol wins over the stored one.
	live.clientUpstream["web"] = "https"
	got = decodeHTTPSettings(t, a.c.get(t, "/api/v1/services/web/http-settings"))
	if got.UpstreamProtocol != "h2" || got.ClientUpstreamProtocol != "https" || got.EffectiveUpstreamProtocol != "https" {
		t.Fatalf("client protocol: %+v", got)
	}

	for _, c := range []struct {
		name string
		body string
		err  error
		want int
		text string
	}{
		{"nothing to change", `{}`, nil, http.StatusBadRequest, "at least one"},
		{"a field it does not know", `{"client_upstream_protocol": "h2"}`, nil, http.StatusBadRequest, "unknown"},
		{"not json", `nope`, nil, http.StatusBadRequest, ""},
		{"unknown protocol", `{"upstream_protocol": "h3"}`, store.ErrInvalidUpstreamProtocol, http.StatusBadRequest, "http1, https, h2c or h2"},
		{"duration", `{"grpc_max_duration_s": 5000000}`, store.ErrInvalidGRPCMaxDuration, http.StatusBadRequest, "1 to 86400"},
		{"ca", `{"upstream_tls_ca_pem": "x"}`, store.ErrInvalidUpstreamCA, http.StatusBadRequest, "CERTIFICATE"},
		{"server name", `{"upstream_tls_server_name": "1.2.3.4"}`, store.ErrInvalidUpstreamTLSServerName, http.StatusBadRequest, "DNS name"},
		{"tcp service", `{"upstream_protocol": "h2"}`, store.ErrServiceNotHTTPSettings, http.StatusConflict, "http services only"},
		{"another user's", `{"upstream_protocol": "h2"}`, store.ErrForbidden, http.StatusForbidden, ""},
		{"missing", `{"upstream_protocol": "h2"}`, db.ErrNotFound, http.StatusNotFound, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a.svcs.httpSettingsErr = c.err
			defer func() { a.svcs.httpSettingsErr = nil }()
			r := a.c.putRaw(t, "/api/v1/services/web/http-settings", c.body)
			body := readBody(t, r)
			if r.StatusCode != c.want || !strings.Contains(body, c.text) {
				t.Fatalf("status %d %s, want %d with %q", r.StatusCode, body, c.want, c.text)
			}
		})
	}
	a.svcs.httpSettingsErr = store.ErrServiceNotHTTPSettings
	if r := a.c.get(t, "/api/v1/services/rdp/http-settings"); r.StatusCode != http.StatusConflict {
		t.Fatalf("GET tcp: %d", r.StatusCode)
	}
}

// GET /ingress/status reports the env-only ingress switches.
func TestIngressStatus(t *testing.T) {
	a := newTCPAPI(t, func(d *Deps) {
		d.Ingress = IngressInfo{H2C: true, HTTPSPassthrough: true, PassthroughListen: ":8443", ACME: true}
	})
	r := a.c.get(t, "/api/v1/ingress/status")
	body := readBody(t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", r.StatusCode, body)
	}
	if body = strings.ReplaceAll(strings.TrimSpace(body), " ", ""); body != `{"h2c":true,"https_passthrough":true,"passthrough_listen":":8443","acme":true}` {
		t.Fatalf("body %s", body)
	}
	b := newTCPAPI(t, nil)
	if body := strings.ReplaceAll(strings.TrimSpace(readBody(t, b.c.get(t, "/api/v1/ingress/status"))), " ", ""); body != `{"h2c":false,"https_passthrough":false,"passthrough_listen":"","acme":false}` {
		t.Fatalf("zero value: %s", body)
	}
}
