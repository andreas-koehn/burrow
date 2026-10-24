package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

// foregroundRun marks a run started by one of the commands that came after
// `connect` (http, tcp, up). Those stop on a refusal that will not change and
// tell the person what the relay did with --slug and --access; `connect` runs
// without the mark and behaves as it always has.
type foregroundRun struct {
	// logsAsked: --log or one of the log variables asks for log lines, so
	// nothing but log lines is written.
	logsAsked bool
}

type foregroundKey struct{}

func foregroundContext(ctx context.Context, logsAsked bool) context.Context {
	return context.WithValue(ctx, foregroundKey{}, foregroundRun{logsAsked: logsAsked})
}

func foregroundFrom(ctx context.Context) (foregroundRun, bool) {
	fg, ok := ctx.Value(foregroundKey{}).(foregroundRun)
	return fg, ok
}

const (
	// msgSharedOrigin is the warning for an open http service: it is served
	// under the dashboard's own address.
	msgSharedOrigin = "This app shares the dashboard's origin; expose only apps you trust."
	// msgGRPCHostName is the note of an HTTP/2 service: gRPC clients cannot
	// use the /svc/<slug>/ path.
	msgGRPCHostName = "gRPC visitors need a host name of their own: add a custom domain to this service in the dashboard."
	// maxDashboardURL is the longest dashboard address that is repeated.
	maxDashboardURL = 300
)

// noObserver is the observer of a run without a status view.
type noObserver struct{}

func (noObserver) State(client.ConnState, string, time.Duration) {}
func (noObserver) Registered(client.RegisteredTunnel)            {}
func (noObserver) Connection(string, time.Time, string)          {}
func (noObserver) ConnectionClosed(string)                       {}
func (noObserver) Latency(time.Duration)                         {}
func (noObserver) LocalTarget(string, bool)                      {}
func (noObserver) Request(string, time.Time, string, string, int) {
}

// runNotes stands between the client and its observer and says, once per run,
// what a person has to know and no log line of the client says: that --slug
// or --access was not applied, that an open http app shares the dashboard's
// origin, that the relay is newer, that the relay cannot be reached.
type runNotes struct {
	client.Observer // what the calls are handed on to

	control string // the control endpoint, for the "cannot reach" line
	single  bool   // the run has one service: "This service" names it
	inView  bool   // the status view is on: its notes are shown too

	note        func(text string) // shows a note
	notice      func(text string) // shows the version notice; nil = not shown
	unreachable func(text string) // shows the "cannot reach" line; nil = not shown

	mu        sync.Mutex
	seen      map[string]bool
	connected bool // the relay was reached at least once
	told      bool // the "cannot reach" line was shown
}

func newRunNotes(next client.Observer, control string, tunnels int) *runNotes {
	return &runNotes{Observer: next, control: control, single: tunnels == 1, seen: map[string]bool{}, note: func(string) {}}
}

// once shows a note unless it was shown before in this run.
func (n *runNotes) once(text string) {
	if text == "" {
		return
	}
	n.mu.Lock()
	seen := n.seen[text]
	n.seen[text] = true
	n.mu.Unlock()
	if !seen {
		n.note(text)
	}
}

// State implements client.Observer.
func (n *runNotes) State(s client.ConnState, detail string, retryIn time.Duration) {
	n.Observer.State(s, detail, retryIn)
	n.mu.Lock()
	tell := false
	switch {
	case s == client.StateConnected:
		n.connected = true
	case s == client.StateReconnecting && !n.connected && !n.told && n.unreachable != nil && cannotReach(detail):
		n.told, tell = true, true
	}
	n.mu.Unlock()
	if tell {
		n.unreachable(relayUnreachable(n.control))
	}
}

// cannotReach reports whether a failed attempt never got to the relay. A
// certificate that is not trusted is a different matter with its own message.
func cannotReach(detail string) bool {
	return strings.HasPrefix(detail, "dial: ") &&
		!strings.Contains(detail, "x509") && !strings.Contains(detail, "certificate") && !strings.Contains(detail, "tls:")
}

// Registered implements client.Observer.
func (n *runNotes) Registered(t client.RegisteredTunnel) {
	n.Observer.Registered(t)
	if t.Ignored.Any() {
		n.once(ignoredNote(t, n.single))
	}
	if t.SlugUnacknowledged {
		n.once(slugUnacknowledgedNote(t, n.single))
	}
	if t.Created && t.AccessMode == "api_key" {
		n.once(apiKeyNote(t, n.single))
	}
	if n.inView && t.Type == "http" && t.AccessMode == "open" {
		n.once(msgSharedOrigin)
	}
	if n.inView && t.Type == "http" && (t.UpstreamProtocol == "h2c" || t.UpstreamProtocol == "h2") {
		n.once(msgGRPCHostName)
	}
	if t.Type == "tcp" && t.AccessMode != "" {
		if n.inView {
			// The view shows the address and the mode itself.
			n.once(tcpWarning(t, n.single))
		} else {
			n.once(tcpNote(t, relayHost(n.control), n.single))
		}
	}
}

