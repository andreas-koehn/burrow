// Package inspector implements Burrow's in-memory request inspector
// (spec Part E, v0.4.0). It is per-service circular buffer that captures
// request/response pairs flowing through the proxy chain when the service
// has inspector.enabled=true.
//
// Privacy invariant (spec Part E): captures are NEVER persisted to disk
// in v0.4.0. A process restart loses every captured entry. Bodies are
// truncated at 64KB (request) / 256KB (response) and the original payload
// reference is dropped so the runtime can reclaim it.
//
// Storage is per-service; each Ring owns its own RWMutex and a small
// per-service publish/subscribe primitive that the SSE handler subscribes
// to. The Bus is intentionally narrow: each Capture publishes one event to
// every active subscriber; slow subscribers drop events (cap-N buffer with
// non-blocking send) so a stalled client cannot back up Capture.
package inspector

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Body-size caps (spec Part E). Anything beyond these is truncated and the
// original is dropped on the floor — Capture only retains the truncated copy.
const (
	MaxReqBodyBytes  = 64 * 1024
	MaxRespBodyBytes = 256 * 1024
)

// Ring-size bounds (spec Part E). The plan calls for default 100, max 1000.
const (
	DefaultMaxRequests = 100
	MinMaxRequests     = 1
	HardMaxRequests    = 1000
)

// Entry is a single captured request/response pair. Fields mirror spec
// Part E.1 exactly. Bodies are stored as []byte regardless of textual or
// binary content-type; the JSON serializer is the caller's responsibility
// (handlers base64-encode in the wire shape).
type Entry struct {
	ID           string            `json:"id"`
	ServiceID    string            `json:"service_id"`
	APIKeyID     string            `json:"api_key_id,omitempty"`
	TS           time.Time         `json:"ts"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	Status       int               `json:"status"`
	DurationMs   int64             `json:"duration_ms"`
	BytesIn      int64             `json:"bytes_in"`
	BytesOut     int64             `json:"bytes_out"`
	ReqHeaders   map[string]string `json:"req_headers,omitempty"`
	ReqBody      []byte            `json:"req_body,omitempty"`
	RespHeaders  map[string]string `json:"resp_headers,omitempty"`
	RespBody     []byte            `json:"resp_body,omitempty"`
	Truncated    bool              `json:"truncated,omitempty"`
	BytesOmitted int64             `json:"bytes_omitted,omitempty"`
	Cache        string            `json:"cache,omitempty"` // "HIT" | "MISS" | "SKIP"
	Redactions   []RedactionHit    `json:"redactions,omitempty"`
	TraceID      string            `json:"trace_id,omitempty"`
	RemoteIP     string            `json:"remote_ip,omitempty"`
	MCP          *MCPInfo          `json:"mcp,omitempty"`
	AdapterLossy bool              `json:"adapter_lossy,omitempty"`
	// Translated is the id of the pair that translated the request, "" when
	// it was served in its own format. Dropped names what the translation
	// left out; a final "more" says the list was cut and is no name.
	Translated string   `json:"translated"`
	Dropped    []string `json:"dropped"`
}

// RedactionHit is one (rule, count) pair from the redactor's report.
type RedactionHit struct {
	Rule  string `json:"rule"`
	Count int    `json:"count"`
}

// MCPInfo is the parsed MCP envelope detail (spec Part P.1) attached when
// the inspector recognizes a JSON-RPC tools/call, prompts/get, etc.
type MCPInfo struct {
	Method string          `json:"method"`
	Tool   string          `json:"tool,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ListQuery is the filter passed to Ring.List. Empty fields are ignored.
type ListQuery struct {
	Status int       // 0 = no status filter; otherwise exact match
	Q      string    // substring match on Method+Path (case-sensitive)
	Since  time.Time // only entries with TS >= Since when non-zero
	Limit  int       // 0 = no limit
}

// NewID returns a fresh inspector entry id of the form "ins_<rand6>" where
// the random suffix is 6 lowercase base32 characters (5 bits per char,
// 30 bits of entropy — enough for ring identifiers that never persist).
func NewID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	// base32 without padding, lowercased; take first 6 chars.
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	if len(s) > 6 {
		s = s[:6]
	}
	// Lowercase to match the spec example shape "ins_ab12cd".
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		}
	}
	return "ins_" + string(out)
}

