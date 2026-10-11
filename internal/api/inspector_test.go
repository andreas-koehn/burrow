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
	"github.com/ankoehn/burrow/internal/store"
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

// The per-service list names the provider, the model and the gateway key of
// each request, and still carries everything it did.
func TestInspectorListCarriesProviderModelKey(t *testing.T) {
	d, mgr := inspectorDeps("admin", map[string]string{"svc1": "u-self"}, nil)
	ring := mgr.GetOrCreate("svc1", 10)
	now := time.Now().UTC()
	ring.Capture(inspector.Entry{ID: "ins_old", ServiceID: "svc1", TS: now, Method: "POST", Path: "/v1/messages", Status: 200})
	ring.Capture(inspector.Entry{
		ID: "ins_full", ServiceID: "svc1", APIKeyID: "key1", TS: now.Add(time.Second),
		Method: "POST", Path: "/v1/chat/completions", Status: 200, DurationMs: 12, BytesIn: 3, BytesOut: 4,
		ReqHeaders: map[string]string{"Content-Type": "application/json"}, ReqBody: []byte(`{"a":1}`),
		RespHeaders: map[string]string{"Content-Type": "application/json"}, RespBody: []byte(`{"b":2}`),
		Truncated: true, BytesOmitted: 9, Cache: "MISS",
		Redactions: []inspector.RedactionHit{{Rule: "email", Count: 1}},
		TraceID:    "trace1", RemoteIP: "203.0.113.7",
		MCP:          &inspector.MCPInfo{Method: "tools/call", Tool: "search"},
		AdapterLossy: true, Translated: "messages-chat", Dropped: []string{"top_k"},
		Provider: "zai", Model: "burrow-smart", GatewayKeyID: "gk1",
	})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	var got []map[string]any
	if err := json.Unmarshal([]byte(readBody(t, c.get(t, "/api/v1/services/svc1/inspector/requests"))), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	full, old := got[0], got[1]
	if full["provider"] != "zai" || full["model"] != "burrow-smart" || full["gateway_key_id"] != "gk1" {
		t.Errorf("full entry: provider=%v model=%v gateway_key_id=%v", full["provider"], full["model"], full["gateway_key_id"])
	}
	for _, k := range []string{
		"id", "service_id", "api_key_id", "ts", "method", "path", "status", "duration_ms", "bytes_in", "bytes_out",
		"req_headers", "req_body", "req_body_encoding", "resp_headers", "resp_body", "resp_body_encoding",
		"truncated", "bytes_omitted", "cache", "redactions", "trace_id", "remote_ip", "mcp", "adapter_lossy",
		"translated", "dropped", "provider", "model", "gateway_key_id",
	} {
		if _, ok := full[k]; !ok {
			t.Errorf("full entry lacks %q", k)
		}
	}
	// An entry without a route has the three keys too, as empty strings.
	for _, k := range []string{"provider", "model", "gateway_key_id"} {
		if v, ok := old[k]; !ok || v != "" {
			t.Errorf("entry without a route: %q = %v (present %v), want an empty string", k, v, ok)
		}
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/ai/requests: captured requests across providers
// ---------------------------------------------------------------------------

// recordingRings is an InspectorRings that notes which services' rings were
// asked for. Tests fill rings through the embedded Manager, which is not
// recorded.
type recordingRings struct {
	*inspector.Manager
	mu   sync.Mutex
	read []string
}

func (r *recordingRings) note(serviceID string) {
	r.mu.Lock()
	r.read = append(r.read, serviceID)
	r.mu.Unlock()
}

// reads returns the services asked for so far and forgets them.
func (r *recordingRings) reads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.read
	r.read = nil
	return out
}

func (r *recordingRings) Get(serviceID string) *inspector.Ring {
	r.note(serviceID)
	return r.Manager.Get(serviceID)
}

func (r *recordingRings) GetOrCreate(serviceID string, max int) *inspector.Ring {
	r.note(serviceID)
	return r.Manager.GetOrCreate(serviceID, max)
}

func (r *recordingRings) seed(serviceID string, entries ...inspector.Entry) {
	ring := r.Manager.GetOrCreate(serviceID, inspector.HardMaxRequests)
	for _, e := range entries {
		e.ServiceID = serviceID
		if e.Method == "" {
			e.Method, e.Path, e.Status = "POST", "/v1/chat/completions", 200
		}
		ring.Capture(e)
	}
}

// aiRequestsDeps is the common setup: provider "ollama" on svc-ollama and
// provider "zai" on svc-zai, both in the caller's service list. owners says
// who owns which service; the caller is "u-self".
func aiRequestsDeps(role string, owners map[string]string) (Deps, *recordingRings) {
	ss := &fakeServiceStore{listSvcs: []store.ServiceView{
		{ID: "svc-ollama", Name: "ollama", Type: "http", AccessMode: "api_key"},
		{ID: "svc-zai", Name: "zai", Type: "http", AccessMode: "api_key"},
	}}
	ps := &fakeProviderStore{rows: []db.AIProvider{
		tunnelProvider("ollama", "Ollama", "svc-ollama"),
		tunnelProvider("zai", "Z.ai", "svc-zai"),
	}}
	d := newAIProviderDeps(ss, &fakeModelStore{}, ps)
	d.Users = &fakeUserStore{role: role}
	rings := &recordingRings{Manager: inspector.NewManager()}
	d.InspectorRings = rings
	d.InspectorServices = &fakeInspectorOwner{owners: owners}
	return d, rings
}

var bothMine = map[string]string{"svc-ollama": "u-self", "svc-zai": "u-self"}

// aiRequestsT0 is the time of the oldest seeded entry.
var aiRequestsT0 = time.Unix(1_700_000_000, 0).UTC()

// seedTwoProviders captures z0, o1, z2, o3 (oldest to newest), alternating
// between zai and ollama.
func seedTwoProviders(rings *recordingRings) {
	at := func(n int) time.Time { return aiRequestsT0.Add(time.Duration(n) * time.Minute) }
	rings.seed("svc-zai",
		inspector.Entry{ID: "z0", TS: at(0), Provider: "zai", Model: "burrow-smart", GatewayKeyID: "gk1", Status: 200, Method: "POST", Path: "/v1/chat/completions"},
		inspector.Entry{ID: "z2", TS: at(2), Provider: "zai", Model: "zai/glm-5.1", GatewayKeyID: "gk2", Status: 500, Method: "POST", Path: "/v1/chat/completions"},
	)
	rings.seed("svc-ollama",
		inspector.Entry{ID: "o1", TS: at(1), Provider: "ollama", Model: "burrow-smart", GatewayKeyID: "gk2", Status: 200, Method: "POST", Path: "/v1/embeddings"},
		inspector.Entry{ID: "o3", TS: at(3), Provider: "ollama", Model: "ollama/qwen", GatewayKeyID: "gk1", Status: 200, Method: "POST", Path: "/v1/chat/completions"},
	)
}

// aiRequestIDs GETs /api/v1/ai/requests with the query, wants 200 and returns
// the entry ids joined by ",".
func aiRequestIDs(t *testing.T, c *authClient, query string) string {
	t.Helper()
	return strings.Join(entryIDs(aiRequests(t, c, query)), ",")
}

func aiRequests(t *testing.T, c *authClient, query string) []inspectorEntryJSON {
	t.Helper()
	path := "/api/v1/ai/requests"
	if query != "" {
		path += "?" + query
	}
	r := c.get(t, path)
	body := readBody(t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d body %s", path, r.StatusCode, body)
	}
	var got []inspectorEntryJSON
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("GET %s: %v in %s", path, err, body)
	}
	if got == nil {
		t.Fatalf("GET %s: body %s, want an array", path, body)
	}
	return got
}

func entryIDs(es []inspectorEntryJSON) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

// Rule 1: the entries of every provider come as one list, newest first.
func TestAIRequestsMergesProvidersNewestFirst(t *testing.T) {
	d, rings := aiRequestsDeps("admin", bothMine)
	seedTwoProviders(rings)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	got := aiRequests(t, c, "")
	if ids := strings.Join(entryIDs(got), ","); ids != "o3,z2,o1,z0" {
		t.Fatalf("got %q want o3,z2,o1,z0", ids)
	}
	if e := got[1]; e.Provider != "zai" || e.Model != "zai/glm-5.1" || e.GatewayKeyID != "gk2" || e.ServiceID != "svc-zai" {
		t.Errorf("z2: provider=%q model=%q gateway_key_id=%q service_id=%q", e.Provider, e.Model, e.GatewayKeyID, e.ServiceID)
	}
}

// Rule 2: provider= returns that provider's entries and reads no other ring.
func TestAIRequestsProviderFilterReadsOneRing(t *testing.T) {
	d, rings := aiRequestsDeps("admin", bothMine)
	seedTwoProviders(rings)
	// A request that came for zai and was answered by ollama (a fallback)
	// sits in zai's ring and is not one of zai's.
	rings.seed("svc-zai", inspector.Entry{ID: "zf", TS: aiRequestsT0.Add(4 * time.Minute), Provider: "ollama", Model: "burrow-smart"})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	if ids := aiRequestIDs(t, c, "provider=zai"); ids != "z2,z0" {
		t.Fatalf("provider=zai: got %q want z2,z0", ids)
	}
	if got := strings.Join(rings.reads(), ","); got != "svc-zai" {
		t.Fatalf("rings read: %q, want svc-zai only", got)
	}

	if ids := aiRequestIDs(t, c, "provider=nope"); ids != "" {
		t.Fatalf("unknown provider: got %q want nothing", ids)
	}
	if read := rings.reads(); len(read) != 0 {
		t.Fatalf("unknown provider read rings %v", read)
	}
}

// Rule 3: model= and gateway_key= match exactly; an unknown value is an
// empty list, not an error.
func TestAIRequestsModelAndGatewayKeyFilters(t *testing.T) {
	d, rings := aiRequestsDeps("admin", bothMine)
	seedTwoProviders(rings)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	for query, want := range map[string]string{
		"model=burrow-smart":                               "o1,z0",
		"model=burrow":                                     "",
		"model=zai%2Fglm-5.1":                              "z2",
		"gateway_key=gk1":                                  "o3,z0",
		"gateway_key=gk":                                   "",
		"gateway_key=unknown":                              "",
		"model=burrow-smart&gateway_key=gk2":               "o1",
		"provider=zai&model=burrow-smart&gateway_key=gk1":  "z0",
		"provider=zai&model=burrow-smart&gateway_key=gk2":  "",
		"provider=ollama&model=nope&gateway_key=also-nope": "",
	} {
		if got := aiRequestIDs(t, c, query); got != want {
			t.Errorf("%s: got %q want %q", query, got, want)
		}
	}
	if body := strings.TrimSpace(readBody(t, c.get(t, "/api/v1/ai/requests?model=nope"))); body != "[]" {
		t.Errorf("unknown model: body %s, want []", body)
	}
}

// Rule 4: since, until, status, q and limit work as on the per-service list,
// and a malformed one is refused with the same message.
func TestAIRequestsQueryFilters(t *testing.T) {
	d, rings := aiRequestsDeps("admin", bothMine)
	seedTwoProviders(rings)
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	at := func(n int) string { return aiRequestsT0.Add(time.Duration(n) * time.Minute).Format(time.RFC3339) }
	for query, want := range map[string]string{
		"since=" + at(2):                     "o3,z2",
		"until=" + at(1):                     "o1,z0",
		"since=" + at(1) + "&until=" + at(2): "z2,o1",
		"status=500":                         "z2",
		"status=404":                         "",
		"q=embeddings":                       "o1",
		"q=ollama":                           "o3,o1",
		"q=glm":                              "z2",
		"limit=3":                            "o3,z2,o1",
		"limit=1&provider=zai":               "z2",
	} {
		if got := aiRequestIDs(t, c, query); got != want {
			t.Errorf("%s: got %q want %q", query, got, want)
		}
	}

	for _, query := range []string{"status=abc", "status=99", "since=not-a-date", "until=not-a-date", "limit=abc", "limit=-1"} {
		across := c.get(t, "/api/v1/ai/requests?"+query)
		acrossBody := readBody(t, across)
		if across.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d body %s, want 400", query, across.StatusCode, acrossBody)
			continue
		}
		one := c.get(t, "/api/v1/services/svc-zai/inspector/requests?"+query)
		oneBody := readBody(t, one)
		if one.StatusCode != http.StatusBadRequest || oneBody != acrossBody {
			t.Errorf("%s: per-service list answers %d %s, the list across providers %s", query, one.StatusCode, oneBody, acrossBody)
		}
	}
	if body := readBody(t, c.get(t, "/api/v1/ai/requests?until=nope")); !strings.Contains(body, "invalid until") {
		t.Errorf("malformed until: body %s", body)
	}
	// The per-service list knows until as well.
	var one []inspectorEntryJSON
	if err := json.Unmarshal([]byte(readBody(t, c.get(t, "/api/v1/services/svc-zai/inspector/requests?until="+at(1)))), &one); err != nil {
		t.Fatal(err)
	}
	if ids := strings.Join(entryIDs(one), ","); ids != "z0" {
		t.Errorf("per-service until: got %q want z0", ids)
	}
}

