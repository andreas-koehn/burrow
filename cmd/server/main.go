// Command burrowd is the Burrow relay server.
//
// `serve` runs the control server: it opens/migrates the SQLite database,
// seeds the first admin from config, authenticates clients against
// DB-issued tokens, and persists registered tunnels. It ALSO serves the
// embedded dashboard SPA at / (the web UI) alongside the HTTP JSON API +
// SSE on BURROW_HTTP_LISTEN (default :8080), beside the control listener.
//
// `token` is an operator/dev helper that mints a client token for an
// existing user directly against the database (no HTTP API needed yet).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/acme"
	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aiprovider"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/cost"
	"github.com/ankoehn/burrow/internal/credinject"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/events"
	"github.com/ankoehn/burrow/internal/logging"
	"github.com/ankoehn/burrow/internal/mcpgw"
	"github.com/ankoehn/burrow/internal/oauth"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/proxy/customdomain"
	"github.com/ankoehn/burrow/internal/retention"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
	"github.com/ankoehn/burrow/internal/version"
	"github.com/ankoehn/burrow/pkg/clientip"
	"github.com/ankoehn/burrow/web"
)

// apiShutdownGrace is the timeout given to (*http.Server).Shutdown when the
// serve command receives a stop signal. It must be strictly greater than
// api.JSONHandlerTimeout (the chi middleware.Timeout applied to JSON routes)
// so that every in-flight handler has time to complete (or be chi-cancelled)
// before Shutdown returns and the deferred database.Close() runs. The five
// seconds on top also cover the audit rows a store mutation writes after its
// handler's deadline: they share one deadline of store.auditWriteTimeout (5s)
// per mutation, so a stuck audit log delays a handler by at most that.
const apiShutdownGrace = api.JSONHandlerTimeout + 5*time.Second

func versionLine() string {
	return fmt.Sprintf("burrowd %s (commit %s, built %s, %s/%s)",
		version.Version, version.Commit, version.Date, runtime.GOOS, runtime.GOARCH)
}

// tunnelStoreAdapter converts the server's *Tunnel to the store's persistence
// shape, so internal/store stays decoupled from internal/server (E8).
type tunnelStoreAdapter struct{ s *store.Store }

func (a tunnelStoreAdapter) SaveTunnel(ctx context.Context, userID string, t *server.Tunnel) error {
	return a.s.SaveTunnel(ctx, userID, &store.SaveTunnelArg{
		ID: t.ID, Name: t.Name, Type: t.Type, RemotePort: t.RemotePort, LocalAddr: t.LocalAddr,
	})
}

func (a tunnelStoreAdapter) MarkTunnelSeen(ctx context.Context, tunnelID string) error {
	return a.s.MarkTunnelSeen(ctx, tunnelID)
}

// userTunnelLister is the minimal slice of *server.Server that
// tunnelListerAdapter needs. Depending on this interface (rather than the
// concrete *server.Server, which it still satisfies) keeps the adapter's
// server.TunnelView -> api.TunnelView field mapping unit-testable in
// package main without driving a full TLS+yamux handshake to populate the
// server's unexported registry.
type userTunnelLister interface {
	ListUserTunnels(userID string) []server.TunnelView
}

// tunnelListerAdapter exposes the live server registry to the HTTP API,
// converting server.TunnelView to api.TunnelView (keeps internal/api
// decoupled from internal/server, same pattern as tunnelStoreAdapter).
//
// access enriches the access_mode field on http tunnels by looking up the
// persisted tunnel row (best-effort: a missing row leaves the field empty,
// so the dashboard falls back to its "Open" default — never blocks the
// hot read path on a store error).
//
// services, when set, overrides that with the durable services row — the
// mode users actually configure — so Tunnels and Services never disagree.
//
// slugs + authDomain compose the public URL of http tunnels from the durable
// service row, so a slug change shows up without a client reconnect.
type tunnelListerAdapter struct {
	s          userTunnelLister
	access     tunnelGetter      // optional; nil in unit tests
	services   serviceModeGetter // optional; nil in unit tests
	slugs      serviceGetter     // optional; nil in unit tests
	authDomain string
	// servicesDomain is BURROW_SERVICES_DOMAIN; when set, the URL is on it.
	servicesDomain string
}

func (a tunnelListerAdapter) ListUserTunnels(userID string) []api.TunnelView {
	var out []api.TunnelView
	for _, t := range a.s.ListUserTunnels(userID) {
		mode := t.AccessMode
		if mode == "" && a.access != nil && t.Type == "http" {
			if row, err := a.access.GetTunnel(context.Background(), t.ID); err == nil {
				mode = row.AccessMode
			}
		}
		mode = durableMode(a.services, t.ServiceID, mode)
		out = append(out, api.TunnelView{
			ID: t.ID, Name: t.Name, Type: t.Type, RemotePort: t.RemotePort,
			LocalAddr: t.LocalAddr, BytesIn: t.BytesIn, BytesOut: t.BytesOut, Connected: t.Connected,
			ServiceID:  t.ServiceID,
			URL:        a.tunnelURL(t.ServiceID),
			AccessMode: mode,
		})
	}
	return out
}

// tunnelURL returns "https://<host>/svc/<slug>/" for the durable service
// behind a tunnel, host being the services domain when set and the auth
// domain otherwise, or "" when there is no auth domain, no service row or no
// slug. A tcp service has a slug (the name a forward reaches it by), but no
// /svc/ address: it gets none either.
func (a tunnelListerAdapter) tunnelURL(serviceID string) string {
	if a.slugs == nil || a.authDomain == "" || serviceID == "" {
		return ""
	}
	svc, err := a.slugs.ServiceByID(context.Background(), serviceID)
	if err != nil || svc.Subdomain == "" || svc.GatewayOnly || svc.Type == "tcp" {
		return ""
	}
	host := a.authDomain
	if a.servicesDomain != "" {
		host = a.servicesDomain
	}
	return "https://" + host + "/svc/" + svc.Subdomain + "/"
}

// serviceGetter loads the durable service row for a service id.
// *store.Store satisfies it; nil in unit tests that don't exercise it.
type serviceGetter interface {
	ServiceByID(ctx context.Context, id string) (db.Service, error)
}

// sessionSnapshotter is the read-only slice of *server.Server the clients
// adapter needs (so the adapter stays unit-testable without a live registry).
type sessionSnapshotter interface {
	SnapshotSessions() []server.SessionSnapshot
}

// tunnelGetter is the read-only slice of *store.Store the clients adapter
// needs for persisted per-tunnel totals + access mode.
type tunnelGetter interface {
	GetTunnel(ctx context.Context, id string) (db.Tunnel, error)
}

// serviceModeGetter resolves the durable access mode for a service id.
// *store.Store satisfies it; nil in unit tests that don't exercise it.
type serviceModeGetter interface {
	ServiceAccessMode(ctx context.Context, serviceID string) (string, error)
}

// durableMode returns the service's durable access mode, or fallback when the
// tunnel has no durable service or the lookup fails (best-effort: the read
// path never fails because of it).
func durableMode(g serviceModeGetter, serviceID, fallback string) string {
	if g == nil || serviceID == "" {
		return fallback
	}
	if m, err := g.ServiceAccessMode(context.Background(), serviceID); err == nil && m != "" {
		return m
	}
	return fallback
}

// clientTokenLister lists the client tokens of every user with the last
// session of each. *store.Store satisfies it.
type clientTokenLister interface {
	ListAllClientTokens(ctx context.Context) ([]db.ClientToken, error)
}

// clientsAdapter exposes live sessions + persisted per-service totals to the
// HTTP API (keeps internal/api decoupled from internal/server and internal/db).
type clientsAdapter struct {
	srv      sessionSnapshotter
	st       tunnelGetter
	services serviceModeGetter // optional; nil in unit tests
	tokens   clientTokenLister // optional; nil in unit tests
}

func (a clientsAdapter) serviceViews(ss server.SessionSnapshot) ([]api.ClientServiceView, int64, int64) {
	var svcs []api.ClientServiceView
	var aggIn, aggOut int64
	for _, tn := range ss.Tunnels {
		var totIn, totOut int64
		mode := "open"
		if row, err := a.st.GetTunnel(context.Background(), tn.ID); err == nil {
			totIn, totOut, mode = row.TotalBytesIn, row.TotalBytesOut, row.AccessMode
		}
		mode = durableMode(a.services, tn.ServiceID, mode)
		aggIn += totIn
		aggOut += totOut
		svcs = append(svcs, api.ClientServiceView{
			ID: tn.ID, Name: tn.Name, Type: tn.Type, RemotePort: tn.RemotePort,
			ServiceID: tn.ServiceID,
			LocalAddr: tn.LocalAddr, AccessMode: mode,
			BytesIn: tn.BytesIn, BytesOut: tn.BytesOut,
			TotalBytesIn: totIn, TotalBytesOut: totOut,
		})
	}
	return svcs, aggIn, aggOut
}

func (a clientsAdapter) toView(ss server.SessionSnapshot, aggIn, aggOut int64, n int) api.ClientView {
	fw := make([]api.ClientForwardView, 0, len(ss.Forwards))
	for _, f := range ss.Forwards {
		fw = append(fw, api.ClientForwardView{
			ID: f.ID, ServiceID: f.ServiceID, Service: f.Service, Since: f.Since,
			BytesIn: f.BytesIn, BytesOut: f.BytesOut,
		})
	}
	slices.SortFunc(fw, func(x, y api.ClientForwardView) int { return x.Since.Compare(y.Since) })
	return api.ClientView{
		SessionID: ss.SessionID, UserID: ss.UserID, TokenName: ss.Token,
		RemoteAddr: ss.RemoteAddr, OS: ss.OS, Arch: ss.Arch,
		ClientVersion: ss.ClientVersion, ServiceCount: n,
		TotalBytesIn: aggIn, TotalBytesOut: aggOut,
		ForwardCount: len(fw), Forwards: fw,
	}
}

