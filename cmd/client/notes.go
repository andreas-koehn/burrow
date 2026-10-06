package main

import (
	"context"
	"fmt"
	"net/url"
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
		n.unreachable(fmt.Sprintf(msgRelayUnreachable, n.control))
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
		return subject(t, single) + " is a tcp service: --slug and --access apply to http services and were not applied."
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