// Rule 5: an entry is returned only to a caller who may read its service's
// inspector. A user sees the requests of their own providers and never
// another user's, even when the service list shows both.
func TestAIRequestsPermission(t *testing.T) {
	t.Run("a user gets their own providers only", func(t *testing.T) {
		d, rings := aiRequestsDeps("user", map[string]string{"svc-zai": "u-self", "svc-ollama": "u-other"})
		seedTwoProviders(rings)
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		c := authedClient(t, srv)

		for _, query := range []string{"", "provider=ollama", "model=burrow-smart", "gateway_key=gk1", "q=ollama", "limit=1000"} {
			for _, e := range aiRequests(t, c, query) {
				if e.ServiceID != "svc-zai" || e.Provider != "zai" {
					t.Errorf("query %q returned another user's entry %s (service %s, provider %s)", query, e.ID, e.ServiceID, e.Provider)
				}
			}
		}
		if ids := aiRequestIDs(t, c, ""); ids != "z2,z0" {
			t.Errorf("own entries: got %q want z2,z0", ids)
		}
		for _, id := range rings.reads() {
			if id != "svc-zai" {
				t.Errorf("read the ring of %s, which the caller does not own", id)
			}
		}
	})
	t.Run("a service nobody is known to own is left out", func(t *testing.T) {
		d, rings := aiRequestsDeps("user", map[string]string{"svc-zai": "u-self"})
		seedTwoProviders(rings)
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		if ids := aiRequestIDs(t, authedClient(t, srv), ""); ids != "z2,z0" {
			t.Errorf("got %q want z2,z0", ids)
		}
	})
	t.Run("an admin gets every provider", func(t *testing.T) {
		d, rings := aiRequestsDeps("admin", map[string]string{"svc-zai": "u-other", "svc-ollama": "u-other"})
		seedTwoProviders(rings)
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		if ids := aiRequestIDs(t, authedClient(t, srv), ""); ids != "o3,z2,o1,z0" {
			t.Errorf("got %q want o3,z2,o1,z0", ids)
		}
	})
	t.Run("without a read permission", func(t *testing.T) {
		d, rings := aiRequestsDeps("no-such-role", bothMine)
		seedTwoProviders(rings)
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		r := authedClient(t, srv).get(t, "/api/v1/ai/requests")
		if body := readBody(t, r); r.StatusCode != http.StatusForbidden || strings.Contains(body, "z0") {
			t.Fatalf("status %d body %s, want 403", r.StatusCode, body)
		}
		if read := rings.reads(); len(read) != 0 {
			t.Errorf("read rings %v", read)
		}
	})
	t.Run("without a session", func(t *testing.T) {
		d, _ := aiRequestsDeps("admin", bothMine)
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		r, err := http.Get(srv.URL + "/api/v1/ai/requests")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", r.StatusCode)
		}
	})
}