func (a clientsAdapter) ListClients() []api.ClientView {
	var out []api.ClientView
	for _, ss := range a.srv.SnapshotSessions() {
		_, in, outB := a.serviceViews(ss)
		out = append(out, a.toView(ss, in, outB, len(ss.Tunnels)))
	}
	return out
}

func (a clientsAdapter) GetClient(sessionID string) (api.ClientDetail, bool) {
	for _, ss := range a.srv.SnapshotSessions() {
		if ss.SessionID != sessionID {
			continue
		}
		svcs, in, outB := a.serviceViews(ss)
		return api.ClientDetail{
			ClientView: a.toView(ss, in, outB, len(ss.Tunnels)),
			Services:   svcs,
		}, true
	}
	return api.ClientDetail{}, false
}

// ListKnownClients joins every client token with the live sessions that
// logged in with it (api.KnownClients).
func (a clientsAdapter) ListKnownClients(ctx context.Context) ([]api.KnownClient, error) {
	// The sessions first: a client that connects in between is missing from
	// this answer, instead of showing up as a session without a token.
	var live []api.LiveClient
	for _, ss := range a.srv.SnapshotSessions() {
		_, in, outB := a.serviceViews(ss)
		lc := api.LiveClient{
			ClientView: a.toView(ss, in, outB, len(ss.Tunnels)),
			TokenID:    ss.TokenID, ConnectedAt: ss.ConnectedAt,
		}
		for _, tn := range ss.Tunnels {
			lc.Served = append(lc.Served, tn.Name)
		}
		live = append(live, lc)
	}
	var tokens []db.ClientToken
	if a.tokens != nil {
		var err error
		if tokens, err = a.tokens.ListAllClientTokens(ctx); err != nil {
			return nil, err
		}
	}
	return api.KnownClients(tokens, live), nil
}

// trafficFinalFlushTimeout bounds the traffic flusher's last flush, the one
// at shutdown.
const trafficFinalFlushTimeout = 5 * time.Second

// liveTunnelSource is the control server as the traffic flusher needs it
// (*server.Server), tunnelTotalsWriter the store (*store.Store). Interfaces
// make the flusher testable without either.
type liveTunnelSource interface {
	SnapshotSessions() []server.SessionSnapshot
}

type tunnelTotalsWriter interface {
	FlushTunnelTotals(ctx context.Context, id string, addIn, addOut int64) error
}

// runTrafficFlusher periodically persists the delta of each live tunnel's
// in-memory byte counters into tunnels.total_bytes_*, so traffic survives
// reconnects. WaitGroup-tracked + ctx-cancelled like runSessionReaper; a final
// flush runs on ctx cancellation before the DB is closed. That one cannot use
// ctx, which is over by then and would fail every write: it runs on a context
// detached from it, bounded by trafficFinalFlushTimeout as a whole.
func runTrafficFlusher(ctx context.Context, wg *sync.WaitGroup, srv liveTunnelSource, st tunnelTotalsWriter, log *slog.Logger, interval time.Duration) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		last := map[string][2]uint64{} // tunnelID -> {in,out} already persisted
		flush := func(ctx context.Context) {
			seen := map[string]struct{}{}
			for _, ss := range srv.SnapshotSessions() {
				for _, tn := range ss.Tunnels {
					seen[tn.ID] = struct{}{}
					prev := last[tn.ID]
					dIn := int64(tn.BytesIn) - int64(prev[0])
					dOut := int64(tn.BytesOut) - int64(prev[1])
					if dIn < 0 {
						dIn = int64(tn.BytesIn) // counter reset (reconnect): persist absolute
					}
					if dOut < 0 {
						dOut = int64(tn.BytesOut)
					}
					if dIn == 0 && dOut == 0 {
						continue
					}
					if err := st.FlushTunnelTotals(ctx, tn.ID, dIn, dOut); err != nil {
						log.Warn("traffic flush", "tunnel_id", tn.ID, "err", err)
						continue
					}
					last[tn.ID] = [2]uint64{tn.BytesIn, tn.BytesOut}
				}
			}
			for id := range last { // drop disconnected tunnels
				if _, ok := seen[id]; !ok {
					delete(last, id)
				}
			}
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				// final flush before shutdown / DB close
				fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trafficFinalFlushTimeout)
				flush(fctx)
				cancel()
				return
			case <-t.C:
				flush(ctx)
			}
		}
	}()
}

// sessionReaper is the type used to purge expired sessions and the OAuth
// tables. Using an interface makes the reaper testable without a real DB.
type sessionReaper interface {
	DeleteExpiredSessions(ctx context.Context) (int64, error)
	// PruneOAuth deletes expired OAuth codes and tokens and the grants
	// revoked more than 30 days ago.
	PruneOAuth(ctx context.Context, now time.Time) error
}

// runSessionReaper starts a goroutine that calls reaper.DeleteExpiredSessions
// and reaper.PruneOAuth immediately and then every interval. It mirrors the
// byteTicker pattern in internal/server: the goroutine is tracked on wg and
// exits when ctx is done. Callers must wg.Wait() before closing the database.
func runSessionReaper(ctx context.Context, wg *sync.WaitGroup, reaper sessionReaper, log *slog.Logger, interval time.Duration) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		purge := func() {
			n, err := reaper.DeleteExpiredSessions(ctx)
			if err != nil {
				log.Warn("session reaper", "err", err)
				return
			}
			if n > 0 {
				log.Info("session reaper: purged expired sessions", "count", n)
			}
		}
		prune := func() {
			if err := reaper.PruneOAuth(ctx, time.Now()); err != nil && ctx.Err() == nil {
				log.Warn("oauth prune", "err", err)
			}
		}
		purge() // run once at startup
		prune()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				purge()
				prune()
			}
		}
	}()
}

// expiredTokenLister lists the client tokens that have expired at a time.
type expiredTokenLister interface {
	ListExpiredClientTokenIDs(ctx context.Context, now time.Time) ([]string, error)
}