// TruncateRequest copies up to MaxReqBodyBytes from src into a fresh slice
// and returns it along with the bytes-omitted accounting. The caller MUST
// pass the returned slice into Entry.ReqBody — never retain src — so the
// original is eligible for GC.
func TruncateRequest(src []byte) (out []byte, omitted int64, truncated bool) {
	return truncate(src, MaxReqBodyBytes)
}

// TruncateResponse mirrors TruncateRequest with the 256KB response cap.
func TruncateResponse(src []byte) (out []byte, omitted int64, truncated bool) {
	return truncate(src, MaxRespBodyBytes)
}

func truncate(src []byte, cap int) ([]byte, int64, bool) {
	if len(src) <= cap {
		// Copy to a fresh slice so caller mutations cannot corrupt the
		// entry, and so the source slice's backing array can be GC'd if
		// the caller drops its reference.
		out := make([]byte, len(src))
		copy(out, src)
		return out, 0, false
	}
	out := make([]byte, cap)
	copy(out, src[:cap])
	return out, int64(len(src) - cap), true
}

// Bus is a small per-service publish/subscribe primitive. Each Subscribe
// returns a buffered channel; Publish does a non-blocking send to every
// subscriber (slow subscribers drop events). The events.Bus in v0.2.0 is
// content-free (signals only); we need to carry Entry payloads so we
// implement a tiny inspector-local fanout instead of reusing it.
type bus struct {
	mu   sync.Mutex
	subs map[*sub]struct{}
}

type sub struct{ ch chan Entry }

// busNew constructs an empty Bus.
func busNew() *bus { return &bus{subs: make(map[*sub]struct{})} }

// Subscribe registers a new subscriber and returns its receive channel
// plus an idempotent cancel that unsubscribes and closes the channel.
// Buffer size of 16 absorbs short bursts without blocking Publish.
func (b *bus) subscribe() (<-chan Entry, func()) {
	s := &sub{ch: make(chan Entry, 16)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			b.mu.Unlock()
			close(s.ch)
		})
	}
	return s.ch, cancel
}

// publish does a non-blocking send to every subscriber. Slow subscribers
// drop events so the proxy hot path never blocks on a stalled SSE client.
func (b *bus) publish(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.ch <- e:
		default:
			// drop; the subscriber will see the next event.
		}
	}
}

// subscriberCount is a test helper.
func (b *bus) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Ring is a per-service in-memory circular buffer of inspector entries.
type Ring struct {
	mu     sync.RWMutex
	max    int
	buf    []Entry
	next   int  // index of the next write slot
	filled bool // true once the buffer has wrapped at least once
	bus    *bus
}

// NewRing returns a Ring with capacity max (clamped to [Min, HardMax] and
// defaulted to DefaultMaxRequests when max <= 0).
func NewRing(max int) *Ring {
	if max <= 0 {
		max = DefaultMaxRequests
	}
	if max < MinMaxRequests {
		max = MinMaxRequests
	}
	if max > HardMaxRequests {
		max = HardMaxRequests
	}
	return &Ring{
		max: max,
		buf: make([]Entry, max),
		bus: busNew(),
	}
}

// Cap returns the configured capacity.
func (r *Ring) Cap() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.max
}

// Len returns the number of entries currently stored (<= Cap).
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.filled {
		return r.max
	}
	return r.next
}

// Capture stores e at the current write slot, advances the cursor, and
// publishes the event to every subscriber. When the ring is full the
// oldest entry is silently overwritten — the spec's privacy guarantee
// (no disk persistence) means an overwritten entry is gone forever.
func (r *Ring) Capture(e Entry) {
	r.mu.Lock()
	r.buf[r.next] = e
	r.next++
	if r.next >= r.max {
		r.next = 0
		r.filled = true
	}
	r.mu.Unlock()
	r.bus.publish(e)
}

