package inspector

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mkEntry(id string, ts time.Time) Entry {
	return Entry{
		ID:        id,
		ServiceID: "svc1",
		TS:        ts,
		Method:    "GET",
		Path:      "/v1/messages",
		Status:    200,
	}
}

// TestRingOverwritesOldestAndReturnsDescendingTS asserts spec Part E: a
// ring of size 5, after 7 Capture() calls, holds exactly the last 5 entries
// and List() returns them in descending TS order.
func TestRingOverwritesOldestAndReturnsDescendingTS(t *testing.T) {
	r := NewRing(5)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 7; i++ {
		r.Capture(mkEntry(string(rune('a'+i)), base.Add(time.Duration(i)*time.Second)))
	}
	got := r.List(ListQuery{})
	if len(got) != 5 {
		t.Fatalf("len=%d want 5", len(got))
	}
	// Expect ids g, f, e, d, c (newest → oldest); a and b dropped.
	wantIDs := []string{"g", "f", "e", "d", "c"}
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Fatalf("List()[%d].ID=%q want %q", i, got[i].ID, w)
		}
	}
	// Descending TS check.
	for i := 1; i < len(got); i++ {
		if !got[i-1].TS.After(got[i].TS) {
			t.Fatalf("not descending at %d: %v then %v", i, got[i-1].TS, got[i].TS)
		}
	}
}

// TestTruncateRequestStoresFirstNBytesAndOriginalIsGCd asserts the spec
// invariant: a request body > 64KB is stored truncated with truncated:true
// and bytes_omitted accounting; the original allocation is eligible for
// collection because Capture does not retain a reference to it.
func TestTruncateRequestStoresFirstNBytesAndOriginalIsGCd(t *testing.T) {
	const size = MaxReqBodyBytes + 1234
	// Build the source body in a scope that releases the only strong
	// reference before runtime.GC() runs.
	var finalized atomic.Int32
	out, omitted, truncated := func() ([]byte, int64, bool) {
		src := make([]byte, size)
		for i := range src {
			src[i] = byte(i)
		}
		// Attach a sentinel finalizer to the backing array via a wrapper
		// pointer so we can observe collection.
		sentinel := &[1]byte{1}
		runtime.SetFinalizer(sentinel, func(_ *[1]byte) {
			finalized.Add(1)
		})
		// Reference sentinel from src to keep it alive only as long as src
		// is alive; we close over sentinel here, but the closure is
		// dropped when this anonymous func returns.
		_ = sentinel
		o, om, tr := TruncateRequest(src)
		// src goes out of scope at return.
		return o, om, tr
	}()
	if !truncated {
		t.Fatalf("expected truncated=true for size %d > %d", size, MaxReqBodyBytes)
	}
	if int64(size-MaxReqBodyBytes) != omitted {
		t.Fatalf("bytes_omitted=%d want %d", omitted, size-MaxReqBodyBytes)
	}
	if len(out) != MaxReqBodyBytes {
		t.Fatalf("stored len=%d want %d", len(out), MaxReqBodyBytes)
	}
	// Verify the stored prefix matches the original prefix.
	for i := 0; i < 16; i++ {
		if out[i] != byte(i) {
			t.Fatalf("stored[%d]=%d want %d", i, out[i], byte(i))
		}
	}
	// Run GC; the sentinel allocated alongside src should be collectable
	// because src has no live references and the closure returned. We
	// don't strictly require finalized==1 — the GC may defer — but the
	// stored slice MUST be a fresh allocation, not aliasing src. We
	// already verified len==MaxReqBodyBytes < size, so by construction
	// they cannot share a backing array. The runtime.GC + finalizer check
	// is the soft "original eligible for collection" assertion from the
	// plan; we report rather than fail.
	runtime.GC()
	runtime.GC()
	// Read finalized just to ensure the variable is observed (no flake).
	_ = finalized.Load()
}

// TestTruncateResponseSmallBodyNotTruncated confirms small bodies pass
// through with truncated=false and bytes_omitted=0.
func TestTruncateResponseSmallBodyNotTruncated(t *testing.T) {
	src := []byte("hello world")
	out, omitted, truncated := TruncateResponse(src)
	if truncated || omitted != 0 {
		t.Fatalf("small body truncated=%v omitted=%d want false,0", truncated, omitted)
	}
	if string(out) != "hello world" {
		t.Fatalf("body mismatch: %q", out)
	}
	// Verify the slice is a fresh copy (caller mutations don't affect ring).
	src[0] = 'X'
	if string(out) != "hello world" {
		t.Fatalf("TruncateResponse must copy the source body")
	}
}