// runExpiredTokenSweeper ends the live sessions of client tokens that have
// expired, every interval until ctx is done: a login with an expired token is
// refused, but a session that was open when the expiry passed would otherwise
// run on. Closing the sessions of a token that has none is a no-op, so tokens
// stay in the list for as long as they exist.
func runExpiredTokenSweeper(ctx context.Context, wg *sync.WaitGroup, tokens expiredTokenLister, sessions interface{ CloseByToken(string) int }, log *slog.Logger, interval time.Duration) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		sweep := func() {
			ids, err := tokens.ListExpiredClientTokenIDs(ctx, time.Now())
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("expired token sweep", "err", err)
				}
				return
			}
			for _, id := range ids {
				if n := sessions.CloseByToken(id); n > 0 {
					log.Info("client sessions closed: token expired", "token_id", id, "sessions", n)
				}
			}
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweep()
			}
		}
	}()
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newRootCmd builds the burrowd command tree. It is separate from main so
// that a test can run `serve` with the real wiring and stop it through the
// command's context.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "burrowd",
		Short:         "Burrow relay server",
		Version:       versionLine(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the relay control server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			overrides := map[string]any{}
			if v, _ := cmd.Flags().GetString("listen"); v != "" {
				overrides["listen"] = v
			}
			if v, _ := cmd.Flags().GetString("tls-cert"); v != "" {
				overrides["tls_cert"] = v
			}
			if v, _ := cmd.Flags().GetString("tls-key"); v != "" {
				overrides["tls_key"] = v
			}
			cfg, err := config.LoadServer(overrides)
			if err != nil {
				return err
			}
			log := logging.New(cfg.LogLevel, cfg.LogFormat)
			if gen, _ := cmd.Flags().GetBool("dev-certs"); gen {
				if err := devcert.Generate("certs", false); err != nil {
					return err
				}
			}
			if isDev, reason := server.DevCertWarning(cfg.TLSCert); isDev {
				log.Warn("serving with a DEVELOPMENT self-signed TLS certificate — NOT for production; set BURROW_TLS_CERT/BURROW_TLS_KEY (or --tls-cert/--tls-key) to real certificates",
					"reason", reason, "cert", cfg.TLSCert)
			}
			// The separate MCP listener is gone. Its two variables are still
			// accepted, so a relay deployed with them keeps starting.
			warnRetiredMCPConfig(cfg, log)
			// v0.5.0 Task 15: branch on Postgres vs SQLite at startup.
			// openBackend (tag-gated in db_default.go / db_postgres.go) selects
			// the backend and runs migrations. The returned Backend is unwrapped
			// to *sql.DB for the rest of the existing code (which was written
			// before the Backend abstraction); new code should accept Backend.
			backend, err := openBackend(cfg, log)
			if err != nil {
				return err
			}
			database := backend.DB()
			defer backend.Close()
			// reaperWg tracks goroutines that touch the DB or publish webhooks
			// (session reaper, traffic flusher, retention compactor, custom-domain
			// status tick). Two defers exist:
			//   - This early one guards the error paths between here and the v04
			//     stack build (web.Handler / buildV04Stack failures), ensuring
			//     the session reaper + traffic flusher drain before backend.Close.
			//   - A second `defer reaperWg.Wait()` is registered AFTER
			//     `defer v04.WebhookDispatcher.Close()` (further down) so that
			//     any reaperWg-tracked goroutine mid-Publish at shutdown finishes
			//     before the dispatcher's queue channel is closed (otherwise the
			//     v0.5.2 custom-domain status tick would panic on send-to-closed-
			//     channel). Calling Wait on an already-drained WaitGroup is a
			//     no-op, so the duplicate is harmless on the happy path.
			var reaperWg sync.WaitGroup
			defer reaperWg.Wait()
			st := store.New(database)
			st.SetSMTPPassword(cfg.SMTPPassword)
			if err := st.SeedAdmin(context.Background(), cfg.AdminEmail, cfg.AdminPassword); err != nil {
				return err
			}
			n, backfillErr := st.BackfillAIProviders(context.Background())
			if backfillErr != nil {
				log.Warn("ai providers backfill failed", "err", backfillErr)
			} else if n > 0 {
				log.Info("ai providers created for existing api_key services", "count", n)
			}
			// The alias import runs once and targets providers: after a failed
			// backfill it waits for the next start.
			if backfillErr != nil {
				log.Warn("model alias import postponed: the ai providers backfill failed")
			} else if n, skipped, err := st.ImportModelAliases(context.Background()); err != nil {
				log.Warn("model alias import failed", "err", err)
			} else if n > 0 || len(skipped) > 0 {
				log.Info("model aliases imported as models", "imported", n, "skipped", len(skipped), "skipped_aliases", skipped)
			}

			bus := events.NewBus()

			// cmd.Context() is context.Background() when main runs the
			// command; a test passes one it can cancel.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// Built-in ACME (v0.6.0): when ACMEDomain is set, burrowd obtains
			// auto-renewing Let's Encrypt certificates and uses them for every
			// listener (dashboard, control channel, proxy ingress) plus a :80
			// HTTP-01 + redirect listener. acme.New blocks until certs are ready
			// (ManageSync) so listeners always start with a cert; an error here
			// is fatal. Built before server.New so the control channel can adopt
			// the ACME cert via Options.GetCertificate. When ACMEDomain == ""
			// acmeMgr stays nil and all behavior below is byte-for-byte unchanged.
			var acmeMgr *acme.Manager
			if cfg.ACMEDomain != "" {
				var aerr error
				acmeMgr, aerr = acme.New(ctx, acme.Config{
					Domains: acmeDomains(cfg.ACMEDomain, cfg.ServicesDomain),
					Email:   cfg.ACMEEmail,
					CA:      cfg.ACMECA,
					Storage: cfg.ACMEStorage,
					Log:     log,
				})
				if aerr != nil {
					log.Error("acme: fatal", "err", aerr)
					return aerr
				}
			}

			// proxyAuthDomain is the single resolved base domain used for ALL
			// host-routing surfaces — the proxy's subdomain matcher, the access
			// checker, the gate, AND the /svc/{slug} path route (api.Deps.AuthDomain
			// + ServicePathHandler). It prefers the explicit BURROW_AUTH_DOMAIN;
			// when that is empty but built-in ACME is on, it falls back to the
			// first ACME-managed domain so single-origin path routing works out
			// of the box on an ACME deployment without a separate auth_domain.
			// In the common case (auth_domain set) this equals cfg.AuthDomain, so
			// every consumer below is byte-for-byte unchanged. When it resolves
			// to "" (no auth_domain, ACME off) the router does NOT register the
			// /svc/{slug} routes (see api.NewRouter) — matching the prior behavior
			// where subdomain routing is disabled.
			//
			// Keeping a single value here guarantees the path handler's
			// synthesized host "<id>.<proxyAuthDomain>" always matches the
			// suffix the proxy routes on — otherwise the route would register but
			// 404 at the proxy.
			//
			// Resolved before server.New: the control server tells clients the
			// public URL of their tunnels and must use the same domain.
			proxyAuthDomain := resolveAuthDomain(cfg.AuthDomain, cfg.ACMEDomain, acmeMgr != nil)
			// The services domain (D26) serves /svc/ apart from the
			// dashboard; the dashboard's host is proxyAuthDomain.
			if err := checkServicesDomain(cfg.ServicesDomain, proxyAuthDomain, cfg.HTTPProxyListen != ""); err != nil {
				return err
			}
			if cfg.ServicesDomain == "" {
				log.Info("services domain not set: tunnelled apps share the dashboard's origin; " +
					"approving a client sign-in asks for the password. Set BURROW_SERVICES_DOMAIN to serve them on a host of their own")
			}

			// v0.5.0 F-13: the connection-log sink. The proxy records one
			// connection_logs row per closed request, the control server one
			// per tcp visitor; adapter shims translate their entries into the
			// concrete connlog.Entry — kept here (not in internal/proxy or
			// internal/server) so those packages stay import-free of connlog.
			//
			// v0.5.1 P2.1 (Task 5): pass the store as the settings
			// reader so SQLSink.Rollup honours the
			// connection_logs.rollup_include_top_ips toggle. The
			// settings reader is only consulted on the daily rollup
			// compaction path, NOT the per-connection Record hot path.
			connLogSink := connlog.NewSQLSink(db.Wrap(database), log).WithSettings(st)

			srv, err := server.New(server.Options{
				Listen: cfg.Listen, TLSCert: cfg.TLSCert, TLSKey: cfg.TLSKey,
				PublicBind: cfg.PublicBind, PortMin: cfg.PortMin, PortMax: cfg.PortMax,
				// A refused token counts as a failed sign-in of its source;
				// a failure of the store does not (controlAuth).
				Auth: controlAuth{st}, Tunnels: tunnelStoreAdapter{st}, Events: bus, Logger: log,
				SessionRecorder: st,
				ServiceRecorder: st,
				// v0.3.0: HTTP tunnel service identity + subdomain resolver;
				// v0.13.0: tcp tunnels are services too, with sticky ports.
				Services: serviceResolverAdapter{db: db.Wrap(database), authDomain: proxyAuthDomain, tcpPortsPerUser: cfg.TCPPortsPerUser},
				ConnLog:  serverConnLogAdapter{sink: connLogSink},
				// v0.13.0: the ip policy and cap of a tcp service, read when
				// its tunnel registers and when the API says it changed.
				TCPAccess: tcpAccessAdapter{db: db.Wrap(database)},
				// v0.13.0: forward streams to tcp services (burrow forward),
				// authorized on every open.
				Forwards:   forwardAuthorizer{db: db.Wrap(database)},
				AuthDomain: proxyAuthDomain,
				// Tunnel addresses are on the services host when one is set.
				ServicesDomain: cfg.ServicesDomain,
				// The control handshake refuses clients older than the minimum.
				MinClientVersion: cfg.MinClientVersion,
				// v0.6.0: when ACME is enabled the control channel serves the
				// ACME-managed certificate instead of TLSCert/TLSKey files.
				// acmeGetCert returns nil when acmeMgr is nil, so the server
				// falls back to file certs exactly as before (unchanged).
				GetCertificate: acmeGetCert(acmeMgr),
				CertNames:      acmeCertNames(cfg, acmeMgr),
				// tcp complete (E1): the TLS passthrough listener names
				// every tcp service <slug>.<tcp domain>, and believes a
				// PROXY protocol header from the trusted proxies only.
				TCPDomain:      cfg.TCPDomain,
				TrustedProxies: trustedPrefixes(cfg.TrustedProxies),
			})
			if err != nil {
				return err
			}

			// Start the session reaper: purges expired sessions and prunes the
			// OAuth tables once at startup and then every hour. Mirrors the byteTicker goroutine pattern in
			// internal/server: WaitGroup-tracked, ctx-cancelled, stops before
			// database.Close() (enforced by LIFO defers above).
			runSessionReaper(ctx, &reaperWg, db.Wrap(database), log, time.Hour)

			// Traffic flusher: persists live byte counters into
			// tunnels.total_bytes_* every ~30s (and once at shutdown).
			// Tracked on reaperWg so its final flush runs before the
			// deferred database.Close() (LIFO defer ordering).
			runTrafficFlusher(ctx, &reaperWg, srv, st, log, 30*time.Second)

			// Sessions of client tokens whose expiry has passed end within 30s.
			runExpiredTokenSweeper(ctx, &reaperWg, st, srv, log, 30*time.Second)

			spaHandler, err := web.Handler()
			if err != nil {
				return err
			}

			// httpsEnabled is true when both HTTP TLS cert+key are configured.
			// effectiveSecureCookies forces Secure on cookies whenever TLS is
			// active (a TLS-served cookie MUST be Secure); the operator-facing
			// http_secure_cookies flag also covers proxy-terminated TLS. ACME
			// always serves the dashboard over HTTPS, so cookies are Secure too.
			httpsEnabled := cfg.HTTPTLSCert != "" && cfg.HTTPTLSKey != ""
			effectiveSecureCookies := httpsEnabled || cfg.HTTPSecureCookies || acmeMgr != nil

			if acmeMgr == nil && !httpsEnabled && !cfg.HTTPSecureCookies {
				log.Warn("dashboard/session cookie is transmitted in plaintext; " +
					"set BURROW_HTTP_TLS_CERT/BURROW_HTTP_TLS_KEY for native HTTPS " +
					"or terminate TLS at a proxy and set BURROW_HTTP_SECURE_COOKIES=true")
			}

			// v0.4.0 Task 20: backup directory + runners. Defaults to
			// <DatabasePath>.backups/ so a stock deployment gets a working
			// JSON API out of the box; operators may pin a dedicated
			// location via BURROW_BACKUP_DIR in a future task.
			backupDir := cfg.DatabasePath + ".backups"
			if cfg.BackupDir != "" {
				backupDir = cfg.BackupDir
			}
			restoreTracker := api.NewRestoreTracker()

			// BURROW_CERT_VALIDATION_ROOTS_FILE: load a custom CA pool for
			// custom-domain cert-chain validation. Fail-soft: a missing or
			// unparseable file is logged as a warning and the system root
			// pool is used instead (nil CertValidationRoots in api.Deps).
			var certValidationRoots *x509.CertPool
			if cfg.CertValidationRootsFile != "" {
				pemBytes, readErr := os.ReadFile(cfg.CertValidationRootsFile)
				if readErr != nil {
					log.Warn("BURROW_CERT_VALIDATION_ROOTS_FILE: cannot read file; using system roots",
						"file", cfg.CertValidationRootsFile, "err", readErr)
				} else {
					pool := x509.NewCertPool()
					if !pool.AppendCertsFromPEM(pemBytes) {
						log.Warn("BURROW_CERT_VALIDATION_ROOTS_FILE: no valid PEM certificates found; using system roots",
							"file", cfg.CertValidationRootsFile)
					} else {
						certValidationRoots = pool
						log.Info("BURROW_CERT_VALIDATION_ROOTS_FILE: custom CA pool loaded",
							"file", cfg.CertValidationRootsFile)
					}
				}
			}

			// v0.4.0 Task 25: compose every Task 3–23 engine into a single
			// stack. The constructor honours cfg.PricingPath, auto-generates
			// the audit signing key on first boot, and ships a no-op geo
			// lookup in the default build (real MMDB under -tags geo).
			v04, err := buildV04Stack(ctx, cfg, database, st, log)
			if err != nil {
				return err
			}
			v04.WebhookDispatcher.Start()
			// Sessions, tunnels and tcp connections are told to the observer;
			// no client is served yet.
			srv.SetObserver(v04.Observer)
			// The gauges a scrape reads, and the statement durations. Stopped
			// before the database closes (LIFO defers).
			defer registerGauges(v04.Metrics, database, backend.Driver(), log)()
			// Defer ordering matters here: WebhookDispatcher.Close MUST run
			// AFTER reaperWg.Wait so that any goroutine tracked by reaperWg
			// (e.g. the v0.5.2 custom-domain status tick) that may call
			// WebhookDispatcher.Publish has fully drained before the dispatcher's
			// internal queue channel is closed — otherwise a tick mid-iteration
			// at shutdown panics on send-to-closed-channel. We therefore register
			// the dispatcher.Close defer FIRST (it runs LAST in LIFO order) and
			// reaperWg.Wait SECOND (it runs FIRST in LIFO order, before
			// dispatcher.Close).
			defer v04.WebhookDispatcher.Close()
			defer reaperWg.Wait()
			// RefreshRolesCache populates the process-wide authz custom-roles
			// table once at startup so the very first request sees the same
			// permission map every store-level role mutation maintains.
			if err := st.RefreshRolesCache(ctx); err != nil {
				log.Warn("v0.4 refresh roles cache failed", "err", err)
			}

			// The management MCP server, mounted at /mcp/burrow by the API
			// router below. Its tools are built from the embedded OpenAPI
			// document; a document the server cannot use stops the relay.
			// It replays every call through the API router, handed to it
			// below once the router exists.
			v04.MCPServer, err = BuildMCPServer(st, v04, api.OpenAPIDocument(), log)
			if err != nil {
				log.Error("mcp server: the OpenAPI document cannot be used", "err", err)
				return err
			}

			// v0.5.0 Task 17: build every new v0.5.0 component and wire the
			// semantic cache + credinject injector into the aigw.Chain.
			v05, err := buildV05Stack(ctx, db.Wrap(database), v04.Metrics, log)
			if err != nil {
				return err
			}
			// Patch the chain fields that were constructed as nil placeholders
			// in buildV04Stack (see the "wired in Task 17" comments there).
			// This must happen before any request is served — safe here because
			// no listener has been started yet.
			v04.AIChain.Semantic = v05.SemanticCache
			v04.AIChain.CredInjector = v05.CredInjector

			// v0.5.0 Task 9: daily retention compaction job.
			// Fires once per day at 00:30 UTC (spec N); ctx cancellation
			// exits the Tick loop before database.Close() (LIFO defer order).
			reaperWg.Add(1)
			go func() {
				defer reaperWg.Done()
				dbWrapped := db.Wrap(database)
				loader := retention.NewDBLoader(dbWrapped)
				auditAdapter := retention.NewAuditLoggerAdapter(v04.AuditLogger)
				compactor := retention.New(dbWrapped, loader, auditAdapter, log)
				compactor.OnRun = retentionCompacted(v04.Metrics)
				compactor.Tick(ctx, "00:30")
			}()

			// v0.5.2 Task 10: daily custom-domain status state-machine tick.
			// Mirrors the retention compactor lifecycle (LIFO defer order
			// ensures ctx cancels before database.Close()). Walks all
			// cert-bearing custom domains, persists active/cert_expiring/
			// cert_expired transitions, and fires custom_domain.cert.expiring
			// exactly once on the active->cert_expiring edge.
			reaperWg.Add(1)
			go func() {
				defer reaperWg.Done()
				customdomain.StatusTick(ctx, customdomain.StatusTickDeps{
					DB:      db.Wrap(database),
					Audit:   v04.AuditLogger,
					Webhook: v04.WebhookDispatcher,
					Log:     log,
				}, "00:30")
			}()

			// Dashboard listen address. With ACME on, promote the stock
			// default (:8080) to :443 so the HTTPS dashboard answers on the
			// canonical port; an operator who pinned a custom http_listen keeps
			// it. ACME off → httpListen == cfg.HTTPListen, unchanged.
			httpListen := cfg.HTTPListen
			if acmeMgr != nil && httpListen == ":8080" {
				httpListen = ":443"
				log.Info("acme: dashboard binding :443", "was", cfg.HTTPListen)
			}

			// Host-routing proxy handler (built unconditionally). It backs both:
			//   1. the dedicated :8443 proxy listener (proxySrv, below), and
			//   2. the single-origin /svc/{slug} path route mounted on the API/
			//      dashboard router (api.Deps.TunnelProxy), so http tunnels are
			//      reachable on the same :443 origin even when HTTPProxyListen is
			//      empty (feat/builtin-acme B3).
			// Construction is cheap and pure (no listener, no goroutine), so it
			// is hoisted above api.NewRouter rather than guarded. The :8443
			// listener block below reuses this handler — it does NOT build a
			// second one. ingressPort is derived from cfg.HTTPProxyListen and may
			// be "" when the proxy listener is disabled; WithIngressPort is then
			// simply not appended (the proxy falls back to the request's own port
			// for X-Forwarded-Port).
			var ingressPort string
			if cfg.HTTPProxyListen != "" {
				if _, port, err := net.SplitHostPort(cfg.HTTPProxyListen); err == nil && port != "" {
					ingressPort = port
				}
			}
			// Proxy ingress serves TLS when file certs are configured OR when
			// ACME is enabled (ACME manages the ingress cert too). Governs the
			// WithTLSBase / proxySrv.TLSConfig wiring below.
			proxyTLSEnabled := (cfg.HTTPProxyTLSCert != "" && cfg.HTTPProxyTLSKey != "") || acmeMgr != nil

			accessChecker := proxy.NewAccessCheckerWithSessionsAndLogger(st, st, proxyAuthDomain, log,
				proxy.WithAccessSecureCookies(effectiveSecureCookies))
			gate := proxy.NewGate(st, proxyAuthDomain, effectiveSecureCookies, log,
				proxy.WithServicesDomain(cfg.ServicesDomain), proxy.WithGateAuditLogger(v04.AuditLogger))
			// The daily rollup of the connection log, announced per service
			// as connection.session_summary. Ends with ctx, before the
			// dispatcher closes (reaperWg).
			reaperWg.Add(1)
			go func() {
				defer reaperWg.Done()
				connLogRoller{
					sink: connLogSink, db: db.Wrap(database),
					events: v04.WebhookDispatcher, audit: v04.AuditLogger, log: log,
				}.tick(ctx)
			}()
			// v0.5.0 F-14: wire the custom-domain routing hook. The proxy
			// invokes this closure when the inbound Host header does NOT
			// end with ".<authDomain>" (proxy.go:285 — the dead-code
			// branch the hook activates). The closure adapts
			// v05.CustomDomainStore.LookupBySNI (which returns a Cert with
			// a ServiceID field) into the (serviceID, ok, err) shape the
			// proxy expects. On a miss the proxy falls through to its
			// existing notFound path; on a real DB / parse error it
			// returns 502 (logged with host + err).
			customDomainLookup := func(ctx context.Context, host string) (string, bool, error) {
				if v05.CustomDomainStore == nil {
					return "", false, nil
				}
				cert, ok, err := v05.CustomDomainStore.LookupBySNI(ctx, host)
				if err != nil || !ok {
					return "", ok, err
				}
				return cert.ServiceID, true, nil
			}
			proxyOpts := []proxy.Option{
				proxy.WithGate(gate),
				// v0.4.0 Task 25: wire the AI middleware chain into the
				// proxy. The chain is pure pass-through when no
				// service_ai_config row exists (IsAIPassThrough), which
				// preserves the FlushInterval=-1 / SSE / WebSocket
				// invariants byte-for-byte for v0.3.0 traffic.
				proxy.WithAIChain(v04.AIChain),
				proxy.WithConnLogSink(proxyConnLogAdapter{sink: connLogSink}),
				proxy.WithCustomDomainLookup(customDomainLookup),
				// Wire the audit logger so the proxy can emit
				// audit.ActionAIUpstreamError rows on upstream failures
				// for AI-gateway services (spec 27 / audit log contract).
				proxy.WithAuditLogger(v04.AuditLogger),
				// Each answered request of an http service is told to the
				// client that serves it, when that client asked (the status
				// view of `burrow http`). The control server never waits.
				proxy.WithSummarySink(srv),
				// Each finished request is told to the observer: metrics,
				// webhook events.
				proxy.WithObserver(v04.Observer),
			}
			if ingressPort != "" {
				proxyOpts = append(proxyOpts, proxy.WithIngressPort(ingressPort))
			}
			// The host-routed ingress behind a proxy (Traefik) reads the
			// visitor's address from X-Forwarded-For of a trusted peer, as
			// the dashboard's /svc/ path does, so that ip rules, limits and
			// logs see the visitor and not the proxy. On /svc/ the API's
			// middleware has already put that address in RemoteAddr.
			if len(cfg.TrustedProxies) > 0 {
				proxyOpts = append(proxyOpts, proxy.WithTrustedProxies(clientip.BuildCIDRs(cfg.TrustedProxies)))
			}
			// When TLS is enabled, build the base config first so it can be
			// passed to the proxy via WithTLSBase. The proxy's
			// GetConfigForClient hook uses the base config as a template when
			// cloning per-vhost configs for mTLS services.
			var proxyTLSCfg *tls.Config
			if proxyTLSEnabled {
				proxyTLSCfg = &tls.Config{
					MinVersion: tls.VersionTLS12,
					// Request (but don't require) a client cert on every TLS
					// connection. For mTLS services GetConfigForClient overrides
					// this with RequireAndVerifyClientCert + per-service ClientCAs
					// when the SNI label matches a registered subdomain. Using
					// RequestClientCert (no TLS-layer verification) allows a client
					// to present a cert even when SNI doesn't resolve to the service
					// subdomain (e.g. connecting via localhost:8443 with a Host
					// header override). The application-layer checkMTLS performs the
					// real cert-chain verification against the service's CA PEM.
					ClientAuth: tls.RequestClientCert,
				}
				// customGet serves any custom-domain certificate uploaded by an
				// operator. With ACME on, fall back to the ACME-managed cert for
				// the configured domain(s) when no custom cert matches the SNI,
				// and advertise the TLS-ALPN-01 protocol so ingress can also
				// solve TLS-ALPN challenges. Without ACME, the proxy serves only
				// custom-domain certs (file-cert mode), exactly as before.
				customGet := customdomain.CertCallback(v05.CustomDomainStore, nil)
				if acmeMgr != nil {
					proxyTLSCfg.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
						if c, err := customGet(hello); err == nil && c != nil {
							return c, nil
						}
						return acmeMgr.GetCertificate(hello)
					}
					proxyTLSCfg.NextProtos = append(proxyTLSCfg.NextProtos, acme.ACMETLSProto)
				} else {
					proxyTLSCfg.GetCertificate = customGet
				}
				proxyOpts = append(proxyOpts, proxy.WithTLSBase(proxyTLSCfg))
			}
			// The proxy's doors do not know a gateway-only service, by slug
			// or by service id; the AI gateway below keeps the plain adapter.
			proxyHandler := proxy.New(
				ingressDialerAdapter{proxyDialerAdapter{st: st, srv: srv}},
				accessChecker,
				proxyAuthDomain,
				log,
				proxyOpts...,
			)
			// One transport for all direct AI providers: it carries the
			// address guard and the connection pool, never a credential.
			aiUpstreamTransport := aiprovider.NewTransport(cfg.AIAllowPrivateUpstreams)
			// The MCP gateway's upstream side: remote tool sources leave
			// through the same guarded transport, tunnel sources through the
			// client's tunnel. Tool syncs and calls run off the control loop.
			mcpPool := mcpgw.New(mcpgw.Options{
				Store: st, Vault: v05.CredVault, Remote: aiUpstreamTransport,
				Tunnels: proxyDialerAdapter{st: st, srv: srv},
				InspectText: func(ctx context.Context, text []byte) (bool, string) {
					return v04.GuardrailsEngine.InspectRequest(text, v04.GlobalGuardrails(ctx).CustomPatterns)
				},
				OnBackgroundSync: func(slug string, res mcpgw.SyncResult) {
					_ = v04.AuditAppender.Append(context.Background(), audit.Event{
						Action: audit.ActionMCPSourceSync, SubjectID: slug, SubjectLabel: slug, Result: "ok",
						Payload: audit.MustJSON(map[string]any{
							"added": res.Added, "changed": res.Changed, "removed": res.Removed,
							"pending": res.Pending, "skipped": len(res.Skipped), "background": true,
						}),
					})
				},
				Log: log,
			})
			mcpPool.Start(ctx)
			// The OAuth authorization server for MCP clients. Its issuer is
			// the dashboard's https origin (BURROW_AUTH_DOMAIN, or the first
			// ACME domain), https also behind a TLS-terminating proxy.
			// Without one it is off: its endpoints answer 404, except to a
			// request sent to a loopback host (development).
			oauthIssuer := ""
			if proxyAuthDomain != "" {
				oauthIssuer = "https://" + proxyAuthDomain
			} else {
				log.Warn("oauth: authorization server off (served on loopback hosts only): no public name; set BURROW_AUTH_DOMAIN or BURROW_ACME_DOMAIN")
			}
			oauthServer := oauth.New(oauth.Config{
				Store: db.Wrap(database), Issuer: oauthIssuer, Audit: v04.AuditAppender, Log: log,
			})

			// The management server and the tool gateway take its access
			// tokens, each at its own resource.
			v04.MCPServer.SetOAuth(oauthServer)
			// The client side: /mcp/<slug> and /mcp behind gateway keys and
			// OAuth grants, each call through the pipeline of limits,
			// redaction and guardrails.
			mcpTools := newMCPToolGateway(st, v04, mcpPool, oauthServer, cfg.OAuthGrantRPM, log)
			// One breaker for the relay; the status API reads its state.
			aiBreaker := aigateway.NewBreaker()
			// One limiter for the relay: every door to a provider shares its
			// places, and the provider API reads how many are taken.
			aiLimiter := aigateway.NewLimiter()
			// The /ai/ data plane shares the proxy's dialer, AI chain and
			// IP/geo policy.
			// One metrics store: the providers page and fastest routing read
			// the same cached p95 figures.
			aiMetrics := newAIMetricsStore(db.Wrap(database), log)
			aiGateway := newAIGateway(aiGatewayParts{
				Latency:    aiMetrics.serviceLatencies,
				Store:      st,
				Tunnels:    proxyDialerAdapter{st: st, srv: srv},
				Chain:      v04.AIChain,
				Vault:      v05.CredVault,
				Injector:   v05.CredInjector,
				Upstream:   aiUpstreamTransport,
				CostEngine: v04.CostEngine,
				BudgetTTL:  15 * time.Second,
				Breaker:    aiBreaker,
				Limiter:    aiLimiter,
				IPGeoDeny:  proxyHandler.IPGeoDenied,
				PublicHost: proxyAuthDomain,
				Log:        log,
			})
			// The chain asks the judge model through the gateway, which is
			// built only now; no listener has been started yet.
			v04.Judge.set(aiGateway)
			if proxyTLSCfg != nil {
				// Wire GetConfigForClient so mTLS services get a per-vhost
				// TLS config with ClientCAs + RequireAndVerifyClientCert at
				// TLS-handshake time. Non-mTLS vhosts continue to use the
				// base config (no client cert required). Set here (after
				// proxy.New) so both the :8443 listener and any future TLS
				// consumer of proxyTLSCfg observe the hook.
				proxyTLSCfg.GetConfigForClient = proxyHandler.GetConfigForClient
			}

			apiRouter := api.NewRouter(api.Deps{
				Users: st, Tunnels: tunnelListerAdapter{s: srv, access: st, services: st, slugs: st, authDomain: proxyAuthDomain, servicesDomain: cfg.ServicesDomain}, Events: bus,
				Log: log, SecureCookies: effectiveSecureCookies, HTTPSEnabled: httpsEnabled,
				SPA: spaHandler, TrustedProxies: cfg.TrustedProxies,
				// Wire env override: 0 means use the api.LoginRateLimitPerIP constant.
				LoginRateLimitPerIPOverride: cfg.LoginRateLimitPerIP,
				Roles:                       st, Sessions: st, Settings: st,
				Clients: clientsAdapter{srv: srv, st: st, services: st, tokens: st}, AccessModes: st,
				// Revoking a client token, suspending or deleting its user
				// ends the sessions that are connected with it.
				ClientSessions: srv,
				// Deleting a service ends the session that serves it and
				// drops what memory holds under its id: the inspector ring
				// and the semantic cache collection (the rows go with the
				// service by cascade).
				ServiceSessions: srv,
				// A tcp service's mode, ip lists, grants and cap reach its
				// live tunnel; just-in-time grants are kept in the database.
				TCPServices: srv,
				IPGrants:    db.Wrap(database),
				Connections: srv,
				Forwards:    srv,
				OnServiceDeleted: func(serviceID string) {
					v04.InspectorMgr.Remove(serviceID)
					_ = v05.SemanticCache.ClearService(context.Background(), serviceID)
				},
				DB: database,
				// v0.3.0: service API + live tunnel lookup + auth domain.
				Services:    st,
				AIProviders: st,
				MCPSources:  st,
				MCPGateway:  mcpGatewayParts{Pool: mcpPool, tools: mcpTools},
				MCPTools:    mcpTools,
				OAuth:       oauthServer,
				AILimiter:   aiLimiter,
				AIBreaker:   aiBreaker,
				AIAttempts:  st,
				// Direct providers: the save-time host check follows the
				// same switch as the dial guard, and the model sync leaves
				// through the guarded transport.
				AllowPrivateUpstreams: cfg.AIAllowPrivateUpstreams,
				HostCheck: func(ctx context.Context, host string) error {
					return aiprovider.CheckHostPublic(ctx, net.DefaultResolver, host)
				},
				FetchProviderModels: func(ctx context.Context, p db.AIProvider) ([]aiprovider.Model, error) {
					return aiprovider.FetchModels(ctx, aiprovider.Config{
						Slug: p.Slug, BaseURL: p.BaseURL, CredentialSlot: p.CredentialSlot,
						AuthHeader: p.AuthHeader, AuthFormat: p.AuthFormat, ExtraHeaders: p.ExtraHeaders,
					}, v05.CredVault, aiUpstreamTransport)
				},
				LiveTunnels: liveTunnelLookupAdapter{srv: srv},
				// AuthDomain uses the same resolved value the proxy/access
				// checker/gate route on (cfg.AuthDomain, or the first ACME
				// domain when auth_domain is unset and ACME is on). In the
				// common case this equals cfg.AuthDomain, so composeHostname
				// and the burrow_login-409 check are unchanged.
				AuthDomain: proxyAuthDomain,
				// D26: /svc/ and the gate on a host of their own, when set.
				ServicesDomain: cfg.ServicesDomain,
				// mTLS is only offered while the host-routed ingress runs.
				HostRouting: cfg.HTTPProxyListen != "",
				// What the tcp-hostname route says and refuses.
				TCPDomain:      cfg.TCPDomain,
				TLSPassthrough: cfg.TLSPassthroughListen != "",
				// feat/builtin-acme B3: single-origin /svc/{slug} tunnel routing.
				// proxyHandler is the hoisted host-routing proxy. The router
				// mounts /svc/{slug} + /svc/{slug}/* on it via ServicePathHandler only
				// when AuthDomain (above) is non-empty; the synthesized host
				// "<id>.<AuthDomain>" then matches the proxy's own routing
				// suffix exactly. When AuthDomain is "" the routes are skipped.
				TunnelProxy: proxyHandler,
				AIGateway:   aiGateway,
				// The burrow_login gate, served at /__burrow/* on this origin so
				// path-routed services can redirect visitors to it.
				Gate: gate,
				// Control plane is on a different port from the API/dashboard
				// (e.g. compose maps :7000 control, :8080 dashboard). Surface
				// it via /clients/connect-info so the "Connect a client" wizard
				// prints a working `burrow connect --server …` command. (P1-2)
				ControlListen: cfg.Listen,
				// Pinning of :7000 (D13): discovery names the leaf it presents.
				ControlCertSHA256: srv.ControlCertSHA256,
				// Reported by the public /client/discovery endpoint.
				MinClientVersion: cfg.MinClientVersion,
				// Browser-approved client sign-in (/client/login/*).
				ClientLogins: st,
				// Where /download/ and the install scripts send a client.
				ClientDownloadBase: cfg.ClientDownloadBase,
				ClientDownloadDir:  cfg.ClientDownloadDir,
				// v0.4.0 Task 20: backup / restore wiring.
				BackupDir:      backupDir,
				BackupRunner:   backupRunnerAdapter{cfg: cfg, events: v04.WebhookDispatcher},
				RestoreRunner:  restoreRunnerAdapter{cfg: cfg},
				RestoreTracker: restoreTracker,
				AuditAppender:  v04.AuditAppender,
				// v0.4.0 Task 25 wiring — every additive Deps surface.
				AuditEvents:           v04.AuditEvents,
				AuditSessionRefCutoff: v04.AuditSessionRefCutoff,
				AuditChain:            v04.AuditChain,
				Metrics:               api.NewMetricsRecorderAdapter(v04.Metrics),
				Webhooks:              db.Wrap(database),
				WebhookDispatcher:     v04.WebhookDispatcher,
				WebhookSecrets:        v04.WebhookSecrets,
				Bearer:                api.NewStoreBearerStore(st),
				Automation:            st,
				MCP: api.MCPInfo{
					Enabled: true,
					Server:  v04.MCPServer,
					Handler: v04.MCPServer,
				},
				RateLimitDB:       db.Wrap(database),
				RateLimits:        v04.QuotaEngine,
				Budgets:           db.Wrap(database),
				CostEngine:        v04.CostEngine,
				AIMetrics:         aiMetrics,
				CacheEngine:       v04.CacheEngine,
				CacheServices:     cacheServiceLookupAdapter{db: db.Wrap(database)},
				InspectorRings:    v04.InspectorMgr,
				InspectorServices: cacheServiceLookupAdapter{db: db.Wrap(database)},
				InspectorReplayer: newInspectorReplayer(v04.AIChain, log),
				AIModels:          st,
				AIGatewayKeys:     st,
				IPGeo:             db.Wrap(database),
				IPGeoServices:     db.Wrap(database),
				GeoLookup:         v04.GeoLookup,
				// v0.5.0 Task 15: database backend status surface.
				// For SQLite, URLRedacted is the on-disk path (no credentials);
				// for Postgres it is the DSN with user:pass redacted.
				Database: api.DBInfo{
					Driver:      backend.Driver(),
					URLRedacted: dbURLForStatus(cfg, backend),
					Alpha:       cfg.ExperimentalPostgres && backend.Driver() == "postgres",
				},
				// v0.5.0 Task 17 + P2.2 wiring — new Deps surfaces for Tasks 3-11.
				// SemanticEngine: newSemanticEngine returns noopSemanticEngine in
				// the default build (GET /cache/stats returns zeros, ClearAll is a
				// no-op) and semanticEngineAdapter under -tags=semantic_cache (sums
				// per-service Stats() across all registered services, implements
				// ClearAll by iterating ListAllServices).
				SemanticEngine:      newSemanticEngine(v05.SemanticCache, db.Wrap(database)),
				ServiceAIConfigs:    db.Wrap(database),
				CredentialVault:     v05.CredVault,
				CredentialDB:        db.Wrap(database),
				CredentialServices:  db.Wrap(database),
				CustomDomains:       db.Wrap(database),
				CustomDomainCache:   v05.CustomDomainStore,
				CertValidationRoots: certValidationRoots,
				ConnLogDB:           v05.ConnLogDB,
			})
			// A tool call of the MCP server is replayed through this router,
			// so it is the same API with the same gate. Set before anything
			// listens.
			v04.MCPServer.SetAPIHandler(apiRouter)
			apiSrv := &http.Server{
				Addr:              httpListen,
				Handler:           apiRouter,
				ReadHeaderTimeout: 10 * time.Second,
			}
			if acmeMgr != nil {
				// ACME-managed TLS (GetCertificate + TLS-ALPN-01) for the dashboard.
				apiSrv.TLSConfig = acmeMgr.TLSConfig()
			} else if httpsEnabled {
				apiSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			}

			// senderCount tracks how many goroutines send on errc; capacity must
			// equal senderCount so that no sender ever blocks after shutdown.
			// Baseline: control listener (srv.Serve) + api server = 2.
			// The proxy listener adds 1 when cfg.HTTPProxyListen is non-empty.
			// The ACME :80 challenge+redirect listener adds 1 when ACME is on.
			// The TLS passthrough listener adds 1 when it is configured.
			senderCount := 2
			if cfg.TLSPassthroughListen != "" {
				senderCount++
			}

			// v0.6.0: when ACME is enabled, run a :80 listener that solves
			// HTTP-01 challenges and redirects every other request to https://.
			// Counted as one errc sender (incremented before errc is sized).
			var challengeSrv *http.Server
			if acmeMgr != nil {
				senderCount++
				redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					u := *r.URL
					u.Scheme = "https"
					u.Host = r.Host
					// G710: standard HTTP->HTTPS upgrade. The target is the same
					// host + path the client already requested (just scheme=https);
					// it cannot point at an attacker-chosen external origin.
					//nolint:gosec // G710: same-host scheme upgrade, not attacker-controlled.
					http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
				})
				challengeSrv = &http.Server{
					Addr:              ":80",
					Handler:           acmeMgr.HTTPChallengeHandler(redirect),
					ReadHeaderTimeout: 10 * time.Second,
				}
			}

			// Build the optional HTTP reverse-proxy listener (v0.3.0).
			// Started only when HTTPProxyListen is non-empty (off by default).
			// The host-routing proxyHandler (+ its TLS base config) is built
			// ABOVE, before api.NewRouter, so it can ALSO back the single-origin
			// /svc/{slug} route on the dashboard origin (feat/builtin-acme B3); this
			// block only wraps that already-built handler in a dedicated listener.
			// TLS is used iff both HTTPProxyTLSCert + HTTPProxyTLSKey are set (or
			// ACME is on); otherwise the listener runs plain HTTP (operator may
			// terminate TLS upstream — e.g. nginx or a cloud load-balancer). A
			// warning is logged in that case so it is never silently insecure.
			var proxySrv *http.Server
			if cfg.HTTPProxyListen != "" {
				senderCount++
				proxySrv = &http.Server{
					Addr:              cfg.HTTPProxyListen,
					Handler:           proxyHandler,
					ReadHeaderTimeout: 10 * time.Second,
				}
				if proxyTLSCfg != nil {
					// proxyTLSCfg is non-nil exactly when proxyTLSEnabled (file
					// certs configured OR ACME on). GetConfigForClient is already
					// wired to proxyHandler in the hoisted construction above, so
					// mTLS services still get a per-vhost TLS config with ClientCAs
					// + RequireAndVerifyClientCert at TLS-handshake time; non-mTLS
					// vhosts use the base config (no client cert required).
					proxySrv.TLSConfig = proxyTLSCfg
				}
			}

			errc := make(chan error, senderCount)
			// The control listener ends every session when its context ends,
			// and each tunnel of a session is then told to the observer as
			// closed. Its context therefore ends after the observer has
			// stopped publishing: a shutdown announces no tunnel.disconnected
			// for the tunnels it closes itself, and does not wait on webhook
			// receivers for them.
			srvCtx, stopSrv := context.WithCancel(context.WithoutCancel(ctx))
			defer stopSrv()
			context.AfterFunc(ctx, func() {
				v04.Observer.stopPublishing()
				stopSrv()
			})
			go func() { errc <- srv.Serve(srvCtx) }()
			if cfg.TLSPassthroughListen != "" {
				// TLS passthrough by SNI to tcp services (E1). It ends with
				// the control listener: both carry tcp visitors.
				go func() { errc <- srv.ListenPassthrough(srvCtx, cfg.TLSPassthroughListen) }()
			}
			go func() {
				if acmeMgr != nil {
					// Certs come from apiSrv.TLSConfig.GetCertificate (ACME),
					// not from files — pass empty strings to ListenAndServeTLS.
					log.Info("http api listening (ACME TLS)", "addr", httpListen)
					if err := apiSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
						errc <- err
						return
					}
					errc <- nil
					return
				}
				if httpsEnabled {
					log.Info("http api listening (TLS)", "addr", httpListen)
					if err := apiSrv.ListenAndServeTLS(cfg.HTTPTLSCert, cfg.HTTPTLSKey); err != nil && err != http.ErrServerClosed {
						errc <- err
						return
					}
				} else {
					log.Info("http api listening", "addr", httpListen)
					if err := apiSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						errc <- err
						return
					}
				}
				errc <- nil
			}()
			if challengeSrv != nil {
				go func() {
					log.Info("acme http-01 + redirect listening", "addr", ":80")
					if err := challengeSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						errc <- err
						return
					}
					errc <- nil
				}()
			}
			if proxySrv != nil {
				// File-cert TLS branch governor (ACME handled separately below).
				proxyTLSEnabled := cfg.HTTPProxyTLSCert != "" && cfg.HTTPProxyTLSKey != ""
				go func() {
					if acmeMgr != nil {
						// Certs come from proxySrv.TLSConfig.GetCertificate (custom
						// domains, falling back to the ACME-managed cert) — pass
						// empty strings to ListenAndServeTLS.
						log.Info("http proxy listening (ACME TLS)", "addr", cfg.HTTPProxyListen)
						if err := proxySrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
							errc <- err
							return
						}
						errc <- nil
						return
					}
					if proxyTLSEnabled {
						log.Info("http proxy listening (TLS)", "addr", cfg.HTTPProxyListen)
						if err := proxySrv.ListenAndServeTLS(cfg.HTTPProxyTLSCert, cfg.HTTPProxyTLSKey); err != nil && err != http.ErrServerClosed {
							errc <- err
							return
						}
					} else {
						// Plaintext proxy: operator is expected to terminate TLS upstream
						// (e.g. nginx, cloud load-balancer). Logged as a warning so this
						// configuration is never silently insecure. The listener still
						// starts; use BURROW_HTTP_PROXY_TLS_CERT/KEY for native TLS.
						log.Warn("WARNING: http proxy listener enabled without TLS — http tunnels are unsecured; " +
							"set BURROW_HTTP_PROXY_TLS_CERT/BURROW_HTTP_PROXY_TLS_KEY for native TLS " +
							"or terminate TLS at a proxy")
						if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
							errc <- err
							return
						}
					}
					errc <- nil
				}()
			}
			// Wait for a shutdown signal OR an early server error (e.g. a
			// listener bind failure such as the HTTP port already in use):
			// surface it immediately instead of running half-dead until SIGINT.
			var firstErr error
			select {
			case <-ctx.Done():
			case firstErr = <-errc:
				stop() // cancel ctx so the other (healthy) servers unwind too
			}
			// apiShutdownGrace (35s) > api.JSONHandlerTimeout (30s): every
			// in-flight handler completes (or is chi-cancelled at 30s and
			// returns) before Shutdown returns. The deferred reaperWg.Wait()
			// and database.Close() then run in LIFO order after this point.
			//
			// Shutdown order (reverse of start): proxy → api → control listener (srv).
			// The proxy is shut first so no new tunnel streams are opened while
			// the control listener is still draining. MCP calls arrive on the
			// api server and drain with it.
			// The ACME :80 challenge+redirect listener (nil unless ACME is on)
			// is shut alongside the api tier it fronts.
			shutCtx, cancel := context.WithTimeout(context.Background(), apiShutdownGrace)
			defer cancel()
			if proxySrv != nil {
				_ = proxySrv.Shutdown(shutCtx)
			}
			_ = apiSrv.Shutdown(shutCtx)
			mcpPool.Close()
			// No /ai/ request is in flight any more.
			aiUpstreamTransport.CloseIdleConnections()
			// What the last requests tried is written before the database closes.
			if err := aiGateway.FlushAttempts(shutCtx); err != nil {
				log.Warn("ai gateway: attempt log not fully written", "err", err)
			}
			if err := v04.GuardrailAudit.Flush(shutCtx); err != nil {
				log.Warn("guardrail audit: entries not fully written", "err", err)
			}
			v04.GuardrailAudit.stop()
			if err := v04.Observer.Flush(shutCtx); err != nil {
				log.Warn("observer: work not fully done", "err", err)
			}
			v04.Observer.stop()
			if challengeSrv != nil {
				_ = challengeSrv.Shutdown(shutCtx)
			}
			srv.Wait()
			// One value already consumed iff the select took the errc branch;
			// drain the remaining senders so no goroutine leaks.
			remaining := senderCount
			if firstErr != nil {
				remaining = senderCount - 1
			}
			for i := 0; i < remaining; i++ {
				if e := <-errc; e != nil && e != http.ErrServerClosed && firstErr == nil {
					firstErr = e
				}
			}
			if firstErr != nil && firstErr != http.ErrServerClosed {
				return firstErr
			}
			return nil
		},
	}
	serveCmd.Flags().String("listen", "", "listen address (default :7000)")
	serveCmd.Flags().String("tls-cert", "", "TLS certificate PEM")
	serveCmd.Flags().String("tls-key", "", "TLS key PEM")
	serveCmd.Flags().Bool("dev-certs", false, "generate ./certs dev certs if missing")
	root.AddCommand(serveCmd)

	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Mint a client token for an existing user (dev/operator helper)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			email, _ := cmd.Flags().GetString("email")
			name, _ := cmd.Flags().GetString("name")
			cfg, err := config.LoadServer(nil)
			if err != nil {
				return err
			}
			// Use the same backend selector as `burrowd serve` so the token
			// command honors BURROW_DATABASE_URL / experimental_postgres_backend
			// (not just SQLite). openBackend logs the choice + runs migrations.
			log := logging.New(cfg.LogLevel, cfg.LogFormat)
			backend, err := openBackend(cfg, log)
			if err != nil {
				return err
			}
			defer backend.Close()
			st := store.New(backend.DB())
			u, err := st.GetUserByEmail(context.Background(), email)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					return fmt.Errorf("no user with email %q (seed an admin via BURROW_ADMIN_EMAIL/PASSWORD and run `serve` once)", email)
				}
				return err
			}
			tok, err := st.IssueClientToken(context.Background(), u.ID, name)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "issued token named %s for %s:\n", name, email)
			fmt.Println(tok)
			return nil
		},
	}
	tokenCmd.Flags().String("email", "", "user email to mint a token for (required)")
	tokenCmd.Flags().String("name", "cli", "token name/label")
	_ = tokenCmd.MarkFlagRequired("email")
	root.AddCommand(tokenCmd)

	// `burrowd audit` is the umbrella for audit-log operator commands.
	// Currently it has one subcommand: `audit verify`. Future audit
	// helpers (export, etc.) will attach here too.
	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit log operator commands",
	}
	auditCmd.AddCommand(newAuditVerifyCmd())
	root.AddCommand(auditCmd)

	// v0.4.0 Task 20: `burrowd backup` + `burrowd restore` CLIs. Both are
	// stand-alone top-level commands so the operator may run them against
	// any database file (the same path-resolution config.LoadServer uses
	// for serve), without needing a running burrowd.
	root.AddCommand(newBackupCmd())
	root.AddCommand(newRestoreCmd())
	root.AddCommand(newHealthcheckCmd())

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(versionLine())
		},
	})

	return root
}

