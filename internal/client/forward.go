package client

// forward.go — `burrow forward` (native tcp, P05): a local listener per
// ForwardSpec, and one forward stream through the relay per local connection
// (proto.MsgForwardOpen). The listeners are opened when Run starts and closed
// when it ends: a lost control connection does not close them. While there is
// no session a local connection is accepted and closed at once, so that a
// program such as mstsc fails fast instead of waiting for the relay.

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/bridge"
	"github.com/ankoehn/burrow/internal/proto"
)

// ForwardSpec is one local listener of `burrow forward` or of the forwards:
// section of burrow.yaml.
type ForwardSpec struct {
	// Target names the tcp service: its slug, the name of one of the user's
	// own tcp services, or its service id.
	Target string
	// Addr is where the listener is opened, host:port.
	Addr string
}

// The defaults of a forward: RDP's 3389 is often taken by the machine's own
// remote desktop server, so the local port is another one, and the listener
// is reachable from this machine alone.
const (
	DefaultForwardPort = 13389
	DefaultForwardBind = "127.0.0.1"
)

const (
	// maxForwardTarget is the longest target the relay takes.
	maxForwardTarget = 200
	// forwardAnswerWait bounds the wait for the relay's answer to an open. The
	// relay itself gives up after 5 s of authorization and 5 s of pairing.
	forwardAnswerWait = 15 * time.Second
)

// Seams for the tests: how a listener is opened, and how long local
// connections are closed without asking the relay after it said rate_limited.
var (
	listenTCP       = net.Listen
	forwardCooldown = 5 * time.Second
)

// ForwardTargetRule is the rule of a target, as the messages state it.
const ForwardTargetRule = "must be 1 to 200 characters without spaces: the slug of a tcp service, the name of one of yours, or its id"

