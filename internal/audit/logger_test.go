package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// newTestDB returns a fresh migrated *db.DB backed by a temp-dir sqlite
// file. The Logger's hash chain needs the real audit_events schema (its
// 14-column INSERT is type-checked at runtime), so we don't fake the DB.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	x := db.Wrap(d)
	t.Cleanup(func() { _ = x.Close() })
	return x
}

// newTestLogger returns a Logger with a freshly-generated key and discard
// slog. Tests that need to verify the public key use l.PublicKey().
func newTestLogger(t *testing.T, x *db.DB) *Logger {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return NewLogger(x, priv, slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
}

func appendN(t *testing.T, l *Logger, n int, actorEmail string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Append(context.Background(), Event{
			ActorID: "u-actor", ActorEmail: actorEmail, Action: ActionUserCreate,
			SubjectID: "u-sub", SubjectLabel: "new@x", Result: "ok",
			Payload: json.RawMessage(`{"role":"user"}`),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// TestAppendThenVerifyOK appends three events and asserts the whole-chain
// Verify() returns ok=true.
func TestAppendThenVerifyOK(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	appendN(t, l, 3, "admin@x")
	ok, mismatched, err := l.Verify(context.Background(), "", "")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("want ok=true, got ok=false mismatched=%s", mismatched)
	}
	if mismatched != "" {
		t.Fatalf("want mismatched empty, got %q", mismatched)
	}
}

// TestVerifyTamperDetected modifies row 2's payload via the test-only
// TamperAuditPayload helper and asserts Verify reports row 2's id as
// mismatched.
func TestVerifyTamperDetected(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	appendN(t, l, 3, "admin@x")

	rows, err := x.ListAuditEvents(context.Background(), db.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// ListAuditEvents is id DESC; sort ASC so rows[1] is the chronological
	// second event.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	row2 := rows[1]
	if err := x.TamperAuditPayload(context.Background(), row2.ID, `{"role":"admin"}`); err != nil {
		t.Fatal(err)
	}

	ok, mismatched, err := l.Verify(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("want ok=false after tamper")
	}
	if mismatched != row2.ID {
		t.Fatalf("want mismatched=%s, got %s", row2.ID, mismatched)
	}
}

// TestExportNDJSONTrailerSignature exports the chain and asserts the
// trailer signature verifies against the public key.
func TestExportNDJSONTrailerSignature(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	appendN(t, l, 3, "admin@x")

	var buf bytes.Buffer
	if err := l.ExportNDJSON(context.Background(), &buf, ExportQuery{}); err != nil {
		t.Fatal(err)
	}

	// Verify shape: 4 lines (3 events + 1 trailer), trailing newline.
	out := buf.Bytes()
	lines := bytes.Split(out, []byte{'\n'})
	for len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != 4 {
		t.Fatalf("want 4 NDJSON lines (3 events + trailer), got %d:\n%s", len(lines), out)
	}
	// Last line is the trailer.
	var tr trailer
	if err := json.Unmarshal(lines[len(lines)-1], &tr); err != nil {
		t.Fatalf("decode trailer: %v", err)
	}
	if tr.Signature == "" || tr.Fingerprint == "" {
		t.Fatalf("trailer missing fields: %+v", tr)
	}

	ok, firstID, lastID, mismatched, err := VerifySignedExport(bytes.NewReader(out), l.PublicKey())
	if err != nil {
		t.Fatalf("verify export: %v", err)
	}
	if !ok {
		t.Fatalf("export verify failed: mismatched=%s", mismatched)
	}
	if firstID == "" || lastID == "" || firstID == lastID {
		t.Fatalf("first/last empty or equal: first=%s last=%s", firstID, lastID)
	}
}

// TestExportNDJSONSignatureTamperRejected flips one byte in the body and
// asserts VerifySignedExport rejects it.
func TestExportNDJSONSignatureTamperRejected(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	appendN(t, l, 2, "admin@x")

	var buf bytes.Buffer
	if err := l.ExportNDJSON(context.Background(), &buf, ExportQuery{}); err != nil {
		t.Fatal(err)
	}
	// Flip a character inside the first event line (changing payload "ok"
	// to "Ok" so JSON still parses but the signed body differs).
	out := buf.Bytes()
	idx := bytes.Index(out, []byte(`"role":"user"`))
	if idx < 0 {
		t.Fatalf("expected role marker in export, got %s", out)
	}
	out[idx+9] = 'X' // "user" -> "uXer" — still valid JSON, breaks signed body
	ok, _, _, _, err := VerifySignedExport(bytes.NewReader(out), l.PublicKey())
	if err != nil {
		t.Fatalf("verify export (tamper): %v", err)
	}
	if ok {
		t.Fatalf("want ok=false after body tamper")
	}
}

// TestSampleRateRedactionApplied appends 10 redaction.applied events for
// the same subject within one minute (via injected now()) and asserts only
// one row was actually persisted.
func TestSampleRateRedactionApplied(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	calls := 0
	l.now = func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * time.Second) // +1s per call
	}

	for i := 0; i < 10; i++ {
		if err := l.Append(context.Background(), Event{
			Action: ActionRedactionApplied, SubjectID: "svc-1",
			SubjectLabel: "rule-a", Result: "ok",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	rows, err := x.ListAuditEvents(context.Background(), db.AuditQuery{
		Action: ActionRedactionApplied, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 aggregated row, got %d", len(rows))
	}
}

// TestSampleRatePerSubjectIndependent asserts that different subject_ids
// don't share an aggregation bucket.
func TestSampleRatePerSubjectIndependent(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	for _, svc := range []string{"svc-a", "svc-b", "svc-c"} {
		if err := l.Append(context.Background(), Event{
			Action: ActionRedactionApplied, SubjectID: svc,
			SubjectLabel: "rule", Result: "ok",
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := x.ListAuditEvents(context.Background(), db.AuditQuery{
		Action: ActionRedactionApplied, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (one per subject), got %d", len(rows))
	}
}

// TestCanonicalSortsPayloadKeys asserts that two payloads with the same
// content but different key order canonicalise to the same bytes.
func TestCanonicalSortsPayloadKeys(t *testing.T) {
	ts := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	a := Event{
		ID: "01", Action: "x", TS: ts, Result: "ok",
		Payload: json.RawMessage(`{"b":2,"a":1}`),
	}
	b := Event{
		ID: "01", Action: "x", TS: ts, Result: "ok",
		Payload: json.RawMessage(`{"a":1,"b":2}`),
	}
	ca, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca, cb) {
		t.Fatalf("canonical mismatch:\na=%s\nb=%s", ca, cb)
	}
}

// TestGenesisPrevHashIsZero asserts the first row in a fresh DB has
// prev_hash = 64 zero hex chars (the genesis sentinel).
func TestGenesisPrevHashIsZero(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	appendN(t, l, 1, "admin@x")
	rows, err := x.ListAuditEvents(context.Background(), db.AuditQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].PrevHash != genesisPrevHash {
		t.Fatalf("want genesis prev_hash, got %s", rows[0].PrevHash)
	}
}

// TestLoadOrGenerateSigningKey_PersistsAcrossLoads asserts the key is
// stable across calls (first call generates, second call loads).
func TestLoadOrGenerateSigningKey_PersistsAcrossLoads(t *testing.T) {
	ss := &fakeSettings{m: map[string]string{}}
	a, err := LoadOrGenerateSigningKey(context.Background(), ss)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrGenerateSigningKey(context.Background(), ss)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("key not stable across loads")
	}
	// And the saved row is base64 (string-decodable, right length).
	if !strings.HasPrefix(ss.m[SettingsKey], "") || len(ss.m[SettingsKey]) < 80 {
		t.Fatalf("persisted key looks wrong: %q", ss.m[SettingsKey])
	}
}

type fakeSettings struct{ m map[string]string }

func (f *fakeSettings) GetSettings(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(f.m))
	for k, v := range f.m {
		out[k] = v
	}
	return out, nil
}
func (f *fakeSettings) SaveSettings(_ context.Context, kv map[string]string) error {
	for k, v := range kv {
		f.m[k] = v
	}
	return nil
}

func countRows(t *testing.T, x *db.DB, action string) int {
	t.Helper()
	rows, err := x.ListAuditEvents(context.Background(), db.AuditQuery{Action: action, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// An event with an AggregationKey is sampled by that key instead of by its
// subject: one row per key per window. Events without one keep the subject.
func TestSampleRateAggregationKey(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	for _, key := range []string{"svc-1|pattern-a", "svc-1|pattern-a", "svc-1|pattern-b", "svc-1|pattern-a"} {
		if err := l.Append(context.Background(), Event{
			Action: ActionGuardrailRefused, SubjectID: "svc-1", Result: "denied", AggregationKey: key,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, x, ActionGuardrailRefused); n != 2 {
		t.Fatalf("%d rows, want 2 (one per key)", n)
	}
	// A keyed event and an unkeyed one never share a slot, whatever the key says.
	for _, e := range []Event{
		{Action: ActionRedactionApplied, SubjectID: "svc-1"},
		{Action: ActionRedactionApplied, SubjectID: "other", AggregationKey: "svc-1"},
		{Action: ActionRedactionApplied, SubjectID: "svc-1"},
	} {
		if err := l.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, x, ActionRedactionApplied); n != 2 {
		t.Fatalf("%d redaction rows, want 2", n)
	}
	// After the window the key is written again.
	l.now = func() time.Time { return time.Now().UTC().Add(AggregationWindow + time.Second) }
	if err := l.Append(context.Background(), Event{
		Action: ActionGuardrailRefused, SubjectID: "svc-1", AggregationKey: "svc-1|pattern-a",
	}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, x, ActionGuardrailRefused); n != 3 {
		t.Fatalf("%d rows after the window, want 3", n)
	}
}

// ratelimit.enforced is sampled: a limit that keeps refusing writes one row.
func TestSampleRateRateLimitEnforced(t *testing.T) {
	if !IsAggregated(ActionRateLimitEnforced) {
		t.Fatal("ratelimit.enforced is not a sampled action")
	}
	x := newTestDB(t)
	l := newTestLogger(t, x)
	for range 10 {
		if err := l.Append(context.Background(), Event{
			Action: ActionRateLimitEnforced, SubjectID: "svc-1", Result: "denied", AggregationKey: "gateway_key|gk1|rpm",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, x, ActionRateLimitEnforced); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

// A write that fails does not use up the window: the next event is written.
func TestSampleRateFailedWriteKeepsTheWindowOpen(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	e := Event{Action: ActionGuardrailRefused, SubjectID: "svc-1", Result: "denied"}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Append(gone, e); err == nil {
		t.Fatal("append on a cancelled context succeeded")
	}
	if n := countRows(t, x, ActionGuardrailRefused); n != 0 {
		t.Fatalf("%d rows after the failed write", n)
	}
	if err := l.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, x, ActionGuardrailRefused); n != 1 {
		t.Fatalf("%d rows, want 1: the retry after a failed write must be written, once", n)
	}
	// An invalid payload fails before the insert; the window stays open too.
	bad := e
	bad.SubjectID, bad.Payload = "svc-2", json.RawMessage(`{not json`)
	if err := l.Append(context.Background(), bad); err == nil {
		t.Fatal("append with an invalid payload succeeded")
	}
	bad.Payload = nil
	if err := l.Append(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, x, ActionGuardrailRefused); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
}

// Concurrent first events of one key insert one row between them.
func TestSampleRateConcurrentFirstEvents(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.Append(context.Background(), Event{
				Action: ActionGuardrailRefused, SubjectID: "svc-1", Result: "denied", AggregationKey: "k",
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if n := countRows(t, x, ActionGuardrailRefused); n != 1 {
		t.Fatalf("%d rows from 32 concurrent first events, want 1", n)
	}
}

// appendConcurrently appends per*workers user.create events through the
// given loggers (round-robin) and fails the test on any error.
func appendConcurrently(t *testing.T, loggers []*Logger, workers, per int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, workers*per)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := loggers[w%len(loggers)]
			for i := range per {
				errs <- l.Append(context.Background(), Event{
					ActorID: "u-actor", Action: ActionUserCreate, SubjectID: "u-sub", Result: "ok",
					Payload: json.RawMessage(`{"w":` + strconv.Itoa(w) + `,"i":` + strconv.Itoa(i) + `}`),
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

// The chain verifies after concurrent appends: id order is chain order.
func TestVerifyAfterConcurrentAppends(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	appendConcurrently(t, []*Logger{l}, 16, 60)
	ok, bad, err := l.Verify(context.Background(), "", "")
	if err != nil || !ok {
		t.Fatalf("verify after concurrent appends: ok=%v mismatched=%q err=%v", ok, bad, err)
	}
	if n := countRows(t, x, ActionUserCreate); n != 100 { // the list is capped at 100
		t.Fatalf("%d rows listed", n)
	}
}

// A new row always sorts after the row it chains to, also when that row's id
// is ahead of this process's clock (another instance, or a clock set back).
func TestAppendSortsAfterTheChainHead(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	ahead := "7ZZZZZZZZZ0000000000000ZZZ" // a ULID far in the future, tail about to carry
	if err := l.Append(context.Background(), Event{ID: ahead, Action: ActionUserCreate, SubjectID: "seed"}); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 40, "admin@x")
	ok, bad, err := l.Verify(context.Background(), "", "")
	if err != nil || !ok {
		t.Fatalf("verify: ok=%v mismatched=%q err=%v", ok, bad, err)
	}
}

func TestULIDAfter(t *testing.T) {
	for prev, want := range map[string]string{
		"01ARZ3NDEKTSV4RRFFQ69G5FAV": "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		"01ARZ3NDEKTSV4RRFFQ69G5FAZ": "01ARZ3NDEKTSV4RRFFQ69G5FB0",
		"01ARZ3NDEKZZZZZZZZZZZZZZZZ": "01ARZ3NDEM0000000000000000",
	} {
		got, ok := ulidAfter(prev)
		if !ok || got != want || got <= prev {
			t.Errorf("ulidAfter(%s) = %s, %v; want %s", prev, got, ok, want)
		}
	}
	for _, bad := range []string{"", "evt-1", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", "01ARZ3NDEKTSV4RRFFQ69G5FAU"} {
		if got, ok := ulidAfter(bad); ok {
			t.Errorf("ulidAfter(%q) = %s, want not ok", bad, got)
		}
	}
}

// panicOnInsert is an audit store whose next insert panics.
type panicOnInsert struct {
	AuditDB
	armed bool
}

func (p *panicOnInsert) InsertAuditEvent(ctx context.Context, tx *sql.Tx, e db.AuditEventInsert) error {
	if p.armed {
		p.armed = false
		panic("insert blew up")
	}
	return p.AuditDB.InsertAuditEvent(ctx, tx, e)
}

func appendRecovering(l *Logger, e Event) (panicked any) {
	defer func() { panicked = recover() }()
	_ = l.Append(context.Background(), e)
	return nil
}

// A panic in the middle of the transaction does not leak it: with one
// database connection the next append would otherwise wait for ever.
func TestAppendPanicMidTransactionReleasesTheConnection(t *testing.T) {
	x := newTestDB(t)
	store := &panicOnInsert{AuditDB: x, armed: true}
	l := newTestLogger(t, x)
	l.d = store
	if appendRecovering(l, Event{Action: ActionUserCreate, SubjectID: "u1"}) == nil {
		t.Fatal("the store did not panic")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Append(ctx, Event{Action: ActionUserCreate, SubjectID: "u2"}); err != nil {
		t.Fatalf("append after a panicked append: %v", err)
	}
	if ok, bad, err := l.Verify(ctx, "", ""); err != nil || !ok {
		t.Fatalf("verify: ok=%v mismatched=%q err=%v", ok, bad, err)
	}
	if n := countRows(t, x, ActionUserCreate); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

// A panic while writing a sampled event gives the window back: the key is
// not muted for an hour with no row to show for it.
func TestSampleRatePanicReleasesTheClaim(t *testing.T) {
	x := newTestDB(t)
	store := &panicOnInsert{AuditDB: x, armed: true}
	l := newTestLogger(t, x)
	l.d = store
	e := Event{Action: ActionGuardrailRefused, SubjectID: "svc-1", Result: "denied", AggregationKey: "k"}
	if appendRecovering(l, e) == nil {
		t.Fatal("the store did not panic")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 2 {
		if err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, x, ActionGuardrailRefused); n != 1 {
		t.Fatalf("%d rows, want 1: the event after the panic must be written, once", n)
	}
}

// A row's timestamp is stored, and hashed, at the precision every backend
// keeps (microseconds): what Verify reads back is what was hashed.
func TestAppendTimestampHasStoredPrecision(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	l.now = func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 123456789, time.UTC) }
	if err := l.Append(context.Background(), Event{Action: ActionUserCreate, SubjectID: "u1"}); err != nil {
		t.Fatal(err)
	}
	rows, err := x.ListAuditEvents(context.Background(), db.AuditQuery{Action: ActionUserCreate, Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %d, err %v", len(rows), err)
	}
	if got := rows[0].Ts.UTC().Nanosecond(); got != 123456000 {
		t.Fatalf("stored nanoseconds = %d, want 123456000", got)
	}
	if ok, bad, err := l.Verify(context.Background(), "", ""); err != nil || !ok {
		t.Fatalf("verify: ok=%v mismatched=%q err=%v", ok, bad, err)
	}
}