// aiGatewayParts is what the /ai/ data plane is assembled from.
type aiGatewayParts struct {
	Store *store.Store
	// Tunnels reaches the tunnels of tunnel providers. It must resolve a
	// gateway-only service: the plain proxyDialerAdapter, not the proxy's
	// ingressDialerAdapter.
	Tunnels aigateway.TunnelDialer
	// Chain is the AI chain; nil = requests go to the upstream directly.
	Chain *aigw.Chain
	// Vault holds the upstream credentials of direct providers.
	Vault aiprovider.Vault
	// Injector applies the upstream credential bound to a tunnel provider's
	// service. It is the chain's own injector: the chain uses it on a
	// provider path, the gateway per attempt on a dialect endpoint, where a
	// request can go to more than one target. nil = no bound credentials.
	Injector *credinject.Injector
	// Upstream is the transport used for direct providers: it carries the
	// address guard and the connection pool, never a credential. Tests inject
	// one that reaches local test servers.
	Upstream http.RoundTripper
	// CostEngine enforces hard budgets; nil = no enforcement. BudgetTTL is
	// how long a budget verdict is reused.
	CostEngine *cost.Engine
	BudgetTTL  time.Duration
	Breaker    *aigateway.Breaker
	Limiter    *aigateway.Limiter
	// Latency feeds routing "fastest"; nil = it behaves like "ordered".
	Latency    aigateway.LatencySource
	IPGeoDeny  func(*proxy.Resolved, *http.Request) bool
	PublicHost string
	Log        *slog.Logger
}

