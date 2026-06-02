package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/db"
)

// genesisPrevHash is the 64-char hex-zero prev_hash of the very first row
// in the chain (spec Part G.1).
const genesisPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

// AggregationWindow is the per-(subject_id, action) deduplication window
// applied to ActionRedactionApplied and ActionGuardrailRefused.
//
// Spec Part G.4 mandates 1/hour aggregation: a high-traffic redaction or
// guardrail-refusal storm therefore cannot inflate the chain. The first
// event in a window is appended; later events within the window are
// dropped (silently — Append returns nil so call sites can stay hot).
var AggregationWindow = time.Hour

// Event is one row destined for the audit_events table. Logger.Append
// fills in ID, TS, PrevHash and Hash; callers populate the rest.
type Event struct {
	ID           string
	ActorID      string
	ActorEmail   string
	Action       string
	SubjectID    string
	SubjectLabel string
	Result       string // "ok" | "denied" | "error"
	SourceIP     string
	UserAgent    string
	RequestID    string
	Payload      json.RawMessage
	TS           time.Time
}

// AuditDB is the narrow slice of *db.DB the Logger needs. The package
// stays test-friendly without dragging in the full *db.DB surface.
type AuditDB interface {
	DB() *sql.DB
	InsertAuditEvent(ctx context.Context, tx *sql.Tx, e db.AuditEventInsert) error
	LatestAuditHash(ctx context.Context, tx *sql.Tx) (string, bool, error)
	IterAuditEventsAsc(ctx context.Context, fromID, toID string, visit func(db.AuditEvent) error) error
	ListAuditEvents(ctx context.Context, q db.AuditQuery) ([]db.AuditEvent, error)
}

// Logger appends events to audit_events under a SHA-256 hash chain and
// signs NDJSON exports with the configured Ed25519 key.
type Logger struct {
	d       AuditDB
	priv    ed25519.PrivateKey
	log     *slog.Logger
	mu      sync.Mutex
	lastAgg map[string]time.Time // (subject_id|action) -> first-seen-at
	now     func() time.Time     // injectable for tests
}

