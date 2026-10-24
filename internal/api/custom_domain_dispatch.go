package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
)

// CustomDomainSwitch is the host dispatch of the dashboard listener (H10):
// a request whose Host is a bound custom domain goes to site (the
// host-routing proxy), every other one to router (dashboard and API).
//
// Precedence:
//  1. A name the relay owns (owns: the auth domain and every name under
//     it, the services domain, the ACME names, the names of its
//     certificate, localhost, IPs) and an empty Host always go to router,
//     without a lookup, even when an old custom-domain row (from before the
//     H12 checks) has the name.
//  2. A bound custom domain goes to site.
//  3. Anything else, and a failed lookup (logged), goes to router.
//
// The decision is made on Host, not on the TLS server name, for three
// reasons. Behind a proxy that terminates TLS (Traefik) there is no server
// name, only Host. Over HTTP/2 a browser may send requests for several
// names on one connection whose certificate covers them (connection
// coalescing), so Host is the name the request is for. And a Host that
// does not match the server name gains a client nothing: site applies the
// service's own access policy to whoever names its custom domain, as it
// does on the host-routed ingress, which has always dispatched by Host;
// and the relay's own names win at step 1, so no custom domain can take
// the dashboard over.
func CustomDomainSwitch(owns func(string) bool, bound func(ctx context.Context, host string) (bool, error), site, router http.Handler, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := canonicalHost(r.Host)
		if host == "" || owns(host) {
			router.ServeHTTP(w, r)
			return
		}
		ok, err := bound(r.Context(), host)
		if err != nil {
			log.Warn("custom domain lookup failed; serving the dashboard", "host", host, "err", err)
		}
		if ok && err == nil {
			// site routes on Host too: hand it the name as it was looked
			// up (lower case, no trailing dot), the port kept.
			r2 := new(http.Request)
			*r2 = *r
			r2.Host = host
			if _, port, perr := net.SplitHostPort(r.Host); perr == nil && port != "" {
				r2.Host = net.JoinHostPort(host, port)
			}
			site.ServeHTTP(w, r2)
			return
		}
		router.ServeHTTP(w, r)
	})
}
