package acme

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeIssuer records what CustomDomains asks of certmagic.
type fakeIssuer struct {
	mu        sync.Mutex
	managed   []string
	ctxs      map[string]context.Context
	unmanaged []string
	ready     map[string]bool
	manageErr error
	block     chan struct{} // when set, ManageAsync waits on it (a hanging ACME)
}

func newFakeIssuer() *fakeIssuer {
	return &fakeIssuer{ctxs: map[string]context.Context{}, ready: map[string]bool{}}
}

func (f *fakeIssuer) ManageAsync(ctx context.Context, names []string) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		f.managed = append(f.managed, n)
		f.ctxs[n] = ctx
	}
	return f.manageErr
}

func (f *fakeIssuer) Unmanage(names []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmanaged = append(f.unmanaged, names...)
}

func (f *fakeIssuer) HasCertificate(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready[name]
}

func (f *fakeIssuer) snapshot() (managed, unmanaged []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.managed), slices.Clone(f.unmanaged)
}

type failure struct{ serviceID, hostname, reason string }

type failures struct {
	mu  sync.Mutex
	got []failure
}

func (r *failures) add(serviceID, hostname, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, failure{serviceID, hostname, reason})
}

func (r *failures) list() []failure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// ownsAllBut refuses the names given, as the H12 checks would.
func ownsAllBut(refused ...string) func(string) string {
	return func(h string) string {
		if slices.Contains(refused, h) {
			return "hostname " + h + " is a name the relay serves itself"
		}
		return ""
	}
}

func TestCustomDomains_ManageAndUnmanage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	iss := newFakeIssuer()
	c := NewCustomDomains(ctx, iss, CustomOptions{Allow: ownsAllBut()})

	if err := c.Manage("svc1", "app.example.com"); err != nil {
		t.Fatal(err)
	}
	managed, _ := iss.snapshot()
	if !slices.Equal(managed, []string{"app.example.com"}) {
		t.Fatalf("managed %v", managed)
	}
	// The certificate is obtained in the background: the name's context
	// outlives the API request that bound it.
	mctx := iss.ctxs["app.example.com"]
	if mctx.Err() != nil {
		t.Fatal("the context of a managed name is done at once")
	}
	if c.Ready("app.example.com") {
		t.Fatal("ready before certmagic has a certificate")
	}
	iss.ready["app.example.com"] = true
	if !c.Ready("app.example.com") {
		t.Fatal("not ready with a certificate")
	}

	// Binding the same name again is no second order.
	if err := c.Manage("svc1", "app.example.com"); err != nil {
		t.Fatal(err)
	}
	if managed, _ := iss.snapshot(); len(managed) != 1 {
		t.Fatalf("managed twice: %v", managed)
	}

	c.Unmanage("app.example.com")
	_, unmanaged := iss.snapshot()
	if !slices.Equal(unmanaged, []string{"app.example.com"}) {
		t.Fatalf("unmanaged %v", unmanaged)
	}
	if mctx.Err() == nil {
		t.Fatal("unmanage left the retries of the name running")
	}
	if c.Ready("app.example.com") {
		t.Fatal("an unmanaged name is ready")
	}
	// A name that was never managed is not passed on.
	c.Unmanage("other.example.com")
	if _, unmanaged := iss.snapshot(); len(unmanaged) != 1 {
		t.Fatalf("unmanaged %v", unmanaged)
	}
}

// Review Focus 3: a certificate is only ever ordered for a name the relay
// owns (H12); a wildcard never (auto needs HTTP-01 or TLS-ALPN-01).
func TestCustomDomains_ANameTheRelayDoesNotOwnIsNeverManaged(t *testing.T) {
	iss := newFakeIssuer()
	var f failures
	c := NewCustomDomains(context.Background(), iss, CustomOptions{
		Allow: ownsAllBut("burrow.example.com"), OnFailure: f.add,
	})
	for _, h := range []string{"burrow.example.com", "*.example.com", ""} {
		if err := c.Manage("svc1", h); !errors.Is(err, ErrNotOwned) {
			t.Errorf("Manage(%q) = %v, want ErrNotOwned", h, err)
		}
	}
	c.Load([]CustomName{{ServiceID: "svc2", Hostname: "burrow.example.com"}})
	c.Wait()
	if managed, _ := iss.snapshot(); len(managed) != 0 {
		t.Fatalf("managed %v", managed)
	}
	got := f.list()
	if len(got) != 4 || got[3].serviceID != "svc2" || got[3].hostname != "burrow.example.com" || got[3].reason == "" {
		t.Fatalf("failures %+v", got)
	}
	if c.Ready("burrow.example.com") {
		t.Fatal("ready")
	}
}

