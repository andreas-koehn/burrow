package customdomain

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// fakeTickDB is an in-memory StatusTickDB used by TestRunStatusTick.
type fakeTickDB struct {
	mu      sync.Mutex
	rows    []db.ServiceCustomDomain
	updates []tickUpdate
	listErr error
}

type tickUpdate struct {
	ID     string
	Status string
}

func (f *fakeTickDB) ListAllCustomDomains(ctx context.Context) ([]db.ServiceCustomDomain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]db.ServiceCustomDomain, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

func (f *fakeTickDB) UpdateCustomDomainStatus(ctx context.Context, id, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, tickUpdate{ID: id, Status: status})
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].Status = status
		}
	}
	return nil
}

// fakeTickAudit captures every Append call.
type fakeTickAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (f *fakeTickAudit) Append(ctx context.Context, e audit.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

// fakeTickWebhook captures every Publish call.
type fakeTickWebhook struct {
	mu       sync.Mutex
	publishs []tickPublish
}

type tickPublish struct {
	Event   string
	Payload any
}

func (f *fakeTickWebhook) Publish(ctx context.Context, event string, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishs = append(f.publishs, tickPublish{Event: event, Payload: payload})
}

// quietLogger discards all output (for tests).
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunStatusTick covers the happy path with three seeded domains:
//
//  1. d-valid: 60d remaining, stored "active" → no transition.
//  2. d-expiring: 7d remaining, stored "active" → transitions to "cert_expiring",
//     audit appended, webhook fired ONCE.
//  3. d-expired: 1h past, stored "active" → transitions to "cert_expired",
//     audit appended, NO webhook fire (edge is not active→cert_expiring).
//
// A second tick with no clock change must emit zero new audits / webhooks
// (the once-per-edge invariant).
func TestRunStatusTick(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	fdb := &fakeTickDB{
		rows: []db.ServiceCustomDomain{
			{
				ID: "d-valid", ServiceID: "svc1", Hostname: "valid.example.com",
				NotAfter: now.Add(60 * 24 * time.Hour),
				Status:   StatusActive,
			},
			{
				ID: "d-expiring", ServiceID: "svc1", Hostname: "expiring.example.com",
				NotAfter: now.Add(7 * 24 * time.Hour),
				Status:   StatusActive,
			},
			{
				ID: "d-expired", ServiceID: "svc1", Hostname: "expired.example.com",
				NotAfter: now.Add(-1 * time.Hour),
				Status:   StatusActive,
			},
		},
	}
	fau := &fakeTickAudit{}
	fwh := &fakeTickWebhook{}
	deps := StatusTickDeps{DB: fdb, Audit: fau, Webhook: fwh, Log: quietLogger()}

	// First tick — 2 transitions: d-expiring → cert_expiring, d-expired → cert_expired.
	n, err := RunStatusTick(context.Background(), deps, now)
	if err != nil {
		t.Fatalf("RunStatusTick: %v", err)
	}
	if n != 2 {
		t.Errorf("transitions = %d; want 2", n)
	}

	// d-valid must not have been updated.
	for _, u := range fdb.updates {
		if u.ID == "d-valid" {
			t.Errorf("d-valid was updated to %q; want no update", u.Status)
		}
	}

	// Verify the persisted statuses.
	want := map[string]string{
		"d-valid":    StatusActive,
		"d-expiring": StatusCertExpiring,
		"d-expired":  StatusCertExpired,
	}
	for _, r := range fdb.rows {
		if r.Status != want[r.ID] {
			t.Errorf("%s: stored status = %q; want %q", r.ID, r.Status, want[r.ID])
		}
	}

	// Exactly 2 audit events, one per transition.
	if len(fau.events) != 2 {
		t.Errorf("audit events = %d; want 2", len(fau.events))
	}
	for _, e := range fau.events {
		if e.Action != audit.ActionServiceCustomDomainStatusChanged {
			t.Errorf("audit action = %q; want %q",
				e.Action, audit.ActionServiceCustomDomainStatusChanged)
		}
	}

	// Webhook: exactly one fire, for d-expiring, with event
	// custom_domain.cert.expiring.
	if len(fwh.publishs) != 1 {
		t.Fatalf("webhook publishes = %d; want 1", len(fwh.publishs))
	}
	p := fwh.publishs[0]
	if p.Event != "custom_domain.cert.expiring" {
		t.Errorf("webhook event = %q; want custom_domain.cert.expiring", p.Event)
	}
	payload, ok := p.Payload.(map[string]any)
	if !ok {
		t.Fatalf("webhook payload type = %T; want map[string]any", p.Payload)
	}
	if payload["id"] != "d-expiring" {
		t.Errorf("payload[id] = %v; want d-expiring", payload["id"])
	}
	if payload["hostname"] != "expiring.example.com" {
		t.Errorf("payload[hostname] = %v; want expiring.example.com", payload["hostname"])
	}

	// --- Second tick: nothing changes, no new fires. ---
	priorAudits := len(fau.events)
	priorPublishs := len(fwh.publishs)
	priorUpdates := len(fdb.updates)
	n2, err := RunStatusTick(context.Background(), deps, now)
	if err != nil {
		t.Fatalf("second RunStatusTick: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second tick transitions = %d; want 0", n2)
	}
	if len(fau.events) != priorAudits {
		t.Errorf("second tick added %d audits; want 0",
			len(fau.events)-priorAudits)
	}
	if len(fwh.publishs) != priorPublishs {
		t.Errorf("second tick added %d webhook fires; want 0",
			len(fwh.publishs)-priorPublishs)
	}
	if len(fdb.updates) != priorUpdates {
		t.Errorf("second tick added %d DB updates; want 0",
			len(fdb.updates)-priorUpdates)
	}
}

// TestRunStatusTickSkipsPending: pending rows have no cert and must not be
// touched even if their notAfter would compute to a non-pending status.
func TestRunStatusTickSkipsPending(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	fdb := &fakeTickDB{
		rows: []db.ServiceCustomDomain{
			{
				ID: "d-pending", ServiceID: "svc1", Hostname: "pending.example.com",
				NotAfter: now.Add(60 * 24 * time.Hour),
				Status:   StatusPending,
			},
		},
	}
	fau := &fakeTickAudit{}
	fwh := &fakeTickWebhook{}
	deps := StatusTickDeps{DB: fdb, Audit: fau, Webhook: fwh, Log: quietLogger()}

	n, err := RunStatusTick(context.Background(), deps, now)
	if err != nil {
		t.Fatalf("RunStatusTick: %v", err)
	}
	if n != 0 {
		t.Errorf("transitions = %d; want 0 (pending row skipped)", n)
	}
	if len(fdb.updates) != 0 {
		t.Errorf("updates = %d; want 0", len(fdb.updates))
	}
}

// TestRunStatusTickNilDB makes the tick a no-op when no DB is wired.
func TestRunStatusTickNilDB(t *testing.T) {
	deps := StatusTickDeps{Log: quietLogger()}
	n, err := RunStatusTick(context.Background(), deps, time.Now())
	if err != nil {
		t.Fatalf("RunStatusTick (nil DB): %v", err)
	}
	if n != 0 {
		t.Errorf("transitions = %d; want 0", n)
	}
}

// TestRunStatusTickNoWebhookEdgeForExpiredJump: a row that jumps directly
// from active to cert_expired (the tick missed the warn window) does NOT
// fire the webhook — the webhook is gated on the specific
// active->cert_expiring edge.
func TestRunStatusTickNoWebhookEdgeForExpiredJump(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	fdb := &fakeTickDB{
		rows: []db.ServiceCustomDomain{
			{
				ID: "d-jumped", ServiceID: "svc1", Hostname: "jumped.example.com",
				NotAfter: now.Add(-30 * time.Minute),
				Status:   StatusActive,
			},
		},
	}
	fau := &fakeTickAudit{}
	fwh := &fakeTickWebhook{}
	deps := StatusTickDeps{DB: fdb, Audit: fau, Webhook: fwh, Log: quietLogger()}

	if _, err := RunStatusTick(context.Background(), deps, now); err != nil {
		t.Fatalf("RunStatusTick: %v", err)
	}
	if len(fdb.updates) != 1 || fdb.updates[0].Status != StatusCertExpired {
		t.Errorf("updates = %v; want one update to cert_expired", fdb.updates)
	}
	if len(fau.events) != 1 {
		t.Errorf("audit events = %d; want 1 (transition)", len(fau.events))
	}
	if len(fwh.publishs) != 0 {
		t.Errorf("webhook publishes = %d; want 0 (active->cert_expired is not an edge fire)",
			len(fwh.publishs))
	}
}

// An auto row has no stored certificate (zero not_after): the tick leaves
// it alone instead of calling it expired (H10).
func TestRunStatusTickSkipsAuto(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	fdb := &fakeTickDB{rows: []db.ServiceCustomDomain{{
		ID: "d-auto", ServiceID: "svc1", Hostname: "auto.example.com",
		Status: StatusActive, CertSource: db.CertSourceAuto,
	}}}
	fau := &fakeTickAudit{}
	n, err := RunStatusTick(context.Background(), StatusTickDeps{DB: fdb, Audit: fau, Log: quietLogger()}, now)
	if err != nil || n != 0 || len(fdb.updates) != 0 || len(fau.events) != 0 {
		t.Fatalf("n=%d err=%v updates=%v audit=%d", n, err, fdb.updates, len(fau.events))
	}
}
