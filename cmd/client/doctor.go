package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

// The names of the checks, in the order they run.
const (
	checkConfig    = "user config"
	checkResolve   = "relay name"
	checkDiscovery = "discovery"
	checkControl   = "control endpoint"
	checkToken     = "token"
	checkVersions  = "versions"
	checkClock     = "clock"
	checkLocal     = "local target"
)

const (
	doctorCheckTimeout = 5 * time.Second
	doctorTotalTimeout = 15 * time.Second
	maxClockSkew       = 2 * time.Minute
)

// checkResult is the outcome of one check. status is pass, warn, fail or skip.
// A skipped check says why in detail; it did not pass.
type checkResult struct {
	name   string
	status string
	detail string
	fix    string
	code   int // exit code a failure stands for; 0 means the general one
}

// doctorDeps is what the checks use from the outside, so that tests run them
// without a network, a terminal or the real user config.
type doctorDeps struct {
	resolve    func() (client.Credentials, error)
	configPath string // the user config, for the mode check
	lookup     func(ctx context.Context, host string) ([]string, error)
	http       *http.Client
	dialTCP    func(ctx context.Context, addr string) (net.Conn, error)
	// tlsHandshake runs the TLS handshake on conn as a client for serverName.
	tlsHandshake func(ctx context.Context, conn net.Conn, serverName string) error
	checkAuth    func(ctx context.Context, o client.Options) (client.AuthResult, error)
	// authOpts carries the TLS settings (--cacert, --server-name, --insecure).
	authOpts client.Options
	now      func() time.Time
	// services finds the burrow.yaml `up` would use: its path, its services
	// and whether there is one. err says the file is there and cannot be read
	// as a burrow.yaml; its text is not printed.
	services func() (path string, tunnels []client.TunnelSpec, found bool, err error)
	// report, when set, is given each result as soon as its check ends.
	report        func(checkResult)
	probe         func(ctx context.Context, addr string) bool
	clientVersion string
	timeout       time.Duration // one network check
	total         time.Duration // the whole run
}

type doctorRun struct {
	d      doctorDeps
	parent context.Context
	ctx    context.Context
	creds  client.Credentials
	out    []checkResult

	signedIn   bool
	resolved   bool
	reachable  bool
	disc       *client.Discovery
	discDate   time.Time
	relayVer   string
	authedHere bool
}

// runDoctor runs the eight checks in order. It always returns the whole list;
// a check that cannot run says so as skip.
func runDoctor(parent context.Context, d doctorDeps) []checkResult {
	if d.timeout <= 0 {
		d.timeout = doctorCheckTimeout
	}
	if d.total <= 0 {
		d.total = doctorTotalTimeout
	}
	ctx, cancel := context.WithTimeout(parent, d.total)
	defer cancel()
	r := &doctorRun{d: d, parent: parent, ctx: ctx}
	r.checkConfig()
	r.checkResolve()
	r.checkDiscovery()
	r.checkControl()
	r.checkToken()
	r.checkVersions()
	r.checkClock()
	r.checkLocal()
	return r.out
}

func (r *doctorRun) add(name, status, detail, fix string, code int) {
	res := checkResult{name: name, status: status, detail: r.clean(detail), fix: r.clean(fix), code: code}
	r.out = append(r.out, res)
	if r.d.report != nil {
		r.d.report(res)
	}
}

func (r *doctorRun) skip(name, why string) { r.add(name, "skip", why, "", 0) }

// clean keeps the token out of anything that is printed, whatever an error or
// the relay put into its text.
func (r *doctorRun) clean(s string) string {
	if t := r.creds.Token; t != "" {
		s = strings.ReplaceAll(s, t, "[token]")
	}
	return s
}

// stopped says why no more network check can run, "" while they can.
func (r *doctorRun) stopped() string {
	switch {
	case r.parent.Err() != nil:
		return "interrupted"
	case r.ctx.Err() != nil:
		return "the time limit for the whole run was reached"
	}
	return ""
}

// netCheck runs fn with the time limit of one check. It reports whether fn
// ran; when it did not, the check is already recorded as skipped.
func (r *doctorRun) netCheck(name string, fn func(ctx context.Context)) bool {
	if why := r.stopped(); why != "" {
		r.skip(name, why)
		return false
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.d.timeout)
	defer cancel()
	fn(ctx)
	return true
}

