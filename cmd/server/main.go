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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/acme"
	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/connlog"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/devcert"
	"github.com/ankoehn/burrow/internal/events"
	"github.com/ankoehn/burrow/internal/logging"
	"github.com/ankoehn/burrow/internal/proxy"
	"github.com/ankoehn/burrow/internal/proxy/customdomain"
	"github.com/ankoehn/burrow/internal/retention"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
	"github.com/ankoehn/burrow/internal/version"
	"github.com/ankoehn/burrow/web"
)

// apiShutdownGrace is the timeout given to (*http.Server).Shutdown when the
// serve command receives a stop signal. It must be strictly greater than
// api.JSONHandlerTimeout (the chi middleware.Timeout applied to JSON routes)
// so that every in-flight handler has time to complete (or be chi-cancelled)
// before Shutdown returns and the deferred database.Close() runs.
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

// tunnelURL returns "https://<authDomain>/svc/<slug>/" for the durable service
// behind a tunnel, or "" when there is no auth domain, no service row or no
// slug (tcp tunnels have none).
func (a tunnelListerAdapter) tunnelURL(serviceID string) string {
	if a.slugs == nil || a.authDomain == "" || serviceID == "" {
		return ""
	}
	svc, err := a.slugs.ServiceByID(context.Background(), serviceID)
	if err != nil || svc.Subdomain == "" {
		return ""
	}
	return "https://" + a.authDomain + "/svc/" + svc.Subdomain + "/"
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

// clientsAdapter exposes live sessions + persisted per-service totals to the
// HTTP API (keeps internal/api decoupled from internal/server and internal/db).
type clientsAdapter struct {
	srv      sessionSnapshotter
	st       tunnelGetter
	services serviceModeGetter // optional; nil in unit tests
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
	return api.ClientView{
		SessionID: ss.SessionID, UserID: ss.UserID, TokenName: ss.Token,
		RemoteAddr: ss.RemoteAddr, OS: ss.OS, Arch: ss.Arch,
		ClientVersion: ss.ClientVersion, ServiceCount: n,
		TotalBytesIn: aggIn, TotalBytesOut: aggOut,
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

// runTrafficFlusher periodically persists the delta of each live tunnel's
// in-memory byte counters into tunnels.total_bytes_*, so traffic survives
// reconnects. WaitGroup-tracked + ctx-cancelled like runSessionReaper; a final
// flush runs on ctx cancellation before the DB is closed.
func runTrafficFlusher(ctx context.Context, wg *sync.WaitGroup, srv *server.Server, st *store.Store, log *slog.Logger, interval time.Duration) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		last := map[string][2]uint64{} // tunnelID -> {in,out} already persisted
		flush := func() {
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
				flush() // final flush before shutdown / DB close
				return
			case <-t.C:
				flush()
			}
		}
	}()
}

// sessionReaper is the type used to purge expired sessions.
// Using an interface makes the reaper testable without a real DB.
type sessionReaper interface {
	DeleteExpiredSessions(ctx context.Context) (int64, error)
}

// runSessionReaper starts a goroutine that calls reaper.DeleteExpiredSessions
// immediately and then every interval. It mirrors the byteTicker pattern in
// internal/server: the goroutine is tracked on wg and exits when ctx is done.
// Callers must wg.Wait() before closing the database.
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
		purge() // run once at startup
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				purge()
			}
		}
	}()
}