// NewLogger returns a ready-to-use Logger. The signing key is loaded (or
// generated on first call) outside this constructor via
// LoadOrGenerateSigningKey — pass the result in.
func NewLogger(d AuditDB, signingKey ed25519.PrivateKey, log *slog.Logger) *Logger {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Logger{
		d:       d,
		priv:    signingKey,
		log:     log,
		lastAgg: map[string]time.Time{},
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// PublicKey returns the Ed25519 public key derived from the configured
// signing key (or nil if the Logger was constructed without one).
func (l *Logger) PublicKey() ed25519.PublicKey {
	if l == nil || l.priv == nil {
		return nil
	}
	pub, _ := l.priv.Public().(ed25519.PublicKey)
	return pub
}

// FingerprintHex returns the sha256 hex of PublicKey() (matches the
// trailer "fingerprint" field on exports).
func (l *Logger) FingerprintHex() string {
	pub := l.PublicKey()
	if pub == nil {
		return ""
	}
	return Fingerprint(pub)
}

// Append inserts e into audit_events under the hash chain. ID, TS, PrevHash
// and Hash are computed here; the caller's e.ID/e.TS/e.Payload are honored
// when set (deterministic-test seam).
//
// For aggregated actions (redaction.applied, guardrail.refused) the call is
// silently dropped when another row for the same (subject_id, action) was
// appended within AggregationWindow — spec Part G.4. Return value is nil
// in that case (call sites should not treat a sample-rate skip as a bug).
func (l *Logger) Append(ctx context.Context, e Event) error {
	if l == nil {
		return nil // nil-logger = audit disabled (tests / Task 13 wiring stub)
	}
	if e.Action == "" {
		return errors.New("audit: action is required")
	}
	if e.Result == "" {
		e.Result = "ok"
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage(`{}`)
	}

	if IsAggregated(e.Action) {
		l.mu.Lock()
		key := e.SubjectID + "|" + e.Action
		now := l.now()
		if last, ok := l.lastAgg[key]; ok && now.Sub(last) < AggregationWindow {
			l.mu.Unlock()
			return nil // dropped: still inside the dedup window
		}
		l.lastAgg[key] = now
		l.mu.Unlock()
	}

	if e.ID == "" {
		id, err := NewULID()
		if err != nil {
			return fmt.Errorf("audit: new id: %w", err)
		}
		e.ID = id
	}
	if e.TS.IsZero() {
		e.TS = l.now()
	}

	// Hash chain in a tx: read head, compute hash, insert. The tx + the
	// sql.DB-level single-connection cap (SetMaxOpenConns(1) in db.Open)
	// guarantee no concurrent append observes the same prev_hash.
	tx, err := l.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("audit: begin tx: %w", err)
	}
	prev, ok, err := l.d.LatestAuditHash(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if !ok {
		prev = genesisPrevHash
	}
	canon, err := Canonical(e)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	prevBytes, err := hex.DecodeString(prev)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("audit: decode prev_hash: %w", err)
	}
	h := sha256.New()
	h.Write(prevBytes)
	h.Write(canon)
	hashHex := hex.EncodeToString(h.Sum(nil))

	if err := l.d.InsertAuditEvent(ctx, tx, db.AuditEventInsert{
		ID: e.ID, Ts: e.TS, ActorID: e.ActorID, ActorEmail: e.ActorEmail, Action: e.Action,
		SubjectID: e.SubjectID, SubjectLabel: e.SubjectLabel, Result: e.Result,
		SourceIP: e.SourceIP, UserAgent: e.UserAgent, RequestID: e.RequestID,
		Payload: string(e.Payload), PrevHash: prev, Hash: hashHex,
	}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("audit: commit: %w", err)
	}
	return nil
}

// Verify walks audit_events in ascending id order between fromID and toID
// (both empty = whole chain) and recomputes each row's hash from its
// prev_hash + canonical bytes. The first row's prev_hash MUST match the
// chain head as the walk sees it (genesis when the range starts at the
// very first row).
//
// Returns ok=true when every row matches. On mismatch ok=false and
// mismatched is the id of the first failing row.
func (l *Logger) Verify(ctx context.Context, fromID, toID string) (ok bool, mismatched string, err error) {
	if l == nil {
		return false, "", errors.New("audit: nil logger")
	}
	expected := genesisPrevHash
	first := true
	mismatchedID := ""
	chainOK := true
	visit := func(row db.AuditEvent) error {
		if !chainOK {
			return nil // already failed; ignore the rest
		}
		// If a range start was specified, the first row's prev_hash is
		// whatever the row itself stores (we cannot recompute prior
		// state). For the whole-chain walk (no fromID), the first row
		// MUST have prev_hash = genesis.
		if first {
			first = false
			if fromID == "" {
				if row.PrevHash != genesisPrevHash {
					chainOK = false
					mismatchedID = row.ID
					return nil
				}
				expected = genesisPrevHash
			} else {
				expected = row.PrevHash
			}
		}
		if row.PrevHash != expected {
			chainOK = false
			mismatchedID = row.ID
			return nil
		}
		ev := Event{
			ID: row.ID, ActorID: row.ActorID, ActorEmail: row.ActorEmail,
			Action: row.Action, SubjectID: row.SubjectID, SubjectLabel: row.SubjectLabel,
			Result: row.Result, SourceIP: row.SourceIP, UserAgent: row.UserAgent,
			RequestID: row.RequestID, Payload: json.RawMessage(row.Payload), TS: row.Ts,
		}
		canon, err := Canonical(ev)
		if err != nil {
			return err
		}
		prevBytes, err := hex.DecodeString(row.PrevHash)
		if err != nil {
			return fmt.Errorf("audit: verify: decode prev_hash %q: %w", row.PrevHash, err)
		}
		h := sha256.New()
		h.Write(prevBytes)
		h.Write(canon)
		if hex.EncodeToString(h.Sum(nil)) != row.Hash {
			chainOK = false
			mismatchedID = row.ID
			return nil
		}
		expected = row.Hash
		return nil
	}
	if err := l.d.IterAuditEventsAsc(ctx, fromID, toID, visit); err != nil {
		return false, "", err
	}
	return chainOK, mismatchedID, nil
}

// ExportQuery is the filter passed to ExportNDJSON. Empty Since/Until
// means "the whole chain"; FromID/ToID are inclusive id bounds.
type ExportQuery struct {
	Since  *time.Time
	Until  *time.Time
	Action string
	Actor  string
	FromID string
	ToID   string
}

// exportEventJSON is the per-line wire shape (mirrors db.AuditEvent + the
// chain columns so the consumer can re-verify offline).
type exportEventJSON struct {
	ID           string          `json:"id"`
	Ts           time.Time       `json:"ts"`
	ActorID      string          `json:"actor_id"`
	ActorEmail   string          `json:"actor_email"`
	Action       string          `json:"action"`
	SubjectID    string          `json:"subject_id"`
	SubjectLabel string          `json:"subject_label"`
	Result       string          `json:"result"`
	SourceIP     string          `json:"source_ip"`
	UserAgent    string          `json:"user_agent"`
	RequestID    string          `json:"request_id"`
	Payload      json.RawMessage `json:"payload"`
	PrevHash     string          `json:"prev_hash"`
	Hash         string          `json:"hash"`
}

// ExportNDJSON streams matching audit_events to w as NDJSON. The final
// line is a trailer {"_signature","fingerprint"}: signature is the
// base64-encoded Ed25519 signature over the concatenation of every
// preceding line (each line including its trailing "\n"). fingerprint is
// the lowercase hex sha256 of the public key.
//
// Consumers verify by replaying the chain (offline) AND verifying the
// trailer signature with the public key fetched from
// GET /api/v1/audit/fingerprint.
func (l *Logger) ExportNDJSON(ctx context.Context, w io.Writer, q ExportQuery) error {
	if l == nil || l.priv == nil {
		return ErrEmptySigningKey
	}
	// Buffer the preceding lines so we can sign them in one call. For very
	// large exports this is bounded by the audit table itself (export
	// endpoint is admin/audit:read-only and gated by Limit when caller
	// supplies one); the v0.4 footprint is far under a few MB.
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)

	apply := func(row db.AuditEvent) bool {
		if q.Action != "" && row.Action != q.Action {
			return false
		}
		if q.Actor != "" && row.ActorID != q.Actor && row.ActorEmail != q.Actor {
			return false
		}
		if q.Since != nil && row.Ts.Before(*q.Since) {
			return false
		}
		if q.Until != nil && row.Ts.After(*q.Until) {
			return false
		}
		return true
	}

	visit := func(row db.AuditEvent) error {
		if !apply(row) {
			return nil
		}
		line := exportEventJSON{
			ID: row.ID, Ts: row.Ts, ActorID: row.ActorID, ActorEmail: row.ActorEmail,
			Action: row.Action, SubjectID: row.SubjectID, SubjectLabel: row.SubjectLabel,
			Result: row.Result, SourceIP: row.SourceIP, UserAgent: row.UserAgent,
			RequestID: row.RequestID, Payload: json.RawMessage(row.Payload),
			PrevHash: row.PrevHash, Hash: row.Hash,
		}
		b, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("audit: marshal export line: %w", err)
		}
		// Errors on the buffered writer surface at bw.Flush() below.
		_, _ = bw.Write(b)
		_ = bw.WriteByte('\n')
		return nil
	}
	if err := l.d.IterAuditEventsAsc(ctx, q.FromID, q.ToID, visit); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	body := buf.Bytes()

	sig := ed25519.Sign(l.priv, body)
	tr := trailer{
		Signature:   base64.StdEncoding.EncodeToString(sig),
		Fingerprint: l.FingerprintHex(),
	}
	trBytes, err := json.Marshal(tr)
	if err != nil {
		return fmt.Errorf("audit: marshal trailer: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	if _, err := w.Write(trBytes); err != nil {
		return err
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}

// VerifySignedExport reads NDJSON from r — the same shape ExportNDJSON
// emits — and verifies the trailer signature against pub. ok=true means
// the trailer's signature matches the concatenation of all preceding
// lines. mismatchedID will be set if a row-level chain hash mismatch is
// found while scanning (the offline reverify pass).
//
// firstID and lastID are the id columns of the first and last EVENT lines
// (the trailer is excluded). The CLI consumes this to print "Chain valid
// from <first> to <last>".
func VerifySignedExport(r io.Reader, pub ed25519.PublicKey) (ok bool, firstID, lastID, mismatchedID string, err error) {
	all, err := io.ReadAll(r)
	if err != nil {
		return false, "", "", "", err
	}
	// Split into lines. The trailer is the last line (with trailing "\n").
	lines := bytes.Split(all, []byte{'\n'})
	// bytes.Split leaves a trailing empty element when input ends with "\n";
	// drop it.
	for len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return false, "", "", "", errors.New("audit: empty export")
	}
	trLine := lines[len(lines)-1]
	eventLines := lines[:len(lines)-1]

	// Recompute the signed body: every event line + its "\n".
	var signedBody bytes.Buffer
	for _, ln := range eventLines {
		signedBody.Write(ln)
		signedBody.WriteByte('\n')
	}

	var tr trailer
	if err := json.Unmarshal(trLine, &tr); err != nil {
		return false, "", "", "", fmt.Errorf("audit: decode trailer: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(tr.Signature)
	if err != nil {
		return false, "", "", "", fmt.Errorf("audit: decode signature: %w", err)
	}
	if !ed25519.Verify(pub, signedBody.Bytes(), sig) {
		return false, "", "", "", nil
	}

	// Per-row chain replay (offline) so we surface the same mismatched_id
	// the in-DB verifier would. Genesis prev = 64 zeros; verifier follows
	// the same canonical encoding as Append.
	expected := genesisPrevHash
	for i, ln := range eventLines {
		var ev exportEventJSON
		if err := json.Unmarshal(ln, &ev); err != nil {
			return false, "", "", "", fmt.Errorf("audit: decode export line %d: %w", i, err)
		}
		if firstID == "" {
			firstID = ev.ID
		}
		lastID = ev.ID
		if ev.PrevHash != expected {
			return false, firstID, lastID, ev.ID, nil
		}
		canon, cerr := Canonical(Event{
			ID: ev.ID, ActorID: ev.ActorID, ActorEmail: ev.ActorEmail,
			Action: ev.Action, SubjectID: ev.SubjectID, SubjectLabel: ev.SubjectLabel,
			Result: ev.Result, SourceIP: ev.SourceIP, UserAgent: ev.UserAgent,
			RequestID: ev.RequestID, Payload: ev.Payload, TS: ev.Ts,
		})
		if cerr != nil {
			return false, firstID, lastID, ev.ID, cerr
		}
		prevBytes, derr := hex.DecodeString(ev.PrevHash)
		if derr != nil {
			return false, firstID, lastID, ev.ID, fmt.Errorf("audit: decode prev_hash: %w", derr)
		}
		h := sha256.New()
		h.Write(prevBytes)
		h.Write(canon)
		if hex.EncodeToString(h.Sum(nil)) != ev.Hash {
			return false, firstID, lastID, ev.ID, nil
		}
		expected = ev.Hash
	}
	return true, firstID, lastID, "", nil
}