// ValidForwardTarget reports whether s can name a tcp service to forward to:
// 1 to 200 printable ASCII characters without spaces.
func ValidForwardTarget(s string) bool {
	if s == "" || len(s) > maxForwardTarget {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// ForwardAddr builds the listen address of a forward from a bind address (an
// IP address or "localhost"; "" is DefaultForwardBind) and a port (1 to
// 65535; 0 is DefaultForwardPort).
func ForwardAddr(bind string, port int) (string, error) {
	bind = strings.TrimSpace(bind)
	if bind == "" {
		bind = DefaultForwardBind
	}
	if port == 0 {
		port = DefaultForwardPort
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("the local port must be between 1 and 65535")
	}
	if bind != "localhost" && net.ParseIP(strings.Trim(bind, "[]")) == nil {
		return "", fmt.Errorf("the bind address must be an IP address or localhost, such as 127.0.0.1")
	}
	return net.JoinHostPort(strings.Trim(bind, "[]"), strconv.Itoa(port)), nil
}

// LoopbackAddr reports whether a listen address is reachable from this
// machine alone.
func LoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ForwardState is what a ForwardObserver is told of one forward.
type ForwardState struct {
	Index        int // in Options.Forwards
	Target, Addr string
	// Connected: there is a session, so a local connection is forwarded.
	Connected bool
	// Open and Total count the forwarded connections: now, and since the
	// run started.
	Open, Total int
	// Problem says why the last local connection was not forwarded, or why
	// the forward stopped; "" after one went through.
	Problem string
	// Stopped: the forward has ended for good and its listener is closed.
	Stopped bool
}

// ForwardObserver is an Observer that is also told of the forwards. It is
// optional: an Observer without the method is not told.
type ForwardObserver interface {
	Forward(s ForwardState)
}

// RefusalObserver is an Observer that is also told when one tunnel is not
// served while the others are: err is a *RefusedError or an
// *AccessNotAppliedError. It is optional.
type RefusalObserver interface {
	Refused(name string, err error)
}

// The problems of a forward that are the client's own words.
const (
	msgRelayTooOld  = "relay too old: it cannot forward to tcp services; update the relay"
	msgNotConnected = "not connected to the relay; local connections are closed until it is back"
)

// ForwardUnsupportedError ends a forward when the relay announces no forward
// streams (proto.CapForward).
type ForwardUnsupportedError struct{ Target string }

func (e *ForwardUnsupportedError) Error() string {
	return "relay too old: it cannot forward to tcp services (burrow forward " + e.Target + "); update the relay"
}

// ForwardRefusedError ends a forward when the relay's answer will not change:
// no tcp service of that name that the user may reach (proto.CodeNotFound).
type ForwardRefusedError struct{ Target, Code, Message string }

func (e *ForwardRefusedError) Error() string {
	return "forward " + e.Target + ": " + e.Message
}

// forwarder is the listener of one ForwardSpec.
type forwarder struct {
	c    *Client
	i    int
	spec ForwardSpec
	ln   net.Listener

	mu          sync.Mutex
	open, total int
	problem     string
	stopped     bool
	conns       map[net.Conn]struct{} // local connections being forwarded
	// holdUntil: the relay said rate_limited; until then local connections
	// are closed without opening a stream, which would only count against
	// the session's limit again.
	holdUntil time.Time
}

// listenForwards opens the listener of every forward. One that cannot be
// opened closes the others and ends Run before anything is sent.
func (c *Client) listenForwards() error {
	c.fwdEnd = make(chan struct{})
	for i, spec := range c.opts.Forwards {
		ln, err := listenTCP("tcp", spec.Addr)
		if err != nil {
			c.closeForwards()
			return listenError(spec, err)
		}
		f := &forwarder{c: c, i: i, spec: spec, ln: ln, conns: map[net.Conn]struct{}{}}
		f.spec.Addr = ln.Addr().String()
		c.fwds = append(c.fwds, f)
	}
	c.fwdLive = len(c.fwds)
	for _, f := range c.fwds {
		c.log.Info("forward listening", "target", f.spec.Target, "addr", f.spec.Addr)
		f.tell()
		c.fwdWG.Go(f.accept)
	}
	return nil
}

// listenError says why a forward's listener could not be opened; most often
// its local port is taken.
func listenError(spec ForwardSpec, err error) error {
	_, port, _ := net.SplitHostPort(spec.Addr)
	if addrInUse(err) {
		return fmt.Errorf("forward %s: local port %s is in use; pick another local port: burrow forward %s <local-port>, or local_port in burrow.yaml: %w",
			spec.Target, port, spec.Target, err)
	}
	return fmt.Errorf("forward %s: cannot listen on %s: %w", spec.Target, spec.Addr, err)
}

// addrInUse reports whether a listen failed because the address is taken.
// Windows reports WSAEADDRINUSE (10048), which is not syscall.EADDRINUSE there.
func addrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == 10048 {
		return true
	}
	return strings.Contains(err.Error(), "address already in use")
}

// closeForwards closes every listener and every forwarded connection, and
// waits for them.
func (c *Client) closeForwards() {
	for _, f := range c.fwds {
		_ = f.ln.Close()
		f.mu.Lock()
		for conn := range f.conns {
			_ = conn.Close()
		}
		f.mu.Unlock()
	}
	c.fwdWG.Wait()
}

// setForwardSession makes sess the session local connections are forwarded
// on; nil while there is none.
func (c *Client) setForwardSession(sess *yamux.Session) {
	c.mu.Lock()
	c.fsess = sess
	c.mu.Unlock()
	for _, f := range c.fwds {
		if sess != nil {
			f.mu.Lock()
			if f.problem == msgNotConnected {
				f.problem = "" // that is over
			}
			f.mu.Unlock()
		}
		f.tell()
	}
}

func (c *Client) forwardSession() *yamux.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fsess
}

// forwardsEnded is closed when every forward has stopped and the run has no
// tunnel; fwdErr is then why the last one stopped.
func (c *Client) forwardsEnded() <-chan struct{} { return c.fwdEnd }

// stopForwards stops every forward that still runs, for the same reason.
func (c *Client) stopForwards(problem string, err func(f *forwarder) error) {
	for _, f := range c.fwds {
		f.stop(problem, err(f))
	}
}

// state is what the observer is told of f.
func (f *forwarder) state() ForwardState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ForwardState{
		Index: f.i, Target: f.spec.Target, Addr: f.spec.Addr,
		Connected: f.c.forwardSession() != nil && !f.stopped,
		Open:      f.open, Total: f.total, Problem: f.problem, Stopped: f.stopped,
	}
}

func (f *forwarder) tell() {
	if f.c.events == nil {
		return
	}
	s := f.state()
	f.c.events.emit(func(o Observer) {
		if fo, ok := o.(ForwardObserver); ok {
			fo.Forward(s)
		}
	})
}

func (f *forwarder) setProblem(p string) {
	f.mu.Lock()
	f.problem = p
	f.mu.Unlock()
	f.tell()
}

