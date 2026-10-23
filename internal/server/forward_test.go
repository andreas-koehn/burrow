package server

// Forward streams (native tcp, P04): a signed-in client reaches a tcp
// service through the relay, whatever its mode; only the owner and the roles
// the service allows get through, and every stream is limited, logged and
// told.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/ankoehn/burrow/internal/observe"
	"github.com/ankoehn/burrow/internal/proto"
)

// tokenUsers is an authenticator whose tokens are users: "secret" is u1 (the
// owner of every service of the tcpResolver), "other" u2, "friend" u3. The
// token's name is "<user>-laptop".
type tokenUsers struct{}

func (tokenUsers) Authenticate(ctx context.Context, tok string) (string, error) {
	u, _, _, err := tokenUsers{}.AuthenticateToken(ctx, tok)
	return u, err
}

func (tokenUsers) AuthenticateToken(_ context.Context, tok string) (string, string, string, error) {
	u, ok := map[string]string{"secret": "u1", "other": "u2", "friend": "u3"}[tok]
	if !ok {
		return "", "", "", fmt.Errorf("unknown token: %w", ErrTokenInvalid)
	}
	return u, "tok-" + u, u + "-laptop", nil
}

// fakeForwards resolves a target by service name (as the tcpResolver names
// services: svc-<name>) and lets the owner u1 and the users in allowed
// through.
type fakeForwards struct {
	mu      sync.Mutex
	res     *tcpResolver
	allowed map[string]bool
	err     error
	calls   int
}

func (f *fakeForwards) AuthorizeForward(_ context.Context, userID, target string) (ForwardTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return ForwardTarget{}, f.err
	}
	f.res.mu.Lock()
	id, ok := f.res.ids[target]
	f.res.mu.Unlock()
	if !ok {
		return ForwardTarget{}, ErrForwardNotFound
	}
	t := ForwardTarget{ServiceID: id, Name: target, Owner: userID == "u1"}
	if userID != "u1" && !f.allowed[userID] {
		return t, ErrForwardDenied
	}
	return t, nil
}

func (f *fakeForwards) allow(userID string) {
	f.mu.Lock()
	f.allowed[userID] = true
	f.mu.Unlock()
}

// forwardRelay is an accessRelay that takes forward streams; its home
// client greets every connection with "hi " and then echoes it.
type forwardRelay struct {
	*accessRelay
	fwd  *fakeForwards
	home *handClient
}

func newForwardRelay(t *testing.T, port int, mut func(*Options)) *forwardRelay {
	t.Helper()
	fwd := &fakeForwards{allowed: map[string]bool{}}
	res, access, log := newTCPResolver(), newFakeTCPAccess(), &recConnLog{}
	fwd.res = res
	r := tcpRelay(t, port, port, res, func(o *Options) {
		o.TCPAccess, o.ConnLog, o.Auth, o.Forwards = access, log, tokenUsers{}, fwd
		if mut != nil {
			mut(o)
		}
	})
	told := &atomic.Int32{}
	home := r.client(func(st *yamux.Stream) {
		told.Add(1)
		_, _ = st.Write([]byte("hi "))
		_, _ = io.Copy(st, st)
	})
	a := &accessRelay{observedRelay: r, res: res, access: access, log: log, c: home, told: told}
	return &forwardRelay{accessRelay: a, fwd: fwd, home: home}
}

// forwarder is a signed-in client that holds no tunnel: it opens its
// control stream, as every client does, and then forward streams.
type forwarder struct {
	conn net.Conn
	sess *yamux.Session
}

func (r *observedRelay) forwarder(token string) *forwarder {
	r.t.Helper()
	conn, ar := r.dial(token, "")
	if !ar.OK {
		r.t.Fatalf("auth: %+v", ar)
	}
	sess, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := sess.OpenStream(); err != nil { // the control stream
		r.t.Fatal(err)
	}
	f := &forwarder{conn: conn, sess: sess}
	r.t.Cleanup(func() { _ = sess.Close(); _ = conn.Close() })
	return f
}