// A nil Allow refuses everything: the relay never orders a certificate it
// was not told it may.
func TestCustomDomains_NoAllowRefusesAll(t *testing.T) {
	iss := newFakeIssuer()
	c := NewCustomDomains(context.Background(), iss, CustomOptions{})
	if err := c.Manage("svc1", "app.example.com"); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("%v", err)
	}
}

// A failure of certmagic is reported for the names CustomDomains manages
// (audited by the caller), and only for those.
func TestCustomDomains_CertEvents(t *testing.T) {
	iss := newFakeIssuer()
	iss.manageErr = errors.New("storage broken")
	var f failures
	c := NewCustomDomains(context.Background(), iss, CustomOptions{Allow: ownsAllBut(), OnFailure: f.add})
	if err := c.Manage("svc1", "app.example.com"); err == nil {
		t.Fatal("the error of ManageAsync is lost")
	}
	c.CertEvent("cert_failed", "app.example.com", errors.New("acme: 429 rate limited"))
	c.CertEvent("cert_failed", "burrow.example.com", errors.New("the relay's own name"))
	c.CertEvent("cert_obtained", "app.example.com", nil)
	got := f.list()
	if len(got) != 2 || got[0].reason != "storage broken" || got[1].reason != "acme: 429 rate limited" || got[1].serviceID != "svc1" {
		t.Fatalf("failures %+v", got)
	}
}

// Startup never waits on ACME: Load returns while certmagic hangs, and the
// names are managed in the background, one after the other.
func TestCustomDomains_LoadNeverBlocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	iss := newFakeIssuer()
	iss.block = make(chan struct{})
	c := NewCustomDomains(ctx, iss, CustomOptions{Allow: ownsAllBut(), Stagger: time.Millisecond})

	done := make(chan struct{})
	go func() {
		c.Load([]CustomName{{"svc1", "a.example.com"}, {"svc2", "b.example.com"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Load waits on ACME")
	}
	close(iss.block)
	c.Wait()
	managed, _ := iss.snapshot()
	if !slices.Equal(managed, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("managed %v", managed)
	}
}

// A name deleted while startup still queues it is not managed afterwards.
func TestCustomDomains_LoadSkipsANameUnmanagedMeanwhile(t *testing.T) {
	iss := newFakeIssuer()
	iss.block = make(chan struct{})
	c := NewCustomDomains(context.Background(), iss, CustomOptions{Allow: ownsAllBut()})
	c.Load([]CustomName{{"svc1", "a.example.com"}, {"svc2", "b.example.com"}})
	c.Unmanage("b.example.com")
	close(iss.block)
	c.Wait()
	managed, _ := iss.snapshot()
	if slices.Contains(managed, "b.example.com") {
		t.Fatalf("managed %v", managed)
	}
}

// The stagger between names ends with the relay: shutdown does not wait
// for the queue.
func TestCustomDomains_LoadStopsWithTheRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	iss := newFakeIssuer()
	c := NewCustomDomains(ctx, iss, CustomOptions{Allow: ownsAllBut(), Stagger: time.Hour})
	c.Load([]CustomName{{"svc1", "a.example.com"}, {"svc2", "b.example.com"}})
	time.Sleep(20 * time.Millisecond)
	cancel()
	waited := make(chan struct{})
	go func() { c.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the queue outlives the relay")
	}
	if managed, _ := iss.snapshot(); len(managed) != 1 {
		t.Fatalf("managed %v", managed)
	}
}

// certmagic caches a stored certificate on a goroutine of its own, so an
// Unmanage right after a Manage is repeated a little later; not when the
// name was bound again meanwhile.
func TestCustomDomains_UnmanageIsRepeated(t *testing.T) {
	old := reunmanageAfter
	reunmanageAfter = 20 * time.Millisecond
	defer func() { reunmanageAfter = old }()

	iss := newFakeIssuer()
	c := NewCustomDomains(context.Background(), iss, CustomOptions{Allow: ownsAllBut()})
	_ = c.Manage("svc1", "a.example.com")
	c.Unmanage("a.example.com")
	_ = c.Manage("svc1", "b.example.com")
	c.Unmanage("b.example.com")
	_ = c.Manage("svc1", "b.example.com") // bound again
	time.Sleep(200 * time.Millisecond)
	_, unmanaged := iss.snapshot()
	var a, b int
	for _, n := range unmanaged {
		switch n {
		case "a.example.com":
			a++
		case "b.example.com":
			b++
		}
	}
	if a != 2 || b != 1 {
		t.Fatalf("unmanaged %v", unmanaged)
	}
}
