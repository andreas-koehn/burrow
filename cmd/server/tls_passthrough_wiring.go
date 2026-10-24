package main

// tls_passthrough_wiring.go — what the relay's TLS passthrough listener
// (tcp complete, E1) needs from the configuration.

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/relaynames"
	"github.com/ankoehn/burrow/internal/server"
)

// relayNameSet is the set of names the relay serves itself on its HTTPS
// port, built once for the mux (server.Options.RelayNames) and the API's
// tcp_hostname check (api.Deps.RelayNames): the dashboard's host and every
// name under it, the services host, the ACME names, and in file-certificate
// mode the DNS names of BURROW_HTTP_TLS_CERT (a wildcard over one label).
// The certificate's names count even without BURROW_AUTH_DOMAIN: they are
// the dashboard's host there.
func relayNameSet(cfg *config.ServerConfig, dashboardHost string, acmeNames []string) (*relaynames.Set, error) {
	names := append([]string{cfg.ServicesDomain}, acmeNames...)
	if cfg.HTTPTLSCert != "" && cfg.HTTPTLSKey != "" {
		sans, err := relaynames.CertDNSNames(cfg.HTTPTLSCert)
		if err != nil {
			return nil, fmt.Errorf("read the names of BURROW_HTTP_TLS_CERT: %w", err)
		}
		names = append(names, sans...)
	}
	return relaynames.New([]string{dashboardHost}, names...), nil
}

// passthroughOn reports whether the relay reaches tcp services by host name:
// the dedicated passthrough listener (BURROW_TLS_PASSTHROUGH_LISTEN) or the
// mux on the HTTPS port (BURROW_TLS_PASSTHROUGH_ON_HTTPS) runs. Host name
// only and the API's "reachable" go by it.
func passthroughOn(cfg *config.ServerConfig) bool {
	return cfg.TLSPassthroughListen != "" || cfg.TLSPassthroughOnHTTPS
}

// trustedPrefixes reads BURROW_TLS_PASSTHROUGH_TRUSTED_PROXIES (addresses and CIDRs, already
// validated by the config) as the prefixes the relay's tcp listeners believe
// a PROXY protocol header from. An entry that cannot be read is left out:
// it trusts nobody.
func trustedPrefixes(entries []string) []netip.Prefix {
	var out []netip.Prefix
	for _, e := range entries {
		if p, err := server.ParseTCPPrefix(strings.TrimSpace(e)); err == nil {
			out = append(out, p)
		}
	}
	return out
}