// newAIGateway assembles the /ai/ data plane: /ai/<provider>/ and the dialect
// endpoints (/openai/v1, /ai/v1, /anthropic).
func newAIGateway(p aiGatewayParts) *aigateway.Gateway {
	g := &aigateway.Gateway{
		Providers:  p.Store,
		Keys:       p.Store,
		Tunnels:    p.Tunnels,
		IPGeoDeny:  p.IPGeoDeny,
		PublicHost: p.PublicHost,
		Log:        p.Log,
		// Direct providers: the upstream credential comes from the
		// env vault, the policy from the backing service's rows.
		// GET /v1/models answers from the stored catalog when it has entries.
		Models:        p.Store,
		Direct:        aigateway.DirectUpstreams(p.Vault, p.Upstream),
		ServicePolicy: directServicePolicy(p.Store),
		// The dialect endpoints (/openai/v1, /ai/v1, /anthropic): gateway
		// keys, synthetic models and the model list.
		GatewayKeys: p.Store,
		Synthetic:   p.Store,
		Catalog:     p.Store,
		// Fallback chains: a failing provider is skipped for a while,
		// and what a request tried is logged.
		Breaker:  p.Breaker,
		Attempts: p.Store,
		Limiter:  p.Limiter,
	}
	// A nil chain must stay a nil interface.
	if p.Chain != nil {
		g.Chain = p.Chain
	}
	// The same goes for the injector.
	if p.Injector != nil {
		g.Credentials = p.Injector
	}
	if p.CostEngine != nil {
		// Hard budgets stop requests at the gateway: a gateway key or
		// a model over a throttle_zero / disable_key budget gets 429
		// before any upstream is called. A typed nil must not become
		// a non-nil interface, hence the check. disable_key on a
		// gateway_key budget also revokes the key.
		p.CostEngine.SetGatewayKeyRevoker(p.Store)
		g.Budgets = cost.NewGuard(p.CostEngine, p.BudgetTTL)
		g.Prices = enginePrices(p.CostEngine)
	}
	if p.Latency != nil {
		g.Latency = p.Latency
	}
	return g
}

