package store

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/db"
)

// eventSink records the webhook events the store announces.
type eventSink struct {
	mu       sync.Mutex
	services [][4]string
	policies []policyChange
}

type policyChange struct {
	actor, action string
	before, after map[string]string
}

func (e *eventSink) EmitServiceCreated(serviceID, name, kind, accessMode string) {
	e.mu.Lock()
	e.services = append(e.services, [4]string{serviceID, name, kind, accessMode})
	e.mu.Unlock()
}

func (e *eventSink) EmitAuditPolicyChange(actorEmail, action string, before, after any) {
	b, _ := before.(map[string]string)
	a, _ := after.(map[string]string)
	e.mu.Lock()
	e.policies = append(e.policies, policyChange{actorEmail, action, b, a})
	e.mu.Unlock()
}

// A write of the model gateway's global settings is audited by key, with the
// actor of the request and without the value.
func TestSaveSettings_AuditsGatewaySettings(t *testing.T) {
	s := newStore(t)
	sink := &auditSink{}
	s.SetAuditLogger(sink)
	ctx := audit.WithLogContext(context.Background(), audit.LogContext{ActorID: "u1", ActorEmail: "admin@x"})

	const secretPattern = "(?i)internal-project-name"
	for key, value := range map[string]string{
		"guardrails.global":      `{"enabled":true,"action":"refuse_403"}`,
		"cache.settings":         `{"enabled":true}`,
		"redaction.global":       `{"enabled":true}`,
		"redaction.custom_rules": `[{"name":"proj","pattern":"` + secretPattern + `"}]`,
		"smtp.host":              "mail.example.org", // not a gateway setting
	} {
		if err := s.SaveSettings(ctx, map[string]string{key: value}); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{
		"guardrails.global":      audit.ActionGuardrailSettingsUpdate,
		"cache.settings":         audit.ActionCacheSettingsUpdate,
		"redaction.global":       audit.ActionRedactionSettingsUpdate,
		"redaction.custom_rules": audit.ActionRedactionRuleUpdate,
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.ev) != len(want) {
		t.Fatalf("%d audit entries, want %d: %+v", len(sink.ev), len(want), sink.ev)
	}
	for _, e := range sink.ev {
		if want[e.SubjectID] != e.Action || e.ActorEmail != "admin@x" || e.Result != "ok" {
			t.Errorf("entry %+v", e)
		}
		var payload map[string]string
		if err := json.Unmarshal(e.Payload, &payload); err != nil || len(payload) != 1 || payload["key"] != e.SubjectID {
			t.Errorf("payload of %s: %s", e.Action, e.Payload)
		}
		delete(want, e.SubjectID)
	}
	raw, _ := json.Marshal(sink.ev)
	if strings.Contains(string(raw), secretPattern) || strings.Contains(string(raw), "refuse_403") {
		t.Fatalf("a settings value is in the audit log: %s", raw)
	}
}

// A retention setting that changes is announced with its old and new value;
// one that is written with the value it has is not, and neither is any other
// setting.
func TestSaveSettings_AnnouncesRetentionChange(t *testing.T) {
	s := newStore(t)
	events := &eventSink{}
	s.SetEvents(events)
	ctx := audit.WithLogContext(context.Background(), audit.LogContext{ActorEmail: "admin@x"})

	stored, err := s.GetSettings(ctx) // the defaults the migrations seeded
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, map[string]string{"audit.retention_days": "91", "usage.retention_days": "31"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, map[string]string{"audit.retention_days": "400", "usage.retention_days": "31", "smtp.host": "h"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, map[string]string{"audit.retention_days": "400"}); err != nil { // no change
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, map[string]string{"smtp.host": "other"}); err != nil { // no retention setting
		t.Fatal(err)
	}
	if len(events.policies) != 2 {
		t.Fatalf("%d policy changes announced, want 2: %+v", len(events.policies), events.policies)
	}
	first, second := events.policies[0], events.policies[1]
	if first.actor != "admin@x" || first.action != "retention.update" ||
		len(first.after) != 2 || first.after["audit.retention_days"] != "91" || first.after["usage.retention_days"] != "31" ||
		first.before["audit.retention_days"] != stored["audit.retention_days"] || first.before["usage.retention_days"] != stored["usage.retention_days"] {
		t.Errorf("first change: %+v (stored before: %v)", first, stored)
	}
	if len(second.after) != 1 || second.before["audit.retention_days"] != "91" || second.after["audit.retention_days"] != "400" {
		t.Errorf("second change: %+v", second)
	}
}

// A service an admin creates is announced; a refused one is not.
func TestCreateService_AnnouncesService(t *testing.T) {
	s := newStore(t)
	events := &eventSink{}
	s.SetEvents(events)
	ctx := context.Background()
	owner := mustCreateUser(t, s, "svc-owner@x", "admin").ID
	svc := db.Service{ID: "svc-pre", UserID: owner, Name: "pre", Type: "http", AccessMode: "api_key", APIKeyHeader: "Authorization"}
	if err := s.CreateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateService(ctx, svc); err == nil {
		t.Fatal("a second service with the same id was created")
	}
	if len(events.services) != 1 || events.services[0] != [4]string{"svc-pre", "pre", "http", "api_key"} {
		t.Fatalf("announced: %v", events.services)
	}
}

// The audit entry of a mutation is written although the caller hung up: the
// request's context is cancelled, the entry's is not.
func TestEmitAudit_SurvivesACancelledRequest(t *testing.T) {
	s := newStore(t)
	sink := &ctxAuditSink{}
	s.SetAuditLogger(sink)
	ctx, cancel := context.WithCancel(audit.WithLogContext(context.Background(), audit.LogContext{ActorEmail: "admin@x"}))
	cancel()
	// SQLite does not check the context of this write; the audit sink does.
	if err := s.SaveSettings(context.WithoutCancel(ctx), map[string]string{"cache.settings": "{}"}); err != nil {
		t.Fatal(err)
	}
	s.emitAudit(ctx, audit.ActionCacheSettingsUpdate, nil)
	if sink.cancelled != 0 || sink.written != 2 {
		t.Fatalf("%d entries written, %d refused for a cancelled context; want 2 and 0", sink.written, sink.cancelled)
	}
	if sink.actor != "admin@x" {
		t.Fatalf("the detached context lost the actor: %q", sink.actor)
	}
}

// ctxAuditSink refuses an entry whose context is cancelled, as the audit
// logger does.
type ctxAuditSink struct {
	written, cancelled int
	actor              string
}

func (a *ctxAuditSink) Append(ctx context.Context, e any) error {
	if err := ctx.Err(); err != nil {
		a.cancelled++
		return err
	}
	a.written++
	if ev, ok := e.(audit.Event); ok {
		a.actor = ev.ActorEmail
	}
	return nil
}