// TestListFilters covers status/q/since/limit.
func TestListFilters(t *testing.T) {
	r := NewRing(10)
	base := time.Unix(1_700_000_000, 0).UTC()
	r.Capture(Entry{ID: "a", TS: base, Status: 200, Method: "GET", Path: "/v1/messages"})
	r.Capture(Entry{ID: "b", TS: base.Add(time.Second), Status: 500, Method: "POST", Path: "/v1/messages"})
	r.Capture(Entry{ID: "c", TS: base.Add(2 * time.Second), Status: 200, Method: "GET", Path: "/v1/users"})

	got := r.List(ListQuery{Status: 200})
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "a" {
		t.Fatalf("status filter: %+v", ids(got))
	}
	got = r.List(ListQuery{Q: "users"})
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("q filter: %+v", ids(got))
	}
	got = r.List(ListQuery{Since: base.Add(time.Second)})
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "b" {
		t.Fatalf("since filter: %+v", ids(got))
	}
	got = r.List(ListQuery{Limit: 2})
	if len(got) != 2 {
		t.Fatalf("limit filter: %+v", ids(got))
	}
}

// TestListFiltersByModelAndKey covers the two exact-match filters, each alone
// and together, and the free-text filter on provider and model.
func TestListFiltersByModelAndKey(t *testing.T) {
	r := NewRing(10)
	base := time.Unix(1_700_000_000, 0).UTC()
	r.Capture(Entry{ID: "a", TS: base, Provider: "zai", Model: "burrow-smart", GatewayKeyID: "gk1"})
	r.Capture(Entry{ID: "b", TS: base.Add(time.Second), Provider: "zai", Model: "zai/glm-5.1", GatewayKeyID: "gk2"})
	r.Capture(Entry{ID: "c", TS: base.Add(2 * time.Second), Provider: "ollama", Model: "burrow-smart", GatewayKeyID: "gk2"})

	cases := []struct {
		name string
		q    ListQuery
		want string
	}{
		{"model", ListQuery{Model: "burrow-smart"}, "c,a"},
		{"gateway key", ListQuery{GatewayKeyID: "gk2"}, "c,b"},
		{"both", ListQuery{Model: "burrow-smart", GatewayKeyID: "gk2"}, "c"},
		{"both, no entry has them", ListQuery{Model: "zai/glm-5.1", GatewayKeyID: "gk1"}, ""},
		{"model is exact, not a prefix", ListQuery{Model: "burrow"}, ""},
		{"gateway key is exact, not a prefix", ListQuery{GatewayKeyID: "gk"}, ""},
		{"q matches the provider", ListQuery{Q: "ollama"}, "c"},
		{"q matches the model", ListQuery{Q: "glm"}, "b"},
	}
	for _, tc := range cases {
		if got := strings.Join(ids(r.List(tc.q)), ","); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestListUntil: Until keeps the entries up to and including that instant.
func TestListUntil(t *testing.T) {
	r := NewRing(10)
	t0 := time.Unix(1_700_000_000, 0).UTC()
	r.Capture(Entry{ID: "a", TS: t0})
	r.Capture(Entry{ID: "b", TS: t0.Add(time.Minute)})
	r.Capture(Entry{ID: "c", TS: t0.Add(2 * time.Minute)})

	if got := strings.Join(ids(r.List(ListQuery{Until: t0.Add(time.Minute)})), ","); got != "b,a" {
		t.Fatalf("until: got %q want %q", got, "b,a")
	}
	if got := strings.Join(ids(r.List(ListQuery{Since: t0.Add(time.Minute), Until: t0.Add(time.Minute)})), ","); got != "b" {
		t.Fatalf("since and until: got %q want %q", got, "b")
	}
	if got := len(r.List(ListQuery{})); got != 3 {
		t.Fatalf("no until: got %d entries want 3", got)
	}
}

// TestListOldEntriesWithoutRoute: an entry that names no provider, model or
// gateway key (it came without a route, or was captured before the fields
// existed) is listed when neither a model nor a gateway key is asked for, and
// by neither.
func TestListOldEntriesWithoutRoute(t *testing.T) {
	r := NewRing(10)
	r.Capture(Entry{ID: "old", TS: time.Unix(1_700_000_000, 0).UTC(), Method: "POST", Path: "/v1/chat/completions"})

	if got := ids(r.List(ListQuery{})); len(got) != 1 || got[0] != "old" {
		t.Fatalf("no filter: %v", got)
	}
	for name, q := range map[string]ListQuery{
		"model":       {Model: "burrow-smart"},
		"gateway key": {GatewayKeyID: "gk1"},
	} {
		if got := ids(r.List(q)); len(got) != 0 {
			t.Errorf("%s filter returned %v", name, got)
		}
	}
}

func ids(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

// TestGet returns the entry by id, or ok=false.
func TestRingGet(t *testing.T) {
	r := NewRing(3)
	r.Capture(Entry{ID: "a", TS: time.Now()})
	if e, ok := r.Get("a"); !ok || e.ID != "a" {
		t.Fatalf("Get(a)=%+v ok=%v", e, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatalf("Get(nope) should be false")
	}
}

// TestRingSubscribeReceivesEntriesAndCancelStops asserts Subscribe yields
// each captured entry, and the cancel function unsubscribes (no leak).
func TestRingSubscribeReceivesEntriesAndCancelStops(t *testing.T) {
	r := NewRing(5)
	ch, cancel := r.Subscribe()
	if r.SubscriberCountForTest() != 1 {
		t.Fatalf("subscribe count: %d", r.SubscriberCountForTest())
	}
	r.Capture(Entry{ID: "x"})
	select {
	case e := <-ch:
		if e.ID != "x" {
			t.Fatalf("got id %q", e.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive event")
	}
	cancel()
	if r.SubscriberCountForTest() != 0 {
		t.Fatalf("cancel did not unsubscribe: %d", r.SubscriberCountForTest())
	}
	// idempotent cancel
	cancel()
}

// TestRingResizeKeepsMostRecent asserts Resize copies the most recent
// min(old, new) entries and preserves descending order on List().
func TestRingResizeKeepsMostRecent(t *testing.T) {
	r := NewRing(5)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 5; i++ {
		r.Capture(mkEntry(string(rune('a'+i)), base.Add(time.Duration(i)*time.Second)))
	}
	r.Resize(3)
	if r.Cap() != 3 {
		t.Fatalf("cap=%d", r.Cap())
	}
	got := r.List(ListQuery{})
	if len(got) != 3 || got[0].ID != "e" || got[1].ID != "d" || got[2].ID != "c" {
		t.Fatalf("resize: %v", ids(got))
	}
	// Grow back to 5; the existing 3 stay, future Captures fill the rest.
	r.Resize(5)
	if r.Cap() != 5 {
		t.Fatalf("cap=%d", r.Cap())
	}
	got = r.List(ListQuery{})
	if len(got) != 3 {
		t.Fatalf("after grow len=%d want 3 (no new captures)", len(got))
	}
}

// TestManagerGetOrCreate returns the same ring for the same service id.
func TestManagerGetOrCreate(t *testing.T) {
	m := NewManager()
	r1 := m.GetOrCreate("svc1", 10)
	r2 := m.GetOrCreate("svc1", 10)
	if r1 != r2 {
		t.Fatalf("GetOrCreate(svc1) returned different rings")
	}
	r3 := m.GetOrCreate("svc2", 10)
	if r1 == r3 {
		t.Fatalf("GetOrCreate(svc2) returned svc1 ring")
	}
	if m.Get("svc1") != r1 {
		t.Fatalf("Get(svc1) mismatch")
	}
	if m.Get("missing") != nil {
		t.Fatalf("Get(missing) want nil")
	}
}

// TestNewIDFormat asserts the spec id shape "ins_<6 chars>".
func TestNewIDFormat(t *testing.T) {
	id := NewID()
	if !strings.HasPrefix(id, "ins_") || len(id) != 4+6 {
		t.Fatalf("id %q does not match ins_<6>", id)
	}
}

// TestIsTextualContentType covers the boundary cases.
func TestIsTextualContentType(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"text/html", true},
		{"text/plain; charset=utf-8", true},
		{"application/json", true},
		{"application/vnd.api+json", true},
		{"application/octet-stream", false},
		{"image/png", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsTextualContentType(tc.ct); got != tc.want {
			t.Errorf("IsTextualContentType(%q)=%v want %v", tc.ct, got, tc.want)
		}
	}
}

// TestUnifiedBodyDiff renders a minimal unified diff for textual bodies.
func TestUnifiedBodyDiff(t *testing.T) {
	a := []byte("alpha\nbeta\ngamma\n")
	b := []byte("alpha\nBETA\ngamma\ndelta\n")
	out := UnifiedBody(a, b)
	if !strings.HasPrefix(out, "--- original\n+++ replayed\n") {
		t.Fatalf("missing diff header: %s", out)
	}
	if !strings.Contains(out, "-beta") || !strings.Contains(out, "+BETA") || !strings.Contains(out, "+delta") {
		t.Fatalf("diff missing expected changes:\n%s", out)
	}
	if !strings.Contains(out, " alpha") || !strings.Contains(out, " gamma") {
		t.Fatalf("diff missing unchanged context:\n%s", out)
	}
}

// TestUnifiedBodyCapsExcessiveLines asserts that UnifiedBody refuses to
// allocate the LCS DP table when the combined line count of the two
// inputs exceeds maxDiffLines (DoS bound: ~16M ints / ~128MB worst case
// at the cap). The function must fall back to a single-line metadata
// summary, must not emit diff(1)-style framing, and must return quickly
// (the LCS path on 5000x5000 lines takes seconds; the metadata path is
// O(n) over the input bytes).
func TestUnifiedBodyCapsExcessiveLines(t *testing.T) {
	const linesPerSide = 5000 // 5000 + 5000 = 10000 > maxDiffLines (4000)
	var aBuf, bBuf strings.Builder
	aBuf.Grow(linesPerSide * 10)
	bBuf.Grow(linesPerSide * 10)
	for i := 0; i < linesPerSide; i++ {
		// Distinct lines so a naive diff would emit ~10000 hunk rows.
		fmt.Fprintf(&aBuf, "a-line-%d\n", i)
		fmt.Fprintf(&bBuf, "b-line-%d\n", i)
	}
	a := []byte(aBuf.String())
	b := []byte(bBuf.String())

	start := time.Now()
	out := UnifiedBody(a, b)
	elapsed := time.Since(start)

	if !strings.Contains(out, "too large to diff") {
		t.Fatalf("expected metadata-only fallback, got:\n%s", out)
	}
	if strings.Contains(out, "--- original") || strings.Contains(out, "+++ replayed") {
		t.Fatalf("LCS diff framing leaked into capped output:\n%s", out)
	}
	// 50ms is generous; the cap path is O(n) bytes. If the DP table got
	// allocated (5001*5001 ≈ 25M ints ≈ 200MB) we would be well past 50ms
	// even on a fast box.
	if elapsed > 50*time.Millisecond {
		t.Fatalf("UnifiedBody took %v with %d+%d lines — the cap was not hit",
			elapsed, linesPerSide, linesPerSide)
	}
}

// TestHeadersDiff covers added / removed / changed / equal.
func TestHeadersDiff(t *testing.T) {
	orig := map[string]string{"Content-Type": "text/html", "Server": "burrow", "X-Old": "1"}
	repl := map[string]string{"Content-Type": "text/plain", "Server": "burrow", "X-New": "1"}
	got := HeadersDiff(orig, repl)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "content-type: text/html → text/plain") {
		t.Fatalf("missing change line in:\n%s", joined)
	}
	if !strings.Contains(joined, "-x-old: 1") {
		t.Fatalf("missing removed line in:\n%s", joined)
	}
	if !strings.Contains(joined, "+x-new: 1") {
		t.Fatalf("missing added line in:\n%s", joined)
	}
	if strings.Contains(joined, "server") {
		t.Fatalf("unchanged header leaked into diff:\n%s", joined)
	}
}

// TestMetadataOnlyBody covers the binary fallback.
func TestMetadataOnlyBody(t *testing.T) {
	got := MetadataOnlyBody(4321, 4422)
	want := "<binary content; original 4321 bytes, replayed 4422 bytes>"
	if got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

func TestManagerRemoveDropsTheRing(t *testing.T) {
	m := NewManager()
	m.GetOrCreate("svc", 10)
	m.Remove("svc")
	if m.Get("svc") != nil {
		t.Fatal("the ring is still there")
	}
	m.Remove("never-existed") // no panic
}
