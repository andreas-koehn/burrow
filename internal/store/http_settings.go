package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// The upstream protocols of an http service (migration 0045): how the relay
// speaks to the local service over the tunnel.
const (
	UpstreamHTTP1 = "http1" // HTTP/1.1, plain (the default)
	UpstreamHTTPS = "https" // HTTP/1.1 over TLS
	UpstreamH2C   = "h2c"   // HTTP/2 prior knowledge, plain
	UpstreamH2    = "h2"    // HTTP/2 over TLS
)

// The grpc_max_duration_s of a service: 0 (no cap) or MinGRPCMaxDurationS to
// MaxGRPCMaxDurationS seconds (1 s to 24 h).
const (
	MinGRPCMaxDurationS = 1
	MaxGRPCMaxDurationS = 86400
)

// MaxUpstreamCAPEMBytes bounds upstream_tls_ca_pem.
const MaxUpstreamCAPEMBytes = 64 << 10

// ValidUpstreamProtocol reports whether v is one of http1, https, h2c, h2.
func ValidUpstreamProtocol(v string) bool {
	switch v {
	case UpstreamHTTP1, UpstreamHTTPS, UpstreamH2C, UpstreamH2:
		return true
	}
	return false
}

// UpstreamUsesTLS reports whether the upstream protocol v is spoken over
// TLS (https and h2): the protocols that upstream_tls_* applies to.
func UpstreamUsesTLS(v string) bool { return v == UpstreamHTTPS || v == UpstreamH2 }

// ValidCertSource reports whether v is a custom-domain cert_source:
// db.CertSourceUploaded or db.CertSourceAuto.
func ValidCertSource(v string) bool {
	return v == db.CertSourceUploaded || v == db.CertSourceAuto
}

var (
	// ErrServiceNotHTTPSettings: the http settings apply to http services
	// only (maps to HTTP 409).
	ErrServiceNotHTTPSettings = errors.New("store: the setting applies to http services only")
	// ErrInvalidUpstreamProtocol: upstream_protocol is not http1, https,
	// h2c or h2.
	ErrInvalidUpstreamProtocol = errors.New("store: upstream_protocol must be http1, https, h2c or h2")
	// ErrInvalidGRPCMaxDuration: grpc_max_duration_s is not 0 or 1 to 86400.
	ErrInvalidGRPCMaxDuration = errors.New("store: grpc_max_duration_s must be 0 (off) or 1 to 86400")
	// ErrInvalidUpstreamCA: upstream_tls_ca_pem holds no parseable
	// CERTIFICATE block, or is over MaxUpstreamCAPEMBytes.
	ErrInvalidUpstreamCA = errors.New("store: upstream_tls_ca_pem must be PEM with a CERTIFICATE block, at most 64 KiB")
	// ErrInvalidUpstreamTLSServerName: upstream_tls_server_name is no DNS
	// name (an IP address is none).
	ErrInvalidUpstreamTLSServerName = errors.New("store: upstream_tls_server_name must be a DNS name")
)

// HTTPSettings are the per-service settings of an http service that shape
// its upstream. The CA is never read back; UpstreamTLSCASHA256 is the
// SHA-256 (hex) of its PEM, "" for none.
type HTTPSettings struct {
	UpstreamProtocol      string `json:"upstream_protocol"`
	UpstreamTLSVerify     bool   `json:"upstream_tls_verify"`
	UpstreamTLSServerName string `json:"upstream_tls_server_name"`
	UpstreamTLSCASHA256   string `json:"upstream_tls_ca_sha256"`
	GRPCMaxDurationS      int    `json:"grpc_max_duration_s"`
}

// HTTPSettingsUpdate names the settings to change; a nil field is kept.
// "" clears UpstreamTLSServerName and UpstreamTLSCAPEM.
type HTTPSettingsUpdate = db.HTTPSettingsUpdate