// Rule 6: the list carries neither bodies nor headers; the detail does.
func TestAIRequestsLeavesOutBodiesAndHeaders(t *testing.T) {
	d, rings := aiRequestsDeps("admin", bothMine)
	rings.seed("svc-zai", inspector.Entry{
		ID: "zb", TS: aiRequestsT0, Provider: "zai", Method: "POST", Path: "/v1/chat/completions", Status: 200,
		ReqHeaders: map[string]string{"X-Marker": "request-header-value"}, ReqBody: []byte("request-body-value"),
		RespHeaders: map[string]string{"X-Marker": "response-header-value"}, RespBody: []byte("response-body-value"),
		BytesIn: 18, BytesOut: 19,
	})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	body := readBody(t, c.get(t, "/api/v1/ai/requests"))
	var got []map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil || len(got) != 1 {
		t.Fatalf("list: %v %s", err, body)
	}
	for _, k := range []string{"req_body", "resp_body", "req_headers", "resp_headers", "req_body_encoding", "resp_body_encoding"} {
		if _, ok := got[0][k]; ok {
			t.Errorf("list entry carries %q", k)
		}
	}
	for _, v := range []string{"request-header-value", "request-body-value", "response-header-value", "response-body-value"} {
		if strings.Contains(body, v) {
			t.Errorf("list carries %q", v)
		}
	}
	if got[0]["bytes_in"] != float64(18) || got[0]["bytes_out"] != float64(19) {
		t.Errorf("sizes: bytes_in=%v bytes_out=%v", got[0]["bytes_in"], got[0]["bytes_out"])
	}
	detail := readBody(t, c.get(t, "/api/v1/services/svc-zai/inspector/requests/zb"))
	if !strings.Contains(detail, "request-body-value") || !strings.Contains(detail, "response-header-value") {
		t.Errorf("detail lost bodies or headers: %s", detail)
	}
}