// open opens a forward stream to target and reads the answer. err is the
// read error when the relay closed the stream without one.
func (f *forwarder) open(t *testing.T, target string) (*yamux.Stream, proto.ForwardOpenResponse, error) {
	t.Helper()
	st, err := f.sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteMessage(st, proto.MsgForwardOpen, proto.ForwardOpen{Target: target}); err != nil {
		t.Fatal(err)
	}
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	var env proto.Envelope
	if err := proto.ReadFrame(st, &env); err != nil {
		return st, proto.ForwardOpenResponse{}, err
	}
	_ = st.SetReadDeadline(time.Time{})
	if env.Type != proto.MsgForwardOpenResp {
		t.Fatalf("answer of type %s", env.Type)
	}
	var resp proto.ForwardOpenResponse
	if err := proto.DecodePayload(env, &resp); err != nil {
		t.Fatal(err)
	}
	return st, resp, nil
}

// rows are the connection-log rows of kind.
func (a *accessRelay) rows(kind string) []ConnLogEntry {
	var out []ConnLogEntry
	for _, e := range a.log.all() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (r *observedRelay) sessionEvents(kind string) []observe.Session {
	r.obs.mu.Lock()
	defer r.obs.mu.Unlock()
	var out []observe.Session
	for _, e := range r.obs.sessions {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func readN(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return string(buf)
}

// The owner forwards to a private service: bytes go both ways, the stream is
// a live connection of the service (with live bytes) and of the forwarding
// session, and when it ends it is a connection-log row with the user, the
// token name and the address, and one ForwardOpened event.
func TestForward_ThePrivateServiceIsReachedBothWays(t *testing.T) {
	r := newForwardRelay(t, 19520, nil)
	rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "127.0.0.1:3389", Access: "private"})
	if !rr.OK || rr.RemotePort != 0 || rr.AccessMode != "private" || rr.Slug != "s-rdp" || rr.URL != "" {
		t.Fatalf("register: %+v", rr)
	}
	f := r.forwarder("secret")
	st, resp, err := f.open(t, "rdp")
	if err != nil || !resp.OK || resp.ServiceID != "svc-rdp" || resp.Name != "rdp" || resp.Code != "" {
		t.Fatalf("open: %+v %v", resp, err)
	}
	if got := readN(t, st, 3); got != "hi " {
		t.Fatalf("home said %q", got)
	}
	if _, err := st.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := readN(t, st, 5); got != "hello" {
		t.Fatalf("echo %q", got)
	}

	conns := r.srv.ServiceConnections("svc-rdp")
	if len(conns) != 1 {
		t.Fatalf("live connections: %+v", conns)
	}
	c := conns[0]
	if c.Kind != ConnKindForward || c.UserID != "u1" || c.TokenName != "u1-laptop" || c.SourceIP != "127.0.0.1" ||
		c.BytesIn != 5 || c.BytesOut != 8 || c.Since.IsZero() || c.SessionID == "" {
		t.Fatalf("live connection: %+v", c)
	}
	var fw []ForwardView
	for _, ss := range r.srv.SnapshotSessions() {
		if ss.SessionID == c.SessionID {
			fw = ss.Forwards
			if len(ss.Tunnels) != 0 {
				t.Fatalf("the forwarder holds tunnels: %+v", ss.Tunnels)
			}
		}
	}
	if len(fw) != 1 || fw[0].ServiceID != "svc-rdp" || fw[0].Service != "rdp" || fw[0].ID != c.ID || fw[0].BytesIn != 5 {
		t.Fatalf("forwards of the session: %+v", fw)
	}
	if tn, _ := r.srv.LookupTunnelByServiceID("svc-rdp"); tn.BytesIn.Load() != 5 || tn.BytesOut.Load() != 8 {
		t.Fatalf("tunnel bytes %d/%d", tn.BytesIn.Load(), tn.BytesOut.Load())
	}

	_ = st.Close()
	r.waitFor("the forward's row", func() bool { return len(r.rows(TrafficTCPForward)) == 1 })
	row := r.rows(TrafficTCPForward)[0]
	if row.ServiceID != "svc-rdp" || row.UserID != "u1" || row.TokenName != "u1-laptop" || row.SourceIP != "127.0.0.1" ||
		row.ClientSessionID != c.SessionID || row.BytesIn != 5 || row.BytesOut != 8 || row.Status != observe.OutcomeClean ||
		row.TunnelID == "" || row.EndedAt.Before(row.StartedAt) {
		t.Fatalf("row: %+v", row)
	}
	opened := r.sessionEvents(observe.ForwardOpened)
	if len(opened) != 1 || opened[0].UserID != "u1" || opened[0].ServiceID != "svc-rdp" || opened[0].TokenName != "u1-laptop" ||
		opened[0].SourceIP != "127.0.0.1" || opened[0].OwnerID != "u1" || opened[0].TokenID != "tok-u1" {
		t.Fatalf("ForwardOpened: %+v", opened)
	}
	r.waitFor("the connection is gone", func() bool { return len(r.srv.ServiceConnections("svc-rdp")) == 0 })
	for _, ss := range r.srv.SnapshotSessions() {
		if len(ss.Forwards) != 0 {
			t.Fatalf("forwards left: %+v", ss.Forwards)
		}
	}
}

// Another user is told not_found, word for word as for a service that does
// not exist; the refusal is a rejected row of the service. A user whose role
// the service allows gets through.
func TestForward_OnlyTheOwnerAndAllowedRoles(t *testing.T) {
	r := newForwardRelay(t, 19521, nil)
	if rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x", Access: "private"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	other := r.forwarder("other")
	_, denied, err := other.open(t, "rdp")
	if err != nil || denied.OK || denied.Code != proto.CodeNotFound {
		t.Fatalf("another user: %+v %v", denied, err)
	}
	_, absent, err := other.open(t, "ghost")
	if err != nil || absent.OK || absent.Code != proto.CodeNotFound {
		t.Fatalf("absent: %+v %v", absent, err)
	}
	if strings.ReplaceAll(absent.Error, "ghost", "rdp") != denied.Error || denied.ServiceID != "" || denied.Name != "" {
		t.Fatalf("the refusals differ: %q vs %q", denied.Error, absent.Error)
	}
	if r.told.Load() != 0 {
		t.Fatal("the home client was told of a refused forward")
	}
	rows := r.refusedRows(ConnLogReasonForbidden)
	if len(rows) != 1 || rows[0].Kind != TrafficTCPForward || rows[0].ServiceID != "svc-rdp" || rows[0].UserID != "u2" || rows[0].TokenName != "u2-laptop" {
		t.Fatalf("refused rows: %+v", rows)
	}

	r.fwd.allow("u3")
	friend := r.forwarder("friend")
	st, ok, err := friend.open(t, "rdp")
	if err != nil || !ok.OK {
		t.Fatalf("allowed role: %+v %v", ok, err)
	}
	if got := readN(t, st, 3); got != "hi " {
		t.Fatalf("home said %q", got)
	}
	// The authorization is asked on every open: the role taken away, the
	// next stream is refused.
	r.fwd.mu.Lock()
	delete(r.fwd.allowed, "u3")
	r.fwd.mu.Unlock()
	if _, again, _ := friend.open(t, "rdp"); again.OK || again.Code != proto.CodeNotFound {
		t.Fatalf("after the role was taken: %+v", again)
	}
}

// A service nobody serves is offline; a store that fails is internal, and
// says nothing of the store.
func TestForward_OfflineAndStoreFailure(t *testing.T) {
	r := newForwardRelay(t, 19522, nil)
	r.res.setMode("rdp", "private")
	f := r.forwarder("secret")
	_, resp, err := f.open(t, "rdp")
	if err != nil || resp.OK || resp.Code != proto.CodeOffline || !strings.Contains(resp.Error, "offline") {
		t.Fatalf("offline: %+v %v", resp, err)
	}
	r.fwd.mu.Lock()
	r.fwd.err = errors.New("database is locked")
	r.fwd.mu.Unlock()
	_, resp, err = f.open(t, "rdp")
	if err != nil || resp.OK || resp.Code != proto.CodeInternal || strings.Contains(resp.Error, "database") {
		t.Fatalf("store failure: %+v %v", resp, err)
	}
	_, resp, err = f.open(t, "")
	if err != nil || resp.OK || resp.Code != proto.CodeBadRequest {
		t.Fatalf("no target: %+v %v", resp, err)
	}
}

// Forwarding is for the owner's convenience in every mode: a service in mode
// open is reached the same way, and its visitors are live connections too.
func TestForward_OpenServiceAndVisitorsAreLiveConnections(t *testing.T) {
	r := newForwardRelay(t, 19523, nil)
	rr := r.home.register(t, proto.TunnelRegister{Name: "db", Type: "tcp", LocalAddr: "x"})
	if !rr.OK || rr.RemotePort != 19523 {
		t.Fatalf("register: %+v", rr)
	}
	visitor, err := net.Dial("tcp", "127.0.0.1:19523")
	if err != nil {
		t.Fatal(err)
	}
	defer visitor.Close()
	if got := readN(t, visitor, 3); got != "hi " {
		t.Fatalf("visitor got %q", got)
	}
	f := r.forwarder("secret")
	st, resp, err := f.open(t, "db")
	if err != nil || !resp.OK {
		t.Fatalf("open: %+v %v", resp, err)
	}
	if got := readN(t, st, 3); got != "hi " {
		t.Fatalf("forward got %q", got)
	}
	kinds := []string{}
	for _, c := range r.srv.ServiceConnections("svc-db") {
		kinds = append(kinds, c.Kind)
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{ConnKindForward, ConnKindVisitor}) {
		t.Fatalf("kinds %v", kinds)
	}
}

// A live connection can be closed by id: the stream ends, and its row is
// written.
func TestForward_CloseServiceConnection(t *testing.T) {
	r := newForwardRelay(t, 19524, nil)
	if rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x", Access: "private"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	f := r.forwarder("secret")
	st, resp, _ := f.open(t, "rdp")
	if !resp.OK {
		t.Fatalf("open: %+v", resp)
	}
	readN(t, st, 3)
	conns := r.srv.ServiceConnections("svc-rdp")
	if len(conns) != 1 {
		t.Fatalf("conns %+v", conns)
	}
	if r.srv.CloseServiceConnection("svc-rdp", "nope") || r.srv.CloseServiceConnection("svc-other", conns[0].ID) {
		t.Fatal("closed what is not there")
	}
	if !r.srv.CloseServiceConnection("svc-rdp", conns[0].ID) {
		t.Fatal("not closed")
	}
	_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := st.Read(make([]byte, 1)); err == nil {
		t.Fatal("the stream is still open")
	}
	r.waitFor("the row", func() bool { return len(r.rows(TrafficTCPForward)) == 1 })
}

// A relay that takes no forwards (as an older one) closes a forward stream
// without an answer and announces no forward; one that does announces it.
func TestForward_ARelayWithoutForwardsClosesTheStream(t *testing.T) {
	r := newForwardRelay(t, 19525, func(o *Options) { o.Forwards = nil })
	conn, ar := r.dial("secret", "")
	_ = conn.Close()
	if slices.Contains(ar.Capabilities, proto.CapForward) {
		t.Fatalf("capabilities %v", ar.Capabilities)
	}
	if rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x", Access: "private"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	f := r.forwarder("secret")
	if _, _, err := f.open(t, "rdp"); err == nil {
		t.Fatal("the relay answered a forward it does not take")
	}
	if r.told.Load() != 0 {
		t.Fatal("the home client was told")
	}

	with := newForwardRelay(t, 19526, nil)
	conn, ar = with.dial("secret", "")
	_ = conn.Close()
	if !slices.Equal(ar.Capabilities, []string{proto.CapTCPAccess, proto.CapForward}) {
		t.Fatalf("capabilities %v", ar.Capabilities)
	}
}

// A session opens forwardOpenBurst streams at once and then waits; what was
// refused counts too, so names cannot be probed. The tunnel's cap counts
// forwards like visitors.
func TestForward_Limits(t *testing.T) {
	r := newForwardRelay(t, 19527, nil)
	f := r.forwarder("other")
	for i := 0; i < forwardOpenBurst; i++ {
		if _, resp, _ := f.open(t, fmt.Sprintf("ghost%d", i)); resp.Code != proto.CodeNotFound {
			t.Fatalf("open %d: %+v", i, resp)
		}
	}
	if _, resp, _ := f.open(t, "ghost"); resp.Code != proto.CodeRateLimited {
		t.Fatalf("over the burst: %+v", resp)
	}
	r.fwd.mu.Lock()
	calls := r.fwd.calls
	r.fwd.mu.Unlock()
	if calls != forwardOpenBurst {
		t.Fatalf("the authorizer was asked %d times", calls)
	}
	// Another session has its own bucket.
	if _, resp, _ := r.forwarder("other").open(t, "ghost"); resp.Code != proto.CodeNotFound {
		t.Fatalf("another session: %+v", resp)
	}

	// The tunnel's cap: one connection at most (for users it allows; the
	// owner has its own slots, TestForward_TheOwnerIsNeverLockedOut).
	r.fwd.allow("u3")
	id := r.res.setMode("rdp", "private")
	r.res.mu.Lock()
	r.res.maxConns[id] = 1
	r.res.mu.Unlock()
	if rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK || rr.AccessMode != "private" {
		t.Fatalf("register: %+v", rr)
	}
	friend := r.forwarder("friend")
	st, resp, _ := friend.open(t, "rdp")
	if !resp.OK {
		t.Fatalf("first: %+v", resp)
	}
	readN(t, st, 3)
	if _, resp, _ := friend.open(t, "rdp"); resp.Code != proto.CodeRateLimited {
		t.Fatalf("over the tunnel's cap: %+v", resp)
	}
	if rows := r.refusedRows(TCPRefusedTunnelFull); len(rows) != 1 || rows[0].Kind != TrafficTCPForward {
		t.Fatalf("rows %+v", rows)
	}
}

func TestForwardLimits_BurstRefillAndActive(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newForwardLimits()
	l.now = func() time.Time { return now }
	for i := 0; i < forwardOpenBurst; i++ {
		if !l.admit() {
			t.Fatalf("open %d refused", i)
		}
	}
	if l.admit() {
		t.Fatal("over the burst")
	}
	now = now.Add(time.Minute / forwardOpensPerMinute)
	if !l.admit() {
		t.Fatal("a token came back after its time")
	}
	// forwardMaxActive at once, however full the bucket.
	l = newForwardLimits()
	l.now = func() time.Time { return now }
	for i := 0; i < forwardMaxActive; i++ {
		now = now.Add(time.Minute)
		if !l.admit() {
			t.Fatalf("active %d refused", i)
		}
	}
	now = now.Add(time.Minute)
	if l.admit() {
		t.Fatal("over the active cap")
	}
	l.done()
	if !l.admit() {
		t.Fatal("one ended, one may open")
	}
}

// rawStream opens a stream on f's session and writes raw bytes on it.
func (f *forwarder) rawStream(t *testing.T, b []byte) *yamux.Stream {
	t.Helper()
	st, err := f.sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 0 {
		if _, err := st.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// closedSoon reports whether the relay ends st within d.
func closedSoon(st *yamux.Stream, d time.Duration) bool {
	_ = st.SetReadDeadline(time.Now().Add(d))
	_, err := st.Read(make([]byte, 1))
	return err == io.EOF
}

// The first frame of a client-opened stream may declare at most
// maxFirstFrame bytes: a larger one is closed before its body is read.
func TestDataStream_FirstFrameIsSmall(t *testing.T) {
	r := newForwardRelay(t, 19540, nil)
	f := r.forwarder("secret")
	hdr := func(n uint32) []byte { return []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)} }
	if st := f.rawStream(t, hdr(maxFirstFrame+1)); !closedSoon(st, 2*time.Second) {
		t.Fatal("a first frame over the cap was waited for")
	}
	// At the cap, the relay reads on (and closes at its deadline, since no
	// body comes): it is not closed at once.
	if st := f.rawStream(t, hdr(maxFirstFrame)); closedSoon(st, 300*time.Millisecond) {
		t.Fatal("a first frame at the cap was refused")
	}
}

// A session may have maxStreamsWaiting streams that have not sent their
// first frame; one more is closed at once. When one sends its frame, there
// is room again.
func TestDataStream_StreamsWaitingForTheFirstFrame(t *testing.T) {
	r := newForwardRelay(t, 19541, nil)
	f := r.forwarder("secret")
	idle := make([]*yamux.Stream, 0, maxStreamsWaiting)
	for i := 0; i < maxStreamsWaiting; i++ {
		idle = append(idle, f.rawStream(t, nil))
	}
	// yamux opens a stream with its first write; a window update does it.
	for _, st := range idle {
		_, _ = st.Write(nil)
	}
	r.waitFor("the idle streams are waiting", func() bool { return r.waitingStreams() == maxStreamsWaiting })
	over := f.rawStream(t, []byte{0})
	if !closedSoon(over, 2*time.Second) {
		t.Fatal("a stream over the limit was kept")
	}
	_ = idle[0].Close() // its read ends: one waits less
	r.waitFor("a slot is free", func() bool { return r.waitingStreams() < maxStreamsWaiting })
	if _, resp, err := f.open(t, "ghost"); err != nil || resp.Code != proto.CodeNotFound {
		t.Fatalf("after one went: %+v %v", resp, err)
	}
}

// waitingStreams is how many streams of all sessions wait for their first
// frame.
func (r *observedRelay) waitingStreams() int {
	n := 0
	for _, cs := range r.srv.reg.Sessions() {
		n += int(cs.waitingFirst.Load())
	}
	return n
}

// The owner keeps a way in: users the policy allows may fill the tunnel's
// cap, and the owner still forwards. One allowed user holds at most
// forwardPerUser streams to one service.
func TestForward_TheOwnerIsNeverLockedOut(t *testing.T) {
	r := newForwardRelay(t, 19542, nil)
	id := r.res.setMode("rdp", "private")
	r.res.mu.Lock()
	r.res.maxConns[id] = forwardPerUser + 1
	r.res.mu.Unlock()
	if rr := r.home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	r.fwd.allow("u2")
	r.fwd.allow("u3")
	other, friend := r.forwarder("other"), r.forwarder("friend")
	for i := 0; i < forwardPerUser; i++ {
		if _, resp, _ := friend.open(t, "rdp"); !resp.OK {
			t.Fatalf("friend %d: %+v", i, resp)
		}
	}
	if _, resp, _ := friend.open(t, "rdp"); resp.Code != proto.CodeRateLimited {
		t.Fatalf("over the per-user limit: %+v", resp)
	}
	if _, resp, _ := other.open(t, "rdp"); !resp.OK {
		t.Fatalf("other user: %+v", resp)
	}
	if _, resp, _ := other.open(t, "rdp"); resp.Code != proto.CodeRateLimited {
		t.Fatalf("over the tunnel's cap: %+v", resp)
	}
	owner := r.forwarder("secret")
	st, resp, _ := owner.open(t, "rdp")
	if !resp.OK {
		t.Fatalf("the owner was locked out: %+v", resp)
	}
	if got := readN(t, st, 3); got != "hi " {
		t.Fatalf("home said %q", got)
	}
}

// A forward whose home client never opens the stream ends offline after the
// pairing timeout, and gives back everything it held: the session's open
// count, the tunnel's pending and active counts and the per-user count.
func TestForward_APairingTimeoutReleasesTheLimits(t *testing.T) {
	fwd := &fakeForwards{allowed: map[string]bool{"u2": true}}
	res := newTCPResolver()
	fwd.res = res
	r := tcpRelay(t, 19543, 19543, res, func(o *Options) {
		o.TCPAccess, o.ConnLog, o.Auth, o.Forwards = newFakeTCPAccess(), &recConnLog{}, tokenUsers{}, fwd
	})
	home := r.client(nil) // never opens a stream
	if rr := home.register(t, proto.TunnelRegister{Name: "rdp", Type: "tcp", LocalAddr: "x", Access: "private"}); !rr.OK {
		t.Fatalf("register: %+v", rr)
	}
	f := r.forwarder("other")
	start := time.Now()
	if _, resp, err := f.open(t, "rdp"); err != nil || resp.Code != proto.CodeOffline {
		t.Fatalf("open: %+v %v", resp, err)
	}
	if d := time.Since(start); d < tcpPairTimeout-time.Second {
		t.Fatalf("answered after %v", d)
	}
	tn, _ := r.srv.LookupTunnelByServiceID("svc-rdp")
	r.waitFor("the limits are free", func() bool {
		active, pending := tn.limits.counts()
		return active == 0 && pending == 0 && tn.limits.forwardsOf("u2") == 0
	})
	for _, cs := range r.srv.reg.Sessions() {
		if l := cs.forwardLimits(); l.activeCount() != 0 {
			t.Fatalf("session %s still counts %d", cs.SessionID, l.activeCount())
		}
	}
}