// Refused implements client.RefusalObserver: one service is not served while
// the others are. Log lines already say so; the status view, which has none,
// shows it as a note.
func (n *runNotes) Refused(name string, err error) {
	if ro, ok := n.Observer.(client.RefusalObserver); ok {
		ro.Refused(name, err)
	}
	if n.inView {
		n.once(refusedNote(name, err))
	}
}

// RequestGRPC implements client.GRPCObserver for the observer behind it, with
// the grpc-status; an observer that does not take it gets a plain request.
func (n *runNotes) RequestGRPC(id string, at time.Time, method, path string, status int, grpc *int) {
	if g, ok := n.Observer.(client.GRPCObserver); ok {
		g.RequestGRPC(id, at, method, path, status, grpc)
		return
	}
	n.Request(id, at, method, path, status)
}

// Forward implements client.ForwardObserver for the observer behind it.
func (n *runNotes) Forward(s client.ForwardState) {
	if fo, ok := n.Observer.(client.ForwardObserver); ok {
		fo.Forward(s)
	}
}

// refusedNote says why one service of several is not served.
func refusedNote(name string, err error) string {
	var re *client.RefusedError
	var na *client.AccessNotAppliedError
	msg := ""
	switch {
	case errors.As(err, &na):
		msg = accessNotApplied(na)
	case errors.As(err, &re):
		if m, _, ok := refusal(re); ok {
			msg = m
		} else {
			msg = plainText(re.Message)
		}
	default:
		msg = plainText(err.Error())
	}
	return "Service " + plainText(name) + " is not served. " + msg
}

// sensitivePorts are local ports whose services are scanned for and attacked
// within minutes of being on a public port.
var sensitivePorts = map[string]string{
	"3389": "remote desktop",
	"22":   "SSH",
	"5900": "VNC",
	"5432": "PostgreSQL",
}

// tcpNote says how a tcp service can be reached: through burrow forward when
// it is private, at the relay's port otherwise (by its host name when it has
// no port: host name only), with a warning for a port
// that should not be public. What the relay says is repeated only in the form
// it must have.
func tcpNote(t client.RegisteredTunnel, host string, single bool) string {
	s := subject(t, single)
	_, port, _ := net.SplitHostPort(t.LocalAddr)
	var b strings.Builder
	switch t.AccessMode {
	case "private":
		target := t.Slug
		if !client.ValidSlug(target) {
			target = plainText(t.Name) // the owner's own name works too
		}
		hint := "burrow forward " + target
		if p, err := strconv.Atoi(port); err == nil && p > 0 && p+10000 <= 65535 {
			hint += " " + strconv.Itoa(p+10000)
		}
		b.WriteString(s + " is private: the relay opened no public port. Reach it from another machine with: " + hint)
	case "open", "ip":
		switch {
		case t.RemotePort <= 0 && t.AccessMode == "open":
			// Reached by host name only (TLS passthrough): no port of its own.
			b.WriteString(s + " is open to anyone by its host name on the relay's TLS port; it has no public port.")
		case t.RemotePort <= 0:
			b.WriteString(s + " is reachable by its host name on the relay's TLS port from the addresses allowed in the dashboard; it has no public port.")
		case t.AccessMode == "open":
			b.WriteString(s + " is open to anyone at " + net.JoinHostPort(plainText(host), strconv.Itoa(t.RemotePort)) + ".")
		default:
			b.WriteString(s + " is reachable at " + net.JoinHostPort(plainText(host), strconv.Itoa(t.RemotePort)) + " from the addresses allowed in the dashboard.")
		}
	default:
		return ""
	}
	if w := tcpWarning(t, single); w != "" {
		b.WriteString(" " + strings.TrimPrefix(w, s+": "))
	}
	return b.String()
}

