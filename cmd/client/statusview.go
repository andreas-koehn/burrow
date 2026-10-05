package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ankoehn/burrow/cmd/client/view"
	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

// terminal is where the status view is drawn.
type terminal struct {
	w       io.Writer
	size    func() (cols, rows int) // 0 = not known
	colour  bool
	restore func() // undoes what opening it changed
}

const (
	// probeInterval is how often a local target is tried while no visitor
	// connection has reached it.
	probeInterval = 5 * time.Second
	// viewPoll is how often the view looks for a new terminal size and
	// advances the countdown to the next connection attempt.
	viewPoll = 500 * time.Millisecond
	// viewGap is the shortest time between two renderings.
	viewGap = 250 * time.Millisecond
)

// Seams for the tests.
var (
	// stdoutShowsView reports whether stdout is a terminal that can show the
	// status view.
	stdoutShowsView = func() bool {
		return view.IsTerminal(os.Stdout) && os.Getenv("TERM") != "dumb"
	}
	// openTerminal prepares stdout for the status view. ok is false when it
	// cannot show one; the client then prints log lines.
	openTerminal = func() (terminal, bool) {
		restore, ok := view.EnableANSI(os.Stdout)
		if !ok {
			return terminal{}, false
		}
		return terminal{
			w:       os.Stdout,
			size:    func() (int, int) { return view.Size(os.Stdout) },
			colour:  view.Colour(os.Stdout, os.Getenv),
			restore: restore,
		}, true
	}
	startProbe = client.Probe
	// viewCloseWait is how long the command waits for the last drawing when
	// it ends. A terminal that takes no output must not keep it from ending.
	viewCloseWait           = time.Second
	viewErr       io.Writer = os.Stderr
)

// probeGate stands between the view's store and its two sources for "does
// the local service answer": the probes, and the client once a visitor
// connection has tried. The first visitor connection that reaches a target
// ends its probe, and from then on only the client is believed.
type probeGate struct {
	*view.Store

	mu    sync.Mutex
	stops map[string]context.CancelFunc // local address → ends its probe
}

// LocalTarget is what the client reports (client.Observer).
func (g *probeGate) LocalTarget(addr string, reachable bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if stop, ok := g.stops[addr]; ok && reachable {
		stop()
		delete(g.stops, addr)
	}
	g.Store.LocalTarget(addr, reachable)
}

// fromProbe is what a probe reports. A probe that has been ended may still
// deliver a result it had under way; that one is dropped.
func (g *probeGate) fromProbe(addr string, reachable bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.stops[addr]; ok {
		g.Store.LocalTarget(addr, reachable)
	}
}

// relayHost returns the host of a control endpoint, for display and as the
// host of public tcp ports.
func relayHost(control string) string {
	if h, _, err := net.SplitHostPort(control); err == nil {
		return h
	}
	return control
}

// runWithView runs the client with the status view on t instead of log lines.
// release gives Ctrl-C back to the system once the client has stopped.
//
// Nothing the client waits for touches the terminal: the client hands its
// events to a queue that drops rather than blocks (client.Observer), the store
// behind it only changes memory, and one goroutine of the view reads the store
// and writes. The token is not passed to anything here but the client.
func runWithView(ctx context.Context, release func(), o client.Options, t terminal) error {
	defer t.restore()

	store := view.NewStore(relayHost(o.Server), version.Version, o.Tunnels)
	gate := &probeGate{Store: store, stops: map[string]context.CancelFunc{}}
	o.Observer = gate
	// The view shows what the log lines would say; an error that ends the
	// command is printed after the view has closed.
	o.Logger = slog.New(slog.DiscardHandler)

	probeCtx, stopProbes := context.WithCancel(ctx)
	var probing sync.WaitGroup
	gate.mu.Lock()
	for _, tn := range o.Tunnels {
		addr := tn.LocalAddr
		if _, ok := gate.stops[addr]; ok || addr == "" {
			continue
		}
		one, stop := context.WithCancel(probeCtx)
		gate.stops[addr] = stop
		probing.Go(func() { startProbe(one, []string{addr}, probeInterval, gate.fromProbe) })
	}
	gate.mu.Unlock()

	stopView := make(chan struct{})
	viewDone := make(chan struct{})
	go drawLoop(store, t, stopView, viewDone)

	err := startClient(ctx, o)

	release()
	stopProbes()
	probing.Wait()
	close(stopView)
	wait := time.NewTimer(viewCloseWait)
	defer wait.Stop()
	select {
	case <-viewDone:
	case <-wait.C:
	}
	return err
}

// drawLoop redraws the view when the store changes, at most four times a
// second, and once more when stop is closed, so that the final state stays on
// the terminal.
func drawLoop(store *view.Store, t terminal, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			// The tunnels are worth more than their display.
			fmt.Fprintln(viewErr, "burrow: the status view stopped and the tunnels keep running:", r)
		}
	}()
	screen := view.NewScreen(t.w, t.size)
	draw := func() {
		cols, rows := t.size()
		screen.Draw(view.Fit(store.Snapshot(), cols, rows, t.colour))
	}
	finish := func() {
		draw()
		screen.Close()
	}
	poll := time.NewTicker(viewPoll)
	defer poll.Stop()
	gap := time.NewTimer(viewGap)
	defer gap.Stop()
	for {
		draw()
		gap.Reset(viewGap)
		select {
		case <-stop:
			finish()
			return
		case <-gap.C:
		}
		select {
		case <-stop:
			finish()
			return
		case <-store.Changed():
		case <-poll.C: // a resize, or a second less to wait
		}
	}
}
