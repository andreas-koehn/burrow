package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/inspector"
)

// fakeInspectorOwner is the InspectorOwnerLookup stand-in. owners maps
// serviceID → userID; unknown service ids return db.ErrNotFound.
type fakeInspectorOwner struct {
	owners map[string]string
}

func (f *fakeInspectorOwner) GetServiceOwner(_ context.Context, id string) (string, error) {
	if u, ok := f.owners[id]; ok {
		return u, nil
	}
	return "", db.ErrNotFound
}

// stubReplayer captures the request and returns a synthesized new Entry.
type stubReplayer struct {
	mu        sync.Mutex
	gotReqs   []*http.Request
	respBody  []byte // override response body for the synthesized new Entry
	respCT    string // content-type for the new Entry's RespHeaders
	respCache string
	err       error
	ring      *inspector.Ring // when set, the replayer Captures into this ring
}

func (s *stubReplayer) Replay(_ context.Context, serviceID string, req *http.Request) (inspector.Entry, error) {
	s.mu.Lock()
	s.gotReqs = append(s.gotReqs, req)
	s.mu.Unlock()
	if s.err != nil {
		return inspector.Entry{}, s.err
	}
	respCT := s.respCT
	if respCT == "" {
		respCT = "application/json"
	}
	respBody := s.respBody
	if respBody == nil {
		respBody = []byte(`{"replayed":true}`)
	}
	e := inspector.Entry{
		ID:          inspector.NewID(),
		ServiceID:   serviceID,
		TS:          time.Now().UTC(),
		Method:      req.Method,
		Path:        req.URL.Path,
		Status:      200,
		RespHeaders: map[string]string{"Content-Type": respCT},
		RespBody:    respBody,
		Cache:       s.respCache,
	}
	if s.ring != nil {
		s.ring.Capture(e)
	}
	return e, nil
}

// inspectorDeps builds a Deps wired with an InspectorRings manager and the
// optional Replayer / OwnerLookup. The fakeUserStore reports the given role
// for the logged-in user; ownerID controls which userID owns each service
// in the owner lookup.
func inspectorDeps(role string, services map[string]string, replayer *stubReplayer) (Deps, *inspector.Manager) {
	mgr := inspector.NewManager()
	d := Deps{
		Log:               discardLog(),
		Users:             &fakeUserStore{role: role},
		InspectorRings:    mgr,
		InspectorServices: &fakeInspectorOwner{owners: services},
	}
	// Only set the InspectorReplayer interface field when a real replayer
	// is provided; storing a typed-nil pointer in the interface would make
	// `d.InspectorReplayer != nil` true and bypass the 503 fallback.
	if replayer != nil {
		d.InspectorReplayer = replayer
	}
	return d, mgr
}

// TestInspectorListReturnsDescendingTSAndFilters covers GET /requests with
// q/status/limit filters and asserts the wire shape (utf8 vs base64
// encoding for bodies).
func TestInspectorListReturnsDescendingTSAndFilters(t *testing.T) {
	d, mgr := inspectorDeps("admin", map[string]string{"svc1": "u-self"}, nil)
	ring := mgr.GetOrCreate("svc1", 10)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i, st := range []int{200, 500, 200} {
		ring.Capture(inspector.Entry{
			ID:        fmt.Sprintf("ins_%d", i),
			ServiceID: "svc1",
			TS:        base.Add(time.Duration(i) * time.Second),
			Method:    "POST",
			Path:      "/v1/messages",
			Status:    st,
			ReqBody:   []byte(`{"hello":"world"}`),
			RespBody:  []byte{0xff, 0xfe, 0xfd}, // not valid utf8 — must be base64
		})
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	r := c.get(t, "/api/v1/services/svc1/inspector/requests")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var got []inspectorEntryJSON
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if len(got) != 3 || got[0].ID != "ins_2" || got[2].ID != "ins_0" {
		t.Fatalf("unexpected list: %+v", got)
	}
	if got[0].ReqBodyEnc != "utf8" || got[0].ReqBody != `{"hello":"world"}` {
		t.Fatalf("req body enc=%q body=%q", got[0].ReqBodyEnc, got[0].ReqBody)
	}
	if got[0].RespBodyEnc != "base64" {
		t.Fatalf("resp body must be base64; enc=%q body=%q", got[0].RespBodyEnc, got[0].RespBody)
	}

	// status=500 filter — exactly one match.
	r = c.get(t, "/api/v1/services/svc1/inspector/requests?status=500")
	got = nil
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if len(got) != 1 || got[0].ID != "ins_1" {
		t.Fatalf("status filter: %+v", got)
	}

	// limit=1 caps the result.
	r = c.get(t, "/api/v1/services/svc1/inspector/requests?limit=1")
	got = nil
	_ = json.NewDecoder(r.Body).Decode(&got)
	r.Body.Close()
	if len(got) != 1 || got[0].ID != "ins_2" {
		t.Fatalf("limit filter: %+v", got)
	}
}

// TestInspectorListUnknownServiceReturns404 covers the ownership /
// not-found path for an admin caller.
func TestInspectorListUnknownServiceReturns404(t *testing.T) {
	d, _ := inspectorDeps("admin", map[string]string{}, nil)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/services/nope/inspector/requests")
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 got %d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestInspectorListNonOwnerForbidden asserts a "user" role calling against
// a service they don't own gets 403.
func TestInspectorListNonOwnerForbidden(t *testing.T) {
	d, mgr := inspectorDeps("user", map[string]string{"svc1": "u-other"}, nil)
	mgr.GetOrCreate("svc1", 10).Capture(inspector.Entry{ID: "ins_x", ServiceID: "svc1", TS: time.Now()})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/services/svc1/inspector/requests")
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 got %d body=%s", r.StatusCode, readBody(t, r))
	}
}

// TestInspectorOwnerCanReadOwnService asserts a "user" role calling against
// their own service succeeds.
func TestInspectorOwnerCanReadOwnService(t *testing.T) {
	d, mgr := inspectorDeps("user", map[string]string{"svc1": "u-self"}, nil)
	mgr.GetOrCreate("svc1", 10).Capture(inspector.Entry{ID: "ins_x", ServiceID: "svc1", TS: time.Now()})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/services/svc1/inspector/requests")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200 got %d", r.StatusCode)
	}
}