// failNet records a failed network step. A step cut short by Ctrl-C did not
// fail; it did not finish.
func (r *doctorRun) failNet(name string, err error, detail, fix string, code int) {
	if r.parent.Err() != nil {
		r.skip(name, "interrupted")
		return
	}
	r.add(name, "fail", detail, fix, code)
}

func (r *doctorRun) checkConfig() {
	creds, err := r.d.resolve()
	if err != nil {
		var mm *client.RelayMismatchError
		var tf *tokenFileError
		switch {
		case errors.As(err, &tf):
			// Not the stored sign-in: the variable names a file that is not there
			// or not readable. The path is the user's own; the error text is not printed.
			r.add(checkConfig, "fail", "the file that BURROW_TOKEN_FILE names cannot be read", "check the path in BURROW_TOKEN_FILE, or unset the variable", 0)
		case errors.As(err, &mm):
			login := "<relay>"
			if h, _, e := net.SplitHostPort(mm.Control); e == nil && h != "" {
				login = h
			}
			r.add(checkConfig, "fail", "not signed in to "+mm.Control+": the stored sign-in is for another relay", "burrow login "+login, exitNotSignedIn)
		case errors.Is(err, client.ErrNotSignedIn):
			r.add(checkConfig, "fail", "Not signed in", "burrow login <relay>", exitNotSignedIn)
		default:
			// The error may come from a file; its text is not printed.
			r.add(checkConfig, "fail", "the stored sign-in cannot be read", "check the file "+r.d.configPath, 0)
		}
		return
	}
	r.creds, r.signedIn = creds, true
	label := tokenLabel(creds.TokenName, creds.Token)
	detail := "signed in to " + creds.Control + " with token " + label + " (" + creds.Source + ")"
	if _, err := os.Stat(r.d.configPath); err == nil {
		if wide, err := client.FileModeTooWide(r.d.configPath); err == nil && wide {
			r.add(checkConfig, "warn", r.d.configPath+" can be read by other users", "chmod 600 "+r.d.configPath, 0)
			return
		}
	}
	r.add(checkConfig, "pass", detail, "", 0)
}

func (r *doctorRun) host() string {
	h, _, err := net.SplitHostPort(r.creds.Control)
	if err != nil {
		return r.creds.Control
	}
	return h
}

func (r *doctorRun) needSignIn(name string) bool {
	if !r.signedIn {
		r.skip(name, "needs a sign-in (see "+checkConfig+")")
		return false
	}
	return true
}

func (r *doctorRun) checkResolve() {
	if !r.needSignIn(checkResolve) {
		return
	}
	host := r.host()
	if net.ParseIP(host) != nil {
		r.resolved = true
		r.add(checkResolve, "pass", host+" is an IP address", "", 0)
		return
	}
	r.netCheck(checkResolve, func(ctx context.Context) {
		addrs, err := r.d.lookup(ctx, host)
		if err != nil || len(addrs) == 0 {
			detail := "cannot resolve " + host
			if ctx.Err() != nil {
				detail = "no answer resolving " + host
			}
			r.failNet(checkResolve, err, detail, "Check the relay address for typos and that this machine has a network connection", exitUnreachable)
			return
		}
		r.resolved = true
		r.add(checkResolve, "pass", host+" resolves to "+addrs[0], "", 0)
	})
}

// certProblem describes a certificate the client cannot accept.
func certProblem(err error) (string, bool) {
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	var cv *tls.CertificateVerificationError
	switch {
	case errors.As(err, &ua):
		issuer := "an unknown issuer"
		if ua.Cert != nil {
			if cn := ua.Cert.Issuer.CommonName; cn != "" {
				issuer = cn
			} else if dn := ua.Cert.Issuer.String(); dn != "" {
				issuer = dn // an issuer without a common name, by its full name
			}
		}
		return "the certificate is not trusted (issued by " + issuer + ")", true
	case errors.As(err, &he):
		return "the certificate is not valid for this name", true
	case errors.As(err, &ci):
		return "the certificate is not valid", true
	case errors.As(err, &cv):
		// Verification failed for a reason without a type of its own above.
		return "the certificate could not be verified", true
	}
	return "", false
}

const fixCacert = "If the relay uses its own CA, trust it with --cacert <ca.pem>"