// enginePrices is the PriceSource of routing "cheapest": the blended price
// per million tokens (3:1 input to output) from the engine's current pricing,
// looked up as "<provider>/<model>" and then as the bare model.
func enginePrices(e *cost.Engine) aigateway.PriceSource {
	return func(provider, model string) (float64, bool) {
		p := e.Pricing()
		ent, ok := p.Lookup(provider + "/" + model)
		if !ok {
			ent, ok = p.Lookup(model)
		}
		if !ok {
			return 0, false
		}
		return 0.75*ent.InputPerMillion + 0.25*ent.OutputPerMillion, true
	}
}

// proxyConnLogAdapter bridges the proxy.ConnLogSink interface (which is
// declared in internal/proxy to keep that package import-free of connlog)
// to the concrete *connlog.SQLSink. Translation is one-to-one — every
// proxy.ConnLogEntry field maps directly to its connlog.Entry counterpart;
// the Kind / Status enums are string-cast through their typed equivalents.
//
// Lives here (not in internal/proxy) so the data-plane proxy package never
// imports connlog. The seam test in cmd/server/e2e_v050_default_test.go
// installs an identical adapter shape, which keeps test and production
// wiring symmetric.
type proxyConnLogAdapter struct {
	sink *connlog.SQLSink
}

func (a proxyConnLogAdapter) Record(ctx context.Context, e proxy.ConnLogEntry) error {
	if a.sink == nil {
		return nil
	}
	return a.sink.Record(ctx, connlog.Entry{
		Kind:            connlog.Kind(e.Kind),
		ServiceID:       e.ServiceID,
		TunnelID:        e.TunnelID,
		UserID:          e.UserID,
		ClientSessionID: e.ClientSessionID,
		SourceIP:        e.SourceIP,
		UserAgent:       e.UserAgent,
		StartedAt:       e.StartedAt,
		EndedAt:         e.EndedAt,
		BytesIn:         e.BytesIn,
		BytesOut:        e.BytesOut,
		Status:          connlog.Status(e.Status),
		Reason:          e.Reason,
	})
}