func main() {
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
			if n, err := st.BackfillAIProviders(context.Background()); err != nil {
				log.Warn("ai providers backfill failed", "err", err)
			} else if n > 0 {
				log.Info("ai providers created for existing api_key services", "count", n)
			}

			bus := events.NewBus()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
					Domains: splitAndTrim(cfg.ACMEDomain),
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

			srv, err := server.New(server.Options{
				Listen: cfg.Listen, TLSCert: cfg.TLSCert, TLSKey: cfg.TLSKey,
				PublicBind: cfg.PublicBind, PortMin: cfg.PortMin, PortMax: cfg.PortMax,
				Auth: st, Tunnels: tunnelStoreAdapter{st}, Events: bus, Logger: log,
				// v0.3.0: HTTP tunnel service identity + subdomain resolver.
				Services:   serviceResolverAdapter{db: db.Wrap(database)},
				AuthDomain: cfg.AuthDomain,
				// v0.6.0: when ACME is enabled the control channel serves the
				// ACME-managed certificate instead of TLSCert/TLSKey files.
				// acmeGetCert returns nil when acmeMgr is nil, so the server
				// falls back to file certs exactly as before (unchanged).
				GetCertificate: acmeGetCert(acmeMgr),
			})
			if err != nil {
				return err
			}

			// Start the session reaper: purges expired sessions once at startup
			// and then every hour. Mirrors the byteTicker goroutine pattern in
			// internal/server: WaitGroup-tracked, ctx-cancelled, stops before
			// database.Close() (enforced by LIFO defers above).
			runSessionReaper(ctx, &reaperWg, db.Wrap(database), log, time.Hour)

			// Traffic flusher: persists live byte counters into
			// tunnels.total_bytes_* every ~30s (and once at shutdown).
			// Tracked on reaperWg so its final flush runs before the
			// deferred database.Close() (LIFO defer ordering).
			runTrafficFlusher(ctx, &reaperWg, srv, st, log, 30*time.Second)

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

			// v0.4.0 Task 25: optional MCP listener — constructed only when
			// cfg.MCPListen is non-empty. The ToolStore adapter binds to the
			// live tunnel registry + audit query store + metrics recorder.
			v04.MCPServer = BuildMCPServer(cfg, st, v04, mcpTunnelAdapter{s: srv}, db.Wrap(database), log)

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
			proxyAuthDomain := cfg.AuthDomain
			if proxyAuthDomain == "" && acmeMgr != nil {
				if d := splitAndTrim(cfg.ACMEDomain); len(d) > 0 {
					proxyAuthDomain = d[0]
				}
			}

			accessChecker := proxy.NewAccessCheckerWithSessionsAndLogger(st, st, proxyAuthDomain, log)
			gate := proxy.NewGate(st, proxyAuthDomain, effectiveSecureCookies, log)
			// v0.5.0 F-13: wire the connection-log sink so the proxy
			// records one connection_logs row per closed request. The
			// adapter shim translates proxy.ConnLogEntry into the
			// concrete connlog.Entry — kept here (not in internal/proxy)
			// so the proxy package stays import-free of connlog.
			//
			// v0.5.1 P2.1 (Task 5): pass the store as the settings
			// reader so SQLSink.Rollup honours the
			// connection_logs.rollup_include_top_ips toggle. The
			// settings reader is only consulted on the daily rollup
			// compaction path, NOT the per-connection Record hot path.
			connLogSink := connlog.NewSQLSink(db.Wrap(database), log).WithSettings(st)
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
			}
			if ingressPort != "" {
				proxyOpts = append(proxyOpts, proxy.WithIngressPort(ingressPort))
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
			proxyHandler := proxy.New(
				proxyDialerAdapter{st: st, srv: srv},
				accessChecker,
				proxyAuthDomain,
				log,
				proxyOpts...,
			)
			if proxyTLSCfg != nil {
				// Wire GetConfigForClient so mTLS services get a per-vhost
				// TLS config with ClientCAs + RequireAndVerifyClientCert at
				// TLS-handshake time. Non-mTLS vhosts continue to use the
				// base config (no client cert required). Set here (after
				// proxy.New) so both the :8443 listener and any future TLS
				// consumer of proxyTLSCfg observe the hook.
				proxyTLSCfg.GetConfigForClient = proxyHandler.GetConfigForClient
			}

			apiSrv := &http.Server{
				Addr: httpListen,
				Handler: api.NewRouter(api.Deps{
					Users: st, Tunnels: tunnelListerAdapter{s: srv, access: st, services: st, slugs: st, authDomain: proxyAuthDomain}, Events: bus,
					Log: log, SecureCookies: effectiveSecureCookies, HTTPSEnabled: httpsEnabled,
					SPA: spaHandler, TrustedProxies: cfg.TrustedProxies,
					// Wire env override: 0 means use the api.LoginRateLimitPerIP constant.
					LoginRateLimitPerIPOverride: cfg.LoginRateLimitPerIP,
					Roles:                       st, Sessions: st, Settings: st,
					Clients: clientsAdapter{srv: srv, st: st, services: st}, AccessModes: st,
					DB: database,
					// v0.3.0: service API + live tunnel lookup + auth domain.
					Services:    st,
					LiveTunnels: liveTunnelLookupAdapter{srv: srv},
					// AuthDomain uses the same resolved value the proxy/access
					// checker/gate route on (cfg.AuthDomain, or the first ACME
					// domain when auth_domain is unset and ACME is on). In the
					// common case this equals cfg.AuthDomain, so composeHostname
					// and the burrow_login-409 check are unchanged.
					AuthDomain: proxyAuthDomain,
					// mTLS is only offered while the host-routed ingress runs.
					HostRouting: cfg.HTTPProxyListen != "",
					// feat/builtin-acme B3: single-origin /svc/{slug} tunnel routing.
					// proxyHandler is the hoisted host-routing proxy. The router
					// mounts /svc/{slug} + /svc/{slug}/* on it via ServicePathHandler only
					// when AuthDomain (above) is non-empty; the synthesized host
					// "<id>.<AuthDomain>" then matches the proxy's own routing
					// suffix exactly. When AuthDomain is "" the routes are skipped.
					TunnelProxy: proxyHandler,
					// The burrow_login gate, served at /__burrow/* on this origin so
					// path-routed services can redirect visitors to it.
					Gate: gate,
					// Control plane is on a different port from the API/dashboard
					// (e.g. compose maps :7000 control, :8080 dashboard). Surface
					// it via /clients/connect-info so the "Connect a client" wizard
					// prints a working `burrow connect --server …` command. (P1-2)
					ControlListen: cfg.Listen,
					// v0.4.0 Task 20: backup / restore wiring.
					BackupDir:      backupDir,
					BackupRunner:   backupRunnerAdapter{cfg: cfg},
					RestoreRunner:  restoreRunnerAdapter{cfg: cfg},
					RestoreTracker: restoreTracker,
					AuditAppender:  v04.AuditAppender,
					// v0.4.0 Task 25 wiring — every additive Deps surface.
					AuditEvents:       v04.AuditEvents,
					AuditChain:        v04.AuditChain,
					Metrics:           api.NewMetricsRecorderAdapter(v04.Metrics),
					Webhooks:          db.Wrap(database),
					WebhookDispatcher: v04.WebhookDispatcher,
					WebhookSecrets:    v04.WebhookSecrets,
					Bearer:            api.NewStoreBearerStore(st),
					Automation:        st,
					MCP: api.MCPInfo{
						Enabled: cfg.MCPListen != "",
						Listen:  cfg.MCPListen,
						Server:  v04.MCPServer,
					},
					RateLimitDB:       db.Wrap(database),
					RateLimits:        v04.QuotaEngine,
					Budgets:           db.Wrap(database),
					CostEngine:        v04.CostEngine,
					AIMetrics:         db.Wrap(database),
					CacheEngine:       v04.CacheEngine,
					CacheServices:     cacheServiceLookupAdapter{db: db.Wrap(database)},
					InspectorRings:    v04.InspectorMgr,
					InspectorServices: cacheServiceLookupAdapter{db: db.Wrap(database)},
					InspectorReplayer: newInspectorReplayer(v04.AIChain, log),
					ModelAliases:      db.Wrap(database),
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
				}),
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
			// The MCP listener (Task 25) adds 1 more when cfg.MCPListen is set.
			// The ACME :80 challenge+redirect listener adds 1 when ACME is on.
			senderCount := 2

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

			// v0.4.0 Task 25: optional MCP JSON-RPC listener (default :7800).
			// Same errc/Shutdown LIFO pattern as the :8443 proxy listener.
			var mcpSrv *http.Server
			if v04.MCPServer != nil {
				senderCount++
				mcpSrv = &http.Server{
					Addr:              cfg.MCPListen,
					Handler:           v04.MCPServer,
					ReadHeaderTimeout: 10 * time.Second,
				}
			}

			errc := make(chan error, senderCount)
			go func() { errc <- srv.Serve(ctx) }()
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
			if mcpSrv != nil {
				go func() {
					log.Info("mcp jsonrpc listening", "addr", cfg.MCPListen)
					if err := mcpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						errc <- err
						return
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
			// Shutdown order (reverse of start): mcp → proxy → api → control listener (srv).
			// The proxy is shut first so no new tunnel streams are opened while
			// the control listener is still draining. The MCP listener uses
			// the same in-process surfaces as the API, so it shuts first to
			// drain any in-flight automation calls before the proxy quiesces.
			// The ACME :80 challenge+redirect listener (nil unless ACME is on)
			// is shut alongside the api tier it fronts.
			shutCtx, cancel := context.WithTimeout(context.Background(), apiShutdownGrace)
			defer cancel()
			if mcpSrv != nil {
				_ = mcpSrv.Shutdown(shutCtx)
			}
			if proxySrv != nil {
				_ = proxySrv.Shutdown(shutCtx)
			}
			_ = apiSrv.Shutdown(shutCtx)
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

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
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