// tcpWarning is what the status view adds to a tcp service: a port that
// should not be public, and a mode the dashboard keeps other than asked for.
func tcpWarning(t client.RegisteredTunnel, single bool) string {
	var parts []string
	_, port, _ := net.SplitHostPort(t.LocalAddr)
	if what, ok := sensitivePorts[port]; ok && (t.AccessMode == "open" || t.AccessMode == "ip") {
		parts = append(parts, "Port "+port+" ("+what+") on a public port is scanned and attacked within minutes: "+
			"use --access private and burrow forward, or keep it to known addresses with --access ip.")
	}
	// Started without --access: a service that exists keeps its mode, and
	// one less strict than the default a new one would have gets said.
	if t.Wished == "" && t.Default == "private" && (t.AccessMode == "open" || t.AccessMode == "ip") {
		parts = append(parts, "This service is "+t.AccessMode+" in the dashboard; use --access private or change it there.")
	}
	if t.Wished != "" && t.Wished != t.AccessMode {
		switch t.AccessMode {
		case "open", "ip", "private":
			parts = append(parts, "--access "+t.Wished+" was asked for; the dashboard keeps the service "+t.AccessMode+".")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return subject(t, single) + ": " + strings.Join(parts, " ")
}

// Session implements client.SessionObserver.
func (n *runNotes) Session(info client.SessionInfo) {
	if so, ok := n.Observer.(client.SessionObserver); ok {
		so.Session(info)
	}
	if !n.inView || n.notice == nil {
		return
	}
	// Only a release version is compared, and only its digits are shown.
	relay, ok := parseVersion(info.RelayVersion)
	if c, cmp := compareVersions(version.Version, info.RelayVersion); !ok || !cmp || c >= 0 {
		return
	}
	n.notice(fmt.Sprintf("The relay runs v%d.%d.%d. Run: burrow update", relay[0], relay[1], relay[2]))
}

// Counts implements client.CountObserver for the observer behind it.
func (n *runNotes) Counts(tunnelID string, open, total int) {
	if co, ok := n.Observer.(client.CountObserver); ok {
		co.Counts(tunnelID, open, total)
	}
}

// subject names the service a note is about.
func subject(t client.RegisteredTunnel, single bool) string {
	if name := plainText(t.Name); !single && name != "" {
		return "Service " + name
	}
	return "This service"
}

// ignoredNote says that the service existed and --slug and --access were not
// applied. Slug, access mode and address come from the relay; each is repeated
// only in the form it must have.
func ignoredNote(t client.RegisteredTunnel, single bool) string {
	if t.Type != "http" {
		// An older relay, asked for open: it opened the port as it always did.
		return subject(t, single) + ": this relay does not know access modes for tcp services, so --access was not applied; the port is open to anyone."
	}
	var has []string
	if slug := slugOf(t.URL); slug != "" {
		has = append(has, "slug "+slug)
	}
	switch t.AccessMode {
	case "open", "api_key", "burrow_login", "mtls":
		has = append(has, "access "+client.AccessName(t.AccessMode))
	}
	s := subject(t, single) + " already exists"
	if len(has) > 0 {
		s += " with " + strings.Join(has, " and ")
	}
	s += ". --slug and --access apply only when a service is created. Change them in the dashboard"
	if u := dashboardURL(t.DashboardURL); u != "" {
		return s + ": " + u
	}
	return s + "."
}

// apiKeyNote says that a service created with api-key access has no key yet.
// Keys are made in the dashboard, which shows each one once; none travels
// over the control connection.
func apiKeyNote(t client.RegisteredTunnel, single bool) string {
	s := subject(t, single) + " was created with api-key access and has no API key yet: every request is refused until one exists. Create one in the dashboard"
	if u := dashboardURL(t.DashboardURL); u != "" {
		return s + ": " + u
	}
	return s + "."
}

// slugUnacknowledgedNote says that the relay is older and never saw --slug.
func slugUnacknowledgedNote(t client.RegisteredTunnel, single bool) string {
	s := "This relay does not support --slug yet, so it was not applied. Change the slug in the dashboard."
	if !single {
		return subject(t, single) + ": this" + strings.TrimPrefix(s, "This")
	}
	return s
}

// slugOf returns the slug in the public address of an http service
// (https://<host>/svc/<slug>/), "" when the address is not of that form.
func slugOf(public string) string {
	u, err := url.Parse(public)
	if err != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Path, "/svc/")
	if !ok {
		return ""
	}
	slug := strings.TrimSuffix(rest, "/")
	if !client.ValidSlug(slug) {
		return ""
	}
	return slug
}

// dashboardURL returns u when it is a plain https address, "" otherwise.
func dashboardURL(u string) string {
	if len(u) > maxDashboardURL || !strings.HasPrefix(u, "https://") {
		return ""
	}
	for i := 0; i < len(u); i++ {
		if c := u[i]; c <= ' ' || c > '~' {
			return ""
		}
	}
	if p, err := url.Parse(u); err != nil || p.Host == "" || p.User != nil {
		return ""
	}
	return u
}