// Rule 7: no provider, or no ring manager, is an empty list.
func TestAIRequestsEmpty(t *testing.T) {
	want := func(t *testing.T, d Deps) {
		t.Helper()
		srv := httptest.NewServer(NewRouter(d))
		defer srv.Close()
		r := authedClient(t, srv).get(t, "/api/v1/ai/requests")
		if body := strings.TrimSpace(readBody(t, r)); r.StatusCode != http.StatusOK || body != "[]" {
			t.Fatalf("status %d body %s, want 200 []", r.StatusCode, body)
		}
	}
	t.Run("no providers", func(t *testing.T) {
		d, rings := aiRequestsDeps("admin", bothMine)
		seedTwoProviders(rings)
		d.AIProviders = &fakeProviderStore{}
		want(t, d)
	})
	t.Run("no ring manager", func(t *testing.T) {
		d, _ := aiRequestsDeps("admin", bothMine)
		d.InspectorRings = nil
		want(t, d)
	})
	t.Run("nothing captured", func(t *testing.T) {
		d, rings := aiRequestsDeps("admin", bothMine)
		want(t, d)
		// Looking must not create a ring: the chain creates it with the
		// capacity the service is configured with.
		if rings.Manager.Get("svc-zai") != nil || rings.Manager.Get("svc-ollama") != nil {
			t.Error("the list created a ring")
		}
	})
}