func (r *doctorRun) checkDiscovery() {
	const name = checkDiscovery
	if !r.needSignIn(name) {
		return
	}
	if !r.resolved {
		r.skip(name, "the relay name does not resolve")
		return
	}
	base := strings.TrimRight(strings.TrimSpace(r.creds.Relay), "/")
	if base == "" {
		// No relay address is known: the control host is asked. An IPv6
		// address needs its brackets in a URL.
		h := r.host()
		if strings.Contains(h, ":") {
			h = "[" + h + "]"
		}
		base = "https://" + h
	}
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" || u.Host == "" {
		r.add(name, "fail", "the relay address is not an https address", "burrow login <relay>", 0)
		return
	}
	r.netCheck(name, func(ctx context.Context) {
		disc, err := client.Discover(ctx, r.d.http, base)
		r.discDate = disc.Date
		var se *client.DiscoveryStatusError
		switch {
		case err == nil:
			r.disc = &disc
			if !client.SameEndpoint(disc.Control, r.creds.Control) {
				r.add(name, "warn", "the relay names the control endpoint "+disc.Control+"; this machine uses "+r.creds.Control,
					"If the relay moved its control endpoint, sign in again: burrow login <relay>", 0)
				return
			}
			detail := base + " answers with a valid certificate"
			if r.d.authOpts.Insecure {
				detail = base + " answers; its certificate was not checked (--insecure)"
			}
			r.add(name, "pass", detail, "", 0)
		case errors.Is(err, client.ErrNoDiscovery):
			r.add(name, "warn", "this relay is older and has no discovery; using "+r.creds.Control, "", 0)
		case errors.Is(err, client.ErrNotARelay):
			r.add(name, "warn", "the answer of "+base+" is not a discovery document", "", 0)
		case errors.As(err, &se):
			r.add(name, "fail", base+" answered discovery with status "+strconv.Itoa(se.Status), "Check the relay address", exitUnreachable)
		default:
			if msg, ok := certProblem(err); ok {
				r.failNet(name, err, msg, fixCacert, exitUnreachable)
				return
			}
			detail := "cannot reach " + base + ": " + err.Error()
			if ctx.Err() != nil {
				detail = "no answer from " + base + " within " + r.d.timeout.String()
			}
			r.failNet(name, err, detail, "Check the relay address and your network connection", exitUnreachable)
		}
	})
}

func (r *doctorRun) checkControl() {
	const name = checkControl
	if !r.needSignIn(name) {
		return
	}
	if !r.resolved {
		r.skip(name, "the relay name does not resolve")
		return
	}
	ctl := r.creds.Control
	_, port, _ := net.SplitHostPort(ctl)
	r.netCheck(name, func(ctx context.Context) {
		conn, err := r.d.dialTCP(ctx, ctl)
		if err != nil {
			detail := "cannot connect to " + ctl
			if ctx.Err() != nil {
				detail = "no answer from " + ctl + " within " + r.d.timeout.String()
			}
			r.failNet(name, err, detail, "Check that port "+port+" is open on the relay's firewall", exitUnreachable)
			return
		}
		defer conn.Close()
		sn := r.d.authOpts.ServerName
		if sn == "" {
			sn = r.host()
		}
		if err := r.d.tlsHandshake(ctx, conn, sn); err != nil {
			if msg, ok := certProblem(err); ok {
				r.failNet(name, err, ctl+": "+msg, fixCacert, exitUnreachable)
				return
			}
			r.failNet(name, err, ctl+": the TLS handshake failed", "Check that "+ctl+" is a Burrow relay", exitUnreachable)
			return
		}
		r.reachable = true
		r.add(name, "pass", ctl+" accepts connections and the TLS handshake works", "", 0)
	})
}

func (r *doctorRun) checkToken() {
	const name = checkToken
	if !r.needSignIn(name) {
		return
	}
	if !r.reachable {
		r.skip(name, "the control endpoint is not reachable")
		return
	}
	r.netCheck(name, func(ctx context.Context) {
		o := r.d.authOpts
		o.Server, o.Token = r.creds.Control, r.creds.Token
		res, err := r.d.checkAuth(ctx, o)
		switch {
		case err != nil:
			if msg, ok := certProblem(err); ok {
				r.failNet(name, err, msg, fixCacert, exitUnreachable)
				return
			}
			r.failNet(name, err, "the relay could not be asked: "+err.Error(), "Run: burrow doctor again, or check the relay", exitUnreachable)
		case !res.OK:
			why := "the relay rejected the token"
			if res.Error != "" {
				why += " (" + res.Error + ")"
			}
			r.add(name, "fail", why, "burrow login <relay>", exitTokenRejected)
		default:
			r.relayVer = res.RelayVersion
			r.add(name, "pass", "the relay accepted token "+tokenLabel(r.creds.TokenName, r.creds.Token), "", 0)
		}
	})
}