// TestInspectorGetRequest returns the entry with bodies; missing id → 404.
func TestInspectorGetRequest(t *testing.T) {
	d, mgr := inspectorDeps("admin", map[string]string{"svc1": "u-self"}, nil)
	mgr.GetOrCreate("svc1", 10).Capture(inspector.Entry{
		ID:        "ins_z",
		ServiceID: "svc1",
		TS:        time.Now().UTC(),
		Method:    "GET",
		Path:      "/v1/foo",
		Status:    200,
		ReqBody:   []byte("hello"),
	})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.get(t, "/api/v1/services/svc1/inspector/requests/ins_z")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var e inspectorEntryJSON
	_ = json.NewDecoder(r.Body).Decode(&e)
	r.Body.Close()
	if e.ID != "ins_z" || e.ReqBody != "hello" {
		t.Fatalf("entry mismatch: %+v", e)
	}

	r = c.get(t, "/api/v1/services/svc1/inspector/requests/nope")
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 got %d", r.StatusCode)
	}
}

// TestInspectorReplayCallsReplayerAndReturnsNewEntry asserts the spec wire
// shape: {new_entry: InspectorEntry} and ring length grows by one. The
// stubReplayer Captures into the same ring so we can verify the count.
func TestInspectorReplayCallsReplayerAndReturnsNewEntry(t *testing.T) {
	mgr := inspector.NewManager()
	ring := mgr.GetOrCreate("svc1", 10)
	ring.Capture(inspector.Entry{
		ID:         "ins_orig",
		ServiceID:  "svc1",
		TS:         time.Now().UTC(),
		Method:     "POST",
		Path:       "/v1/messages",
		Status:     200,
		ReqHeaders: map[string]string{"X-Orig": "1"},
		ReqBody:    []byte(`{"k":"v"}`),
	})
	rp := &stubReplayer{ring: ring, respBody: []byte(`{"replayed":true}`)}
	d := Deps{
		Log:               discardLog(),
		Users:             &fakeUserStore{role: "admin"},
		InspectorRings:    mgr,
		InspectorServices: &fakeInspectorOwner{owners: map[string]string{"svc1": "u-self"}},
		InspectorReplayer: rp,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	beforeLen := ring.Len()

	r := c.post(t, "/api/v1/services/svc1/inspector/requests/ins_orig/replay",
		map[string]any{"follow_routing": true})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var body struct {
		NewEntry inspectorEntryJSON `json:"new_entry"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()
	if body.NewEntry.ID == "" || body.NewEntry.Method != "POST" {
		t.Fatalf("new_entry not populated: %+v", body.NewEntry)
	}
	if got := ring.Len(); got != beforeLen+1 {
		t.Fatalf("ring length: before=%d after=%d (want +1)", beforeLen, got)
	}
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if len(rp.gotReqs) != 1 {
		t.Fatalf("replayer not called: %d", len(rp.gotReqs))
	}
	if rp.gotReqs[0].Header.Get("X-Orig") != "1" {
		t.Fatalf("original headers not propagated: %v", rp.gotReqs[0].Header)
	}
	if rp.gotReqs[0].Header.Get("Burrow-Cache") == "bypass" {
		t.Fatal("replay (non-compare) must not set Burrow-Cache: bypass")
	}
}

// TestInspectorReplayCompareReturnsUnifiedDiffForText asserts the wire
// shape {original, replayed, diff:{headers, body}} and that the replayer
// receives the Burrow-Cache: bypass header.
func TestInspectorReplayCompareReturnsUnifiedDiffForText(t *testing.T) {
	mgr := inspector.NewManager()
	ring := mgr.GetOrCreate("svc1", 10)
	ring.Capture(inspector.Entry{
		ID:          "ins_orig",
		ServiceID:   "svc1",
		TS:          time.Now().UTC(),
		Method:      "GET",
		Path:        "/v1/foo",
		Status:      200,
		RespHeaders: map[string]string{"Content-Type": "application/json"},
		RespBody:    []byte("{\n  \"hello\": \"world\"\n}\n"),
	})
	rp := &stubReplayer{
		ring:     ring,
		respCT:   "application/json",
		respBody: []byte("{\n  \"hello\": \"earth\"\n}\n"),
	}
	d := Deps{
		Log:               discardLog(),
		Users:             &fakeUserStore{role: "admin"},
		InspectorRings:    mgr,
		InspectorServices: &fakeInspectorOwner{owners: map[string]string{"svc1": "u-self"}},
		InspectorReplayer: rp,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.post(t, "/api/v1/services/svc1/inspector/requests/ins_orig/replay-compare", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", r.StatusCode, readBody(t, r))
	}
	var body struct {
		Original inspectorEntryJSON `json:"original"`
		Replayed inspectorEntryJSON `json:"replayed"`
		Diff     compareDiff        `json:"diff"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()
	if body.Original.ID != "ins_orig" {
		t.Fatalf("original.id=%q", body.Original.ID)
	}
	if body.Replayed.ID == "" || body.Replayed.ID == body.Original.ID {
		t.Fatalf("replayed.id missing or aliased: %q", body.Replayed.ID)
	}
	if !strings.Contains(body.Diff.Body, "-  \"hello\": \"world\"") ||
		!strings.Contains(body.Diff.Body, "+  \"hello\": \"earth\"") {
		t.Fatalf("unified diff missing expected lines:\n%s", body.Diff.Body)
	}
	// The compare arm must set Burrow-Cache: bypass on the replayer's req.
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if got := rp.gotReqs[0].Header.Get("Burrow-Cache"); got != "bypass" {
		t.Fatalf("Burrow-Cache header=%q want bypass", got)
	}
}

// TestInspectorReplayCompareBinaryFallsBackToMetadata asserts the binary
// (image/png) content-type produces the metadata-only diff body.
func TestInspectorReplayCompareBinaryFallsBackToMetadata(t *testing.T) {
	mgr := inspector.NewManager()
	ring := mgr.GetOrCreate("svc1", 10)
	ring.Capture(inspector.Entry{
		ID:          "ins_orig",
		ServiceID:   "svc1",
		TS:          time.Now().UTC(),
		Method:      "GET",
		Path:        "/v1/foo.png",
		Status:      200,
		RespHeaders: map[string]string{"Content-Type": "image/png"},
		RespBody:    []byte{0x89, 0x50, 0x4e, 0x47}, // 4 bytes
	})
	rp := &stubReplayer{
		ring:     ring,
		respCT:   "image/png",
		respBody: []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a}, // 6 bytes
	}
	d := Deps{
		Log:               discardLog(),
		Users:             &fakeUserStore{role: "admin"},
		InspectorRings:    mgr,
		InspectorServices: &fakeInspectorOwner{owners: map[string]string{"svc1": "u-self"}},
		InspectorReplayer: rp,
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.post(t, "/api/v1/services/svc1/inspector/requests/ins_orig/replay-compare", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", r.StatusCode)
	}
	var body struct {
		Diff compareDiff `json:"diff"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()
	if !strings.HasPrefix(body.Diff.Body, "<binary content;") {
		t.Fatalf("binary diff want metadata, got:\n%s", body.Diff.Body)
	}
	if !strings.Contains(body.Diff.Body, "original 4 bytes") ||
		!strings.Contains(body.Diff.Body, "replayed 6 bytes") {
		t.Fatalf("metadata-only body lengths: %s", body.Diff.Body)
	}
}

// TestInspectorReplayNoReplayerReturns503 asserts the route stays wired
// but returns 503 when no Replayer dep is injected.
func TestInspectorReplayNoReplayerReturns503(t *testing.T) {
	d, mgr := inspectorDeps("admin", map[string]string{"svc1": "u-self"}, nil)
	mgr.GetOrCreate("svc1", 10).Capture(inspector.Entry{ID: "ins_x", ServiceID: "svc1", TS: time.Now()})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.post(t, "/api/v1/services/svc1/inspector/requests/ins_x/replay", nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", r.StatusCode)
	}
}

// TestInspectorReplayUnknownIDReturns404 asserts a missing entry yields 404.
func TestInspectorReplayUnknownIDReturns404(t *testing.T) {
	rp := &stubReplayer{}
	d, _ := inspectorDeps("admin", map[string]string{"svc1": "u-self"}, rp)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.post(t, "/api/v1/services/svc1/inspector/requests/nope/replay", nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d want 404", r.StatusCode)
	}
}

// TestInspectorReplayForbiddenForNonOwner asserts a user role replaying
// against a service they don't own gets 403.
func TestInspectorReplayForbiddenForNonOwner(t *testing.T) {
	rp := &stubReplayer{}
	d, mgr := inspectorDeps("user", map[string]string{"svc1": "u-other"}, rp)
	mgr.GetOrCreate("svc1", 10).Capture(inspector.Entry{ID: "ins_x", ServiceID: "svc1", TS: time.Now()})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)
	r := c.post(t, "/api/v1/services/svc1/inspector/requests/ins_x/replay", nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", r.StatusCode)
	}
}

// TestInspectorStreamEmitsEventOnCapture asserts the SSE handler emits one
// event: request frame per Ring.Capture and that the bus unsubscribes on
// client disconnect (no leak).
func TestInspectorStreamEmitsEventOnCapture(t *testing.T) {
	mgr := inspector.NewManager()
	ring := mgr.GetOrCreate("svc1", 10)
	d := Deps{
		Log:               discardLog(),
		Users:             &fakeUserStore{role: "admin"},
		InspectorRings:    mgr,
		InspectorServices: &fakeInspectorOwner{owners: map[string]string{"svc1": "u-self"}},
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	// Log in to get the session/CSRF cookies, then open the SSE stream
	// over the same cookie jar.
	cl := authedClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/v1/services/svc1/inspector/stream", nil)
	resp, err := cl.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	// Give the handler a beat to subscribe, then capture an entry.
	time.Sleep(100 * time.Millisecond)
	ring.Capture(inspector.Entry{
		ID:        "ins_x",
		ServiceID: "svc1",
		TS:        time.Now().UTC(),
		Method:    "GET",
		Path:      "/v1/foo",
		Status:    200,
	})
	sc := bufio.NewScanner(resp.Body)
	got := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "event: request") {
				got <- sc.Text()
				return
			}
		}
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive SSE inspector event")
	}
	cancel()
	resp.Body.Close()
	// Give the handler a moment to unsubscribe, then assert no leak.
	time.Sleep(200 * time.Millisecond)
	if n := ring.SubscriberCountForTest(); n != 0 {
		t.Fatalf("SSE handler leaked subscriber: %d", n)
	}
}

// TestInspectorListInvalidQueryReturns400 covers the basic input validation.
func TestInspectorListInvalidQueryReturns400(t *testing.T) {
	d, _ := inspectorDeps("admin", map[string]string{"svc1": "u-self"}, nil)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	cases := []string{"status=abc", "limit=-1", "since=not-a-date"}
	for _, qs := range cases {
		r := c.get(t, "/api/v1/services/svc1/inspector/requests?"+qs)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("query %q status=%d want 400", qs, r.StatusCode)
		}
		r.Body.Close()
	}
}

// A translated request says so in the list and in the detail: the pair and
// what was left out. A native one has "" and an empty array, never null.
func TestInspectorEntryCarriesTranslation(t *testing.T) {
	d, mgr := inspectorDeps("admin", map[string]string{"svc1": "u-self"}, nil)
	ring := mgr.GetOrCreate("svc1", 10)
	now := time.Now().UTC()
	ring.Capture(inspector.Entry{ID: "ins_n", ServiceID: "svc1", TS: now, Method: "POST", Path: "/v1/messages", Status: 200})
	ring.Capture(inspector.Entry{ID: "ins_t", ServiceID: "svc1", TS: now.Add(time.Second), Method: "POST", Path: "/v1/messages", Status: 200,
		Translated: "messages-chat", Dropped: []string{"cache_control", "top_k"}})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	const translated = `"translated":"messages-chat","dropped":["cache_control","top_k"]`
	const native = `"translated":"","dropped":[]`
	list := readBody(t, c.get(t, "/api/v1/services/svc1/inspector/requests"))
	if !strings.Contains(list, translated) || !strings.Contains(list, native) {
		t.Fatalf("list: %s", list)
	}
	if got := readBody(t, c.get(t, "/api/v1/services/svc1/inspector/requests/ins_t")); !strings.Contains(got, translated) {
		t.Fatalf("translated detail: %s", got)
	}
	if got := readBody(t, c.get(t, "/api/v1/services/svc1/inspector/requests/ins_n")); !strings.Contains(got, native) {
		t.Fatalf("native detail: %s", got)
	}
}