// Rule 8: an entry that names no provider (captured before entries did, or
// for a request without a route) is given the slug of the provider whose ring
// holds it.
func TestAIRequestsNamesProviderOfOldEntries(t *testing.T) {
	d, rings := aiRequestsDeps("admin", bothMine)
	rings.seed("svc-zai", inspector.Entry{ID: "old", TS: aiRequestsT0})
	rings.seed("svc-ollama", inspector.Entry{ID: "new", TS: aiRequestsT0.Add(time.Minute), Provider: "ollama"})
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	got := aiRequests(t, c, "")
	if len(got) != 2 || got[1].ID != "old" || got[1].Provider != "zai" || got[1].Model != "" || got[1].GatewayKeyID != "" {
		t.Fatalf("list: %+v", got)
	}
	if got := aiRequests(t, c, "provider=zai"); len(got) != 1 || got[0].ID != "old" || got[0].Provider != "zai" {
		t.Fatalf("provider=zai: %+v", got)
	}
	// The ring's own copy is untouched.
	if e, _ := rings.Manager.Get("svc-zai").Get("old"); e.Provider != "" {
		t.Errorf("the ring's entry was changed: provider %q", e.Provider)
	}
}

// Rule 9: the list is 200 long unless limit says otherwise, never longer
// than 1000, and limit must be a positive number.
func TestAIRequestsLimit(t *testing.T) {
	d, rings := aiRequestsDeps("admin", bothMine)
	for i := 0; i < 600; i++ {
		ts := aiRequestsT0.Add(time.Duration(i) * time.Second)
		rings.seed("svc-zai", inspector.Entry{ID: fmt.Sprintf("z%d", i), TS: ts, Provider: "zai"})
		rings.seed("svc-ollama", inspector.Entry{ID: fmt.Sprintf("o%d", i), TS: ts.Add(time.Millisecond), Provider: "ollama"})
	}
	srv := httptest.NewServer(NewRouter(d))
	defer srv.Close()
	c := authedClient(t, srv)

	for query, want := range map[string]int{"": 200, "limit=5": 5, "limit=1000": 1000, "limit=1001": 1000, "limit=5000": 1000} {
		got := aiRequests(t, c, query)
		if len(got) != want {
			t.Errorf("%q: %d entries, want %d", query, len(got), want)
			continue
		}
		// The newest ones, from both providers.
		if got[0].ID != "o599" || got[1].ID != "z599" {
			t.Errorf("%q: starts with %s, %s; want o599, z599", query, got[0].ID, got[1].ID)
		}
	}
	for _, query := range []string{"limit=0", "limit=-1", "limit=-5000"} {
		r := c.get(t, "/api/v1/ai/requests?"+query)
		if body := readBody(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "invalid limit") {
			t.Errorf("%s: status %d body %s, want 400 invalid limit", query, r.StatusCode, body)
		}
	}
	// The per-service list keeps its own rule: limit=0 means no limit.
	if r := c.get(t, "/api/v1/services/svc-zai/inspector/requests?limit=0"); r.StatusCode != http.StatusOK {
		t.Errorf("per-service limit=0: status %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
}