// compareVersions compares two vMAJOR.MINOR.PATCH versions (the v is optional).
// ok is false when either is anything else, such as develop or a commit.
func compareVersions(a, b string) (cmp int, ok bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, true
		case pa[i] > pb[i]:
			return 1, true
		}
	}
	return 0, true
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" || len(p) > 9 {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p[0] == '+' || p[0] == '-' {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func (r *doctorRun) checkVersions() {
	const name = checkVersions
	if !r.needSignIn(name) {
		return
	}
	relay, min := r.relayVer, ""
	if r.disc != nil {
		relay, min = cmpOr(r.disc.Version, relay), r.disc.MinClientVersion
	}
	if relay == "" && min == "" {
		r.skip(name, "the relay did not report its version")
		return
	}
	me := r.d.clientVersion
	desc := "client " + me + ", relay " + cmpOr(relay, "unknown")
	if min != "" {
		if c, ok := compareVersions(me, min); ok && c < 0 {
			r.add(name, "fail", desc+"; the relay needs "+min+" or newer", "Run: burrow update", exitClientTooOld)
			return
		}
	}
	c, ok := compareVersions(me, relay)
	switch {
	case !ok:
		r.skip(name, "versions cannot be compared ("+desc+")")
	case c < 0:
		r.add(name, "warn", desc+"; the client is older than the relay", "Run: burrow update", 0)
	default:
		r.add(name, "pass", desc, "", 0)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (r *doctorRun) checkClock() {
	const name = checkClock
	if !r.needSignIn(name) {
		return
	}
	if r.discDate.IsZero() {
		r.skip(name, "the relay sent no Date header")
		return
	}
	local := r.d.now().UTC()
	relay := r.discDate.UTC()
	diff := local.Sub(relay)
	if diff < 0 {
		diff = -diff
	}
	const layout = "2006-01-02 15:04:05"
	if diff > maxClockSkew {
		r.add(name, "warn", "this machine says "+local.Format(layout)+" UTC, the relay says "+relay.Format(layout)+" UTC", "Turn on automatic time sync", 0)
		return
	}
	r.add(name, "pass", "within two minutes of the relay", "", 0)
}

func (r *doctorRun) checkLocal() {
	path, tunnels, found, err := r.d.services()
	if !found {
		r.skip(checkLocal, "no burrow.yaml found")
		return
	}
	if err != nil {
		// The parser's message can quote the file; it is not shown.
		r.add(checkLocal, "warn", path+" cannot be read as a burrow.yaml; its services were not checked",
			"Fix the file: `burrow up` reports the line", 0)
		return
	}
	if len(tunnels) == 0 {
		r.skip(checkLocal, "no services in "+path)
		return
	}
	for _, t := range tunnels {
		label := t.Name + " → " + t.LocalAddr
		r.netCheck(checkLocal, func(ctx context.Context) {
			if r.d.probe(ctx, t.LocalAddr) {
				r.add(checkLocal, "pass", label+" accepts connections", "", 0)
				return
			}
			if why := r.stopped(); why != "" {
				r.skip(checkLocal, label+": "+why)
				return
			}
			if ctx.Err() != nil {
				r.skip(checkLocal, label+": no answer within "+r.d.timeout.String())
				return
			}
			r.add(checkLocal, "warn", "nothing is listening on "+t.LocalAddr+" ("+t.Name+")", "Start the service, or fix `local` in "+path, 0)
		})
	}
}

// exitCodeOf is the exit code for a run: 0 when nothing failed, otherwise the
// code of the first failure.
func exitCodeOf(rs []checkResult) int {
	for _, r := range rs {
		if r.status == "fail" {
			if r.code != 0 {
				return r.code
			}
			return exitGeneral
		}
	}
	return 0
}

// printDoctor writes one line per check, and the fix under a warning or failure.
func printDoctor(w io.Writer, rs []checkResult, colour bool) {
	marks := map[string]struct{ text, colour string }{
		"pass": {"ok", "32"}, "warn": {"warn", "33"}, "fail": {"FAIL", "31"}, "skip": {"skip", "2"},
	}
	for _, r := range rs {
		m := marks[r.status]
		mark := fmt.Sprintf("%-4s", m.text)
		if colour {
			mark = "\x1b[" + m.colour + "m" + mark + "\x1b[0m"
		}
		fmt.Fprintf(w, "%s  %s  %s\n", mark, r.name, r.detail)
		if (r.status == "warn" || r.status == "fail") && r.fix != "" {
			fmt.Fprintf(w, "  → %s\n", r.fix)
		}
	}
}

// doctorExit is how a run ends: nil when nothing failed and every check had
// its turn, otherwise an error with the exit code. A run cut short by Ctrl-C
// did not find the machine in order, so it does not end with 0.
func doctorExit(rs []checkResult, interrupted bool) error {
	if code := exitCodeOf(rs); code != 0 {
		return &exitError{code: code, msg: "burrow doctor found problems; the lines marked FAIL say how to fix them."}
	}
	if interrupted {
		return &exitError{code: exitGeneral, msg: "Interrupted: not every check ran."}
	}
	return nil
}

// newDoctorDeps wires the checks to the network.
func newDoctorDeps(d deps, g globalFlags, userPath string) (doctorDeps, error) {
	pool, err := loadRootCAs(g.cacert)
	if err != nil {
		return doctorDeps{}, err
	}
	hc, err := relayHTTPClient(g)
	if err != nil {
		return doctorDeps{}, err
	}
	tlsCfg := func(serverName string) *tls.Config {
		return &tls.Config{InsecureSkipVerify: g.insecure, RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12} //nolint:gosec // dev-only opt-in
	}
	var fc *client.FileConfig
	var svcPath string
	var svcErr error
	if p, err := findServiceFile(d, ""); err == nil {
		svcPath = p
		if f, err := client.LoadFileConfig(p); err == nil {
			fc = &f
		} else {
			svcErr = err
		}
	}
	return doctorDeps{
		resolve:    func() (client.Credentials, error) { return resolveCredentials(d, userPath, fc) },
		configPath: userPath,
		lookup:     net.DefaultResolver.LookupHost,
		http:       hc, // client.Discover follows no redirect, whatever the client
		dialTCP: func(ctx context.Context, addr string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "tcp", addr)
		},
		tlsHandshake: func(ctx context.Context, conn net.Conn, serverName string) error {
			return tls.Client(conn, tlsCfg(serverName)).HandshakeContext(ctx)
		},
		checkAuth: client.CheckAuth,
		authOpts:  client.Options{Insecure: g.insecure, RootCAs: pool, ServerName: g.serverName},
		now:       time.Now,
		services: func() (string, []client.TunnelSpec, bool, error) {
			if svcPath == "" {
				return "", nil, false, nil
			}
			if fc == nil {
				return svcPath, nil, true, svcErr
			}
			return svcPath, fc.Tunnels, true, nil
		},
		probe: func(ctx context.Context, addr string) bool {
			var nd net.Dialer
			c, err := nd.DialContext(ctx, "tcp", addr)
			if err != nil {
				return false
			}
			_ = c.Close()
			return true
		},
		clientVersion: version.Version,
	}, nil
}

