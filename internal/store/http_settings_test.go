package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestValidUpstreamProtocol(t *testing.T) {
	for _, v := range []string{"http1", "https", "h2c", "h2"} {
		if !ValidUpstreamProtocol(v) {
			t.Errorf("%q must be valid", v)
		}
	}
	for _, v := range []string{"", "http", "HTTP1", "h3", "http2", " h2"} {
		if ValidUpstreamProtocol(v) {
			t.Errorf("%q must be invalid", v)
		}
	}
	if !ValidCertSource("uploaded") || !ValidCertSource("auto") || ValidCertSource("") || ValidCertSource("acme") {
		t.Error("cert source values")
	}
}

// The http settings of a service: defaults, partial updates, bounds, the
// CA fingerprint on read, and the permission rule.
func TestServiceHTTPSettings(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	owner := mustCreateUser(t, st, "owner@x", "user")
	other := mustCreateUser(t, st, "other@x", "user")
	web := mustGetOrCreateService(t, st, owner.ID, "web", "http")
	rdp := mustGetOrCreateService(t, st, owner.ID, "rdp", "tcp")

	got, err := st.ServiceHTTPSettings(ctx, owner.ID, "user", web.ID)
	if err != nil || got != (HTTPSettings{UpstreamProtocol: "http1"}) {
		t.Fatalf("new service: %+v %v", got, err)
	}
	ca := string(genTestCAPEM(t))
	sum := sha256.Sum256([]byte(ca))
	before, after, err := st.SetServiceHTTPSettings(ctx, owner.ID, "user", web.ID, HTTPSettingsUpdate{
		UpstreamProtocol: ptrTo("h2"), UpstreamTLSVerify: ptrTo(true), UpstreamTLSServerName: ptrTo("App.Internal"),
		UpstreamTLSCAPEM: ptrTo(ca), GRPCMaxDurationS: ptrTo(3600),
	})
	want := HTTPSettings{UpstreamProtocol: "h2", UpstreamTLSVerify: true, UpstreamTLSServerName: "app.internal",
		UpstreamTLSCASHA256: hex.EncodeToString(sum[:]), GRPCMaxDurationS: 3600}
	if err != nil || before != (HTTPSettings{UpstreamProtocol: "http1"}) || after != want {
		t.Fatalf("set: %+v %+v %v", before, after, err)
	}
	// The PEM itself is for the proxy: the raw service row has it.
	if svc, _ := st.q.GetServiceByID(ctx, web.ID); svc.UpstreamTLSCAPEM != ca {
		t.Fatalf("stored PEM %q", svc.UpstreamTLSCAPEM)
	}
	// One field alone keeps the others.
	if _, after, err := st.SetServiceHTTPSettings(ctx, owner.ID, "user", web.ID, HTTPSettingsUpdate{UpstreamProtocol: ptrTo("h2c")}); err != nil ||
		after.UpstreamProtocol != "h2c" || !after.UpstreamTLSVerify || after.GRPCMaxDurationS != 3600 || after.UpstreamTLSCASHA256 == "" {
		t.Fatalf("protocol alone: %+v %v", after, err)
	}
	// "" clears the CA and the name.
	if _, after, err := st.SetServiceHTTPSettings(ctx, owner.ID, "user", web.ID, HTTPSettingsUpdate{UpstreamTLSCAPEM: ptrTo(""), UpstreamTLSServerName: ptrTo("")}); err != nil ||
		after.UpstreamTLSCASHA256 != "" || after.UpstreamTLSServerName != "" {
		t.Fatalf("clear: %+v %v", after, err)
	}
	// Invalid values write nothing.
	bad := []struct {
		u   HTTPSettingsUpdate
		err error
	}{
		{HTTPSettingsUpdate{UpstreamProtocol: ptrTo("h3")}, ErrInvalidUpstreamProtocol},
		{HTTPSettingsUpdate{UpstreamProtocol: ptrTo("")}, ErrInvalidUpstreamProtocol},
		{HTTPSettingsUpdate{GRPCMaxDurationS: ptrTo(-1)}, ErrInvalidGRPCMaxDuration},
		{HTTPSettingsUpdate{GRPCMaxDurationS: ptrTo(86401)}, ErrInvalidGRPCMaxDuration},
		{HTTPSettingsUpdate{UpstreamTLSCAPEM: ptrTo("not a pem")}, ErrInvalidUpstreamCA},
		{HTTPSettingsUpdate{UpstreamTLSServerName: ptrTo("bad name!")}, ErrInvalidUpstreamTLSServerName},
		{HTTPSettingsUpdate{UpstreamTLSServerName: ptrTo("10.0.0.1")}, ErrInvalidUpstreamTLSServerName},
	}
	for i, c := range bad {
		if _, _, err := st.SetServiceHTTPSettings(ctx, owner.ID, "user", web.ID, c.u); !errors.Is(err, c.err) {
			t.Fatalf("bad %d: %v; want %v", i, err, c.err)
		}
	}
	for _, n := range []int{0, 1, 86400} {
		if _, _, err := st.SetServiceHTTPSettings(ctx, owner.ID, "user", web.ID, HTTPSettingsUpdate{GRPCMaxDurationS: ptrTo(n)}); err != nil {
			t.Fatalf("max %d: %v", n, err)
		}
	}
	if got, _ := st.ServiceHTTPSettings(ctx, owner.ID, "user", web.ID); got.UpstreamProtocol != "h2c" {
		t.Fatalf("a refused update wrote: %+v", got)
	}
	// Permissions and types.
	if _, err := st.ServiceHTTPSettings(ctx, other.ID, "user", web.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other user read: %v", err)
	}
	if _, _, err := st.SetServiceHTTPSettings(ctx, other.ID, "user", web.ID, HTTPSettingsUpdate{UpstreamProtocol: ptrTo("h2")}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other user write: %v", err)
	}
	if _, _, err := st.SetServiceHTTPSettings(ctx, other.ID, "admin", web.ID, HTTPSettingsUpdate{UpstreamProtocol: ptrTo("h2")}); err != nil {
		t.Fatalf("admin write: %v", err)
	}
	if _, err := st.ServiceHTTPSettings(ctx, owner.ID, "user", rdp.ID); !errors.Is(err, ErrServiceNotHTTPSettings) {
		t.Fatalf("tcp read: %v", err)
	}
	if _, _, err := st.SetServiceHTTPSettings(ctx, owner.ID, "user", rdp.ID, HTTPSettingsUpdate{UpstreamProtocol: ptrTo("h2")}); !errors.Is(err, ErrServiceNotHTTPSettings) {
		t.Fatalf("tcp write: %v", err)
	}
}