func httpSettingsOf(svc db.Service) HTTPSettings {
	h := HTTPSettings{
		UpstreamProtocol:      svc.UpstreamProtocol,
		UpstreamTLSVerify:     svc.UpstreamTLSVerify,
		UpstreamTLSServerName: svc.UpstreamTLSServerName,
		GRPCMaxDurationS:      svc.GRPCMaxDurationS,
	}
	if svc.UpstreamTLSCAPEM != "" {
		sum := sha256.Sum256([]byte(svc.UpstreamTLSCAPEM))
		h.UpstreamTLSCASHA256 = hex.EncodeToString(sum[:])
	}
	return h
}

// ServiceHTTPSettings reads the http settings of a service. Same permission
// rule as SetServiceMaxConnections; ErrServiceNotHTTPSettings for any other
// type than http.
func (s *Store) ServiceHTTPSettings(ctx context.Context, callerID, callerRole, serviceID string) (HTTPSettings, error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return HTTPSettings{}, err
	}
	if svc.Type != "http" {
		return HTTPSettings{}, ErrServiceNotHTTPSettings
	}
	return httpSettingsOf(svc), nil
}

// SetServiceHTTPSettings changes the http settings named in u and keeps the
// others. Same permission rule as SetServiceMaxConnections.
// ErrServiceNotHTTPSettings for any other type than http, and
// ErrInvalidUpstreamProtocol, ErrInvalidGRPCMaxDuration,
// ErrInvalidUpstreamCA or ErrInvalidUpstreamTLSServerName for a bad value
// (nothing is written then). The server name is stored lower case. It
// returns both states for the caller's audit row (the CA only as its
// fingerprint). It audits the write as service.http_settings.update.
func (s *Store) SetServiceHTTPSettings(ctx context.Context, callerID, callerRole, serviceID string, u HTTPSettingsUpdate) (before, after HTTPSettings, err error) {
	svc, err := s.canConfigure(ctx, callerID, callerRole, serviceID)
	if err != nil {
		return HTTPSettings{}, HTTPSettings{}, err
	}
	if svc.Type != "http" {
		return HTTPSettings{}, HTTPSettings{}, ErrServiceNotHTTPSettings
	}
	if p := u.UpstreamProtocol; p != nil && !ValidUpstreamProtocol(*p) {
		return HTTPSettings{}, HTTPSettings{}, ErrInvalidUpstreamProtocol
	}
	if n := u.GRPCMaxDurationS; n != nil && *n != 0 && (*n < MinGRPCMaxDurationS || *n > MaxGRPCMaxDurationS) {
		return HTTPSettings{}, HTTPSettings{}, ErrInvalidGRPCMaxDuration
	}
	if ca := u.UpstreamTLSCAPEM; ca != nil && *ca != "" && (len(*ca) > MaxUpstreamCAPEMBytes || !validateCAPEM([]byte(*ca))) {
		return HTTPSettings{}, HTTPSettings{}, ErrInvalidUpstreamCA
	}
	if n := u.UpstreamTLSServerName; n != nil && *n != "" {
		name := strings.ToLower(strings.TrimSpace(*n))
		if !validUpstreamServerName(name) {
			return HTTPSettings{}, HTTPSettings{}, ErrInvalidUpstreamTLSServerName
		}
		u.UpstreamTLSServerName = &name
	}
	before = httpSettingsOf(svc)
	if err := s.q.SetServiceHTTPSettings(ctx, serviceID, u); err != nil {
		return HTTPSettings{}, HTTPSettings{}, err
	}
	now, err := s.q.GetServiceByID(ctx, serviceID)
	if err != nil {
		return HTTPSettings{}, HTTPSettings{}, err
	}
	after = httpSettingsOf(now)
	s.emitAudit(withAuditDeadline(ctx), audit.ActionServiceHTTPSettingsUpdate, func(e *audit.Event) {
		e.SubjectID = serviceID
		e.SubjectLabel = svc.Name
		e.Payload = audit.MustJSON(map[string]HTTPSettings{"from": before, "to": after})
	})
	return before, after, nil
}

// validUpstreamServerName reports whether name (lower case) is a DNS name
// of letters, digits, hyphens and dots, at most 253 bytes, that is no IP
// address.
func validUpstreamServerName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