// stop ends the forward for good: its listener closes; connections already
// forwarded keep running. The last forward to stop in a run without tunnels
// ends the run with err.
func (f *forwarder) stop(problem string, err error) {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return
	}
	f.stopped, f.problem = true, problem
	f.mu.Unlock()
	_ = f.ln.Close()
	f.c.log.Error("forward stopped", "target", f.spec.Target, "addr", f.spec.Addr, "reason", problem)
	f.tell()

	c := f.c
	c.mu.Lock()
	c.fwdLive--
	end := c.fwdLive == 0 && len(c.opts.Tunnels) == 0
	if end {
		c.fwdErr = err
	}
	c.mu.Unlock()
	if end {
		close(c.fwdEnd)
	}
}

func (f *forwarder) accept() {
	var delay time.Duration // as the relay's accept loops: 5 ms doubling up to 1 s
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // stopped, or the run ended
			}
			// Out of descriptors, say: the listener stays and tries again.
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			f.c.log.Warn("forward: accept failed; trying again", "target", f.spec.Target, "err", err, "retry_in", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		f.mu.Lock()
		wait := time.Until(f.holdUntil)
		f.mu.Unlock()
		if wait > 0 {
			_ = conn.Close()
			f.setProblem(fmt.Sprintf("waiting %d s after the relay's limit on forwards; the local connection was closed", int((wait+time.Second-1)/time.Second)))
			continue
		}
		sess := f.c.forwardSession()
		if sess == nil {
			// Not queued: the relay may be away for minutes.
			_ = conn.Close()
			f.c.log.Warn("forward: not connected to the relay; the local connection was closed", "target", f.spec.Target)
			f.setProblem(msgNotConnected)
			continue
		}
		f.mu.Lock()
		f.conns[conn] = struct{}{}
		f.mu.Unlock()
		f.c.fwdWG.Go(func() {
			defer func() {
				_ = conn.Close()
				f.mu.Lock()
				delete(f.conns, conn)
				f.mu.Unlock()
			}()
			f.forward(sess, conn)
		})
	}
}

// forward carries one local connection through a stream of its own.
func (f *forwarder) forward(sess *yamux.Session, local net.Conn) {
	st, err := sess.OpenStream()
	if err != nil {
		f.setProblem(msgNotConnected)
		return
	}
	defer st.Close()
	// The relay reads the frame within 10 s and answers within about 10 s.
	_ = st.SetDeadline(time.Now().Add(forwardAnswerWait))
	if err := proto.WriteMessage(st, proto.MsgForwardOpen, proto.ForwardOpen{Target: f.spec.Target}); err != nil {
		f.setProblem("the relay did not take the forward: " + err.Error())
		return
	}
	var env proto.Envelope
	var resp proto.ForwardOpenResponse
	if err := proto.ReadFrame(st, &env); err != nil || env.Type != proto.MsgForwardOpenResp || proto.DecodePayload(env, &resp) != nil {
		// A relay that does not know forwards closes the stream unanswered;
		// one that announced them should not.
		f.c.log.Warn("forward: the relay closed the stream without an answer", "target", f.spec.Target, "err", err)
		f.setProblem("the relay closed the forward without an answer")
		return
	}
	_ = st.SetDeadline(time.Time{})
	if !resp.OK {
		f.refused(resp)
		return
	}

	f.mu.Lock()
	f.open++
	f.total++
	f.problem = ""
	f.mu.Unlock()
	f.tell()
	var in, out atomicCounter
	bridge.Pipe(local, st, &in.v, &out.v)
	f.mu.Lock()
	f.open--
	f.mu.Unlock()
	f.tell()
}

// refused tells what the relay said. Only not_found ends the forward: no such
// service, or one this user may not reach (the relay does not say which).
func (f *forwarder) refused(resp proto.ForwardOpenResponse) {
	t := f.spec.Target
	var p string
	switch resp.Code {
	case proto.CodeNotFound:
		p = fmt.Sprintf("tcp service %q not found, or you may not reach it; check the name (the slug the dashboard shows) and ask its owner to allow your role", t)
		f.stop(p, &ForwardRefusedError{Target: t, Code: resp.Code, Message: p})
		return
	case proto.CodeOffline:
		p = fmt.Sprintf("%s is offline: no client serves it right now; try again when its machine is connected", t)
	case proto.CodeRateLimited:
		p = "the relay limits forwards (too many connections at once or in a minute); wait a moment and connect again"
		f.mu.Lock()
		f.holdUntil = time.Now().Add(forwardCooldown)
		f.mu.Unlock()
	case proto.CodeInternal:
		p = "the relay could not look the service up; try again"
	default:
		p = "the relay refused the forward"
		if resp.Code != "" {
			p += " (" + resp.Code + ")"
		}
	}
	f.c.log.Warn("forward refused", "target", t, "code", resp.Code, "err", resp.Error)
	f.setProblem(p)
}