// List returns the matching entries in descending TS order, honouring the
// q.Limit cap. The returned slice is a fresh copy — callers may mutate it
// freely without affecting the ring.
func (r *Ring) List(q ListQuery) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, r.lenLocked())
	for _, e := range r.snapshotLocked() {
		if q.Status != 0 && e.Status != q.Status {
			continue
		}
		if q.Q != "" && !matchQ(e, q.Q) {
			continue
		}
		if !q.Since.IsZero() && e.TS.Before(q.Since) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// Get returns the entry with the given id and whether it was found.
func (r *Ring) Get(id string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.snapshotLocked() {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Subscribe returns a per-subscriber channel of newly-captured Entries
// plus an idempotent cancel.
func (r *Ring) Subscribe() (<-chan Entry, func()) {
	return r.bus.subscribe()
}

// Resize changes the ring capacity, copying the most recent
// min(old.Len(), n) entries into a fresh ring. Existing subscribers stay
// attached to the same bus.
func (r *Ring) Resize(n int) {
	if n <= 0 {
		n = DefaultMaxRequests
	}
	if n < MinMaxRequests {
		n = MinMaxRequests
	}
	if n > HardMaxRequests {
		n = HardMaxRequests
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n == r.max {
		return
	}
	// snapshot in ascending TS order so the new buffer's recency invariant
	// holds (the most-recent entries are the last to be written).
	snap := r.snapshotLocked()
	// snapshotLocked returns the buffer in chronological order (oldest →
	// newest), so the last n of snap are the most recent.
	if len(snap) > n {
		snap = snap[len(snap)-n:]
	}
	r.buf = make([]Entry, n)
	r.max = n
	r.next = 0
	r.filled = false
	for _, e := range snap {
		r.buf[r.next] = e
		r.next++
		if r.next >= r.max {
			r.next = 0
			r.filled = true
		}
	}
}

// SubscriberCountForTest exposes the bus subscriber count to package tests
// in other packages.
func (r *Ring) SubscriberCountForTest() int { return r.bus.subscriberCount() }

// lenLocked must be called with r.mu held (RLock or Lock).
func (r *Ring) lenLocked() int {
	if r.filled {
		return r.max
	}
	return r.next
}

// snapshotLocked returns the entries in chronological order (oldest →
// newest). Caller must hold r.mu (RLock or Lock).
func (r *Ring) snapshotLocked() []Entry {
	n := r.lenLocked()
	out := make([]Entry, 0, n)
	if !r.filled {
		out = append(out, r.buf[:r.next]...)
		return out
	}
	// Wrapped: oldest is at r.next, newest is at (r.next-1+max) % max.
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	return out
}

// matchQ reports whether the q substring matches the entry's method+path.
func matchQ(e Entry, q string) bool {
	if q == "" {
		return true
	}
	return contains(e.Method, q) || contains(e.Path, q)
}

// contains is a hand-rolled substring check (no strings import to keep this
// file's surface area tight). Case-sensitive — spec doesn't mandate
// case-insensitive.
func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Manager holds one Ring per service id. It lazily creates a Ring on first
// access via GetOrCreate. The manager is concurrency-safe; concurrent calls
// for the same service id return the same *Ring.
type Manager struct {
	mu    sync.RWMutex
	rings map[string]*Ring
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{rings: make(map[string]*Ring)}
}

// GetOrCreate returns the Ring for serviceID, creating one with the given
// capacity if it does not yet exist. Capacity is honoured only on creation;
// callers may use Resize to change an existing ring's capacity.
func (m *Manager) GetOrCreate(serviceID string, maxRequests int) *Ring {
	m.mu.RLock()
	r, ok := m.rings[serviceID]
	m.mu.RUnlock()
	if ok {
		return r
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok = m.rings[serviceID]; ok {
		return r
	}
	r = NewRing(maxRequests)
	m.rings[serviceID] = r
	return r
}

// Get returns the existing Ring for serviceID, or nil when none exists.
// Useful for read-only paths (list/get/stream) that must not allocate a
// Ring just because the URL was hit.
func (m *Manager) Get(serviceID string) *Ring {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rings[serviceID]
}