// serverConnLogAdapter hands the control server's connection-log entries
// (tcp visitors) to the concrete *connlog.SQLSink, as proxyConnLogAdapter
// does for the proxy's. Record writes in the background, so the visitor's
// goroutine never waits for the database.
type serverConnLogAdapter struct {
	sink *connlog.SQLSink
}

func (a serverConnLogAdapter) RecordConnection(ctx context.Context, e server.ConnLogEntry) {
	if a.sink == nil {
		return
	}
	_ = a.sink.Record(ctx, connlog.Entry{
		Kind:            connlog.Kind(e.Kind),
		ServiceID:       e.ServiceID,
		TunnelID:        e.TunnelID,
		UserID:          e.UserID,
		ClientSessionID: e.ClientSessionID,
		SourceIP:        e.SourceIP,
		StartedAt:       e.StartedAt,
		EndedAt:         e.EndedAt,
		BytesIn:         e.BytesIn,
		BytesOut:        e.BytesOut,
		Status:          connlog.Status(e.Status),
		Reason:          e.Reason,
		TokenName:       e.TokenName,
		Protocol:        e.Protocol,
		SNI:             e.SNI,
	})
}

// resolveAuthDomain returns the base domain of every public URL: the
// configured auth domain, or the first ACME-managed domain when none is set
// and built-in ACME is on.
func resolveAuthDomain(authDomain, acmeDomain string, acmeEnabled bool) string {
	if authDomain == "" && acmeEnabled {
		if d := splitAndTrim(acmeDomain); len(d) > 0 {
			return d[0]
		}
	}
	return authDomain
}

