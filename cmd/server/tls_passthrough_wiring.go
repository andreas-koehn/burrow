package main

// tls_passthrough_wiring.go — what the relay's TLS passthrough listener
// (tcp complete, E1) needs from the configuration.

import (
	"net/netip"
	"strings"

	"github.com/ankoehn/burrow/internal/server"
)

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