// newDoctorCmd builds `burrow doctor`.
func newDoctorCmd(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the relay, the sign-in, versions and local targets",
		Long: "Run these checks in order and print one line for each:\n" +
			"  user config, relay name, discovery, control endpoint, token, versions, clock,\n" +
			"  and the local target of each service in burrow.yaml.\n\n" +
			"The token is never printed. Exit code 0 when nothing failed; otherwise the code of the\n" +
			"first failure: 3 not signed in, 4 token rejected, 5 relay unreachable or certificate\n" +
			"not trusted, 6 client too old, 1 anything else.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			g, err := readGlobals(cmd, d)
			if err != nil {
				return err
			}
			userPath, err := userConfigFile(cmd, d)
			if err != nil {
				return err
			}
			dd, err := newDoctorDeps(d, g, userPath)
			if err != nil {
				return err
			}
			// Each line appears when its check ends: a run that hangs on one
			// check, or is cut short, has shown the ones before it.
			out := cmd.OutOrStdout()
			colour := d.viewTerminal != nil && d.viewTerminal() && d.getenv("NO_COLOR") == ""
			dd.report = func(c checkResult) { printDoctor(out, []checkResult{c}, colour) }
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			rs := runDoctor(ctx, dd)
			return doctorExit(rs, ctx.Err() != nil)
		},
	}
}