// acmeDomains is the list of names ACME obtains certificates for: the
// comma-separated BURROW_ACME_DOMAIN, then the services domain unless it is
// listed already. The first name stays first: it is the fallback auth domain.
func acmeDomains(acmeDomain, servicesDomain string) []string {
	out := splitAndTrim(acmeDomain)
	if servicesDomain == "" {
		return out
	}
	for _, d := range out {
		if strings.EqualFold(d, servicesDomain) {
			return out
		}
	}
	return append(out, servicesDomain)
}

// checkServicesDomain refuses a services domain without a dashboard host
// (the handoff and every redirect name it) or equal to it, and one under the
// dashboard's host while the host-routed ingress runs: that ingress reads
// <label>.<auth domain> as the service with slug <label>.
func checkServicesDomain(servicesDomain, dashboardHost string, hostRouting bool) error {
	if servicesDomain == "" {
		return nil
	}
	if dashboardHost == "" {
		return errors.New("BURROW_SERVICES_DOMAIN needs the dashboard's host: set BURROW_AUTH_DOMAIN or BURROW_ACME_DOMAIN")
	}
	if strings.EqualFold(servicesDomain, dashboardHost) {
		return fmt.Errorf("BURROW_SERVICES_DOMAIN must differ from the dashboard's host %q", dashboardHost)
	}
	if strings.HasSuffix(strings.ToLower(dashboardHost), "."+strings.ToLower(servicesDomain)) {
		return fmt.Errorf("BURROW_SERVICES_DOMAIN %q is a parent of the dashboard's host %q; "+
			"a page on %q is same-site with the dashboard and can set cookies for the whole %q domain (cookie tossing), "+
			"planting cookies the dashboard would read. Choose a services domain that is not above the dashboard's host (for example a sibling such as apps.%s)",
			servicesDomain, dashboardHost, servicesDomain, servicesDomain, servicesDomain)
	}
	if hostRouting && strings.HasSuffix(strings.ToLower(servicesDomain), "."+strings.ToLower(dashboardHost)) {
		return fmt.Errorf("BURROW_SERVICES_DOMAIN %q lies under %q while the host-routed ingress (BURROW_HTTP_PROXY_LISTEN) is on; "+
			"that ingress serves <slug>.%s as the service with that slug, so the services host would collide with a service name. "+
			"Choose a services domain outside %q (for example apps.example.com) or turn the host-routed ingress off",
			servicesDomain, dashboardHost, dashboardHost, dashboardHost)
	}
	return nil
}

// splitAndTrim splits a comma-separated string into a slice of trimmed,
// non-empty values. Used to parse BURROW_ACME_DOMAIN ("a.example.com,
// b.example.com") into the []string the ACME manager expects.
func splitAndTrim(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// acmeGetCert returns the manager's GetCertificate callback, or nil when the
// manager is nil (ACME disabled). A nil return lets server.Options /
// tls.Config consumers fall back to their file-cert paths unchanged.
func acmeGetCert(m *acme.Manager) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if m == nil {
		return nil
	}
	return m.GetCertificate
}

// acmeCertNames are the names the ACME manager serves; nil without ACME.
func acmeCertNames(cfg *config.ServerConfig, m *acme.Manager) []string {
	if m == nil {
		return nil
	}
	return acmeDomains(cfg.ACMEDomain, cfg.ServicesDomain)
}
