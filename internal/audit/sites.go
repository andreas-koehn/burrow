package audit

import (
	"context"
	"encoding/json"
)

// sites.go is a thin call-site library: one helper per auditable mutation,
// invoked from internal/store (and one or two handlers) so the action
// strings + payload shape live in one place. Task 25 wires every helper
// from the mutating site; Task 13 just provides the helpers themselves.
//
// Every helper accepts a nil logger as a no-op so production code can
// pre-wire the call before the Logger field on *store.Store is populated
// (the same nil-safe pattern the rest of the codebase already uses).

// LogContext is the per-request audit metadata the handler/store extracts
// from the http.Request before delegating to the mutating function.
// Reuse one across multiple Append calls inside the same request.
type LogContext struct {
	ActorID    string
	ActorEmail string
	SourceIP   string
	UserAgent  string
	RequestID  string
}

// MustJSON marshals v to a json.RawMessage suitable for Event.Payload.
// On marshal failure it falls back to an empty object so the chain still
// ticks forward — Marshal should never fail on the maps callers pass in.
func MustJSON(v any) json.RawMessage { return mustJSON(v) }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Marshal should never fail on the maps we hand it; if it does,
		// fall back to an empty object so the chain still ticks forward.
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}

// LogUserCreate logs ActionUserCreate.
func LogUserCreate(ctx context.Context, l *Logger, c LogContext, newUserID, newUserEmail, role string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionUserCreate,
		SubjectID: newUserID, SubjectLabel: newUserEmail, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]string{"role": role}),
	})
}

// LogUserUpdate logs ActionUserUpdate (role, etc.).
func LogUserUpdate(ctx context.Context, l *Logger, c LogContext, userID, userEmail string, fields map[string]any) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionUserUpdate,
		SubjectID: userID, SubjectLabel: userEmail, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(fields),
	})
}

// LogUserDelete logs ActionUserDelete.
func LogUserDelete(ctx context.Context, l *Logger, c LogContext, userID, userEmail string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionUserDelete,
		SubjectID: userID, SubjectLabel: userEmail, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogTokenMint logs ActionTokenMint (client API token).
func LogTokenMint(ctx context.Context, l *Logger, c LogContext, tokenID, label string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionTokenMint,
		SubjectID: tokenID, SubjectLabel: label, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogTokenRevoke logs ActionTokenRevoke.
func LogTokenRevoke(ctx context.Context, l *Logger, c LogContext, tokenID, label string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionTokenRevoke,
		SubjectID: tokenID, SubjectLabel: label, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogServiceCreate logs ActionServiceCreate.
func LogServiceCreate(ctx context.Context, l *Logger, c LogContext, serviceID, serviceName string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionServiceCreate,
		SubjectID: serviceID, SubjectLabel: serviceName, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogServiceDelete logs ActionServiceDelete.
func LogServiceDelete(ctx context.Context, l *Logger, c LogContext, serviceID, serviceName string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionServiceDelete,
		SubjectID: serviceID, SubjectLabel: serviceName, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogServiceAccessModeUpdate logs ActionServiceAccessModeUpdate.
func LogServiceAccessModeUpdate(ctx context.Context, l *Logger, c LogContext, serviceID, serviceName, oldMode, newMode string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionServiceAccessModeUpdate,
		SubjectID: serviceID, SubjectLabel: serviceName, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]string{"from": oldMode, "to": newMode}),
	})
}

// LogServiceAccessPolicyUpdate logs ActionServiceAccessPolicyUpdate.
func LogServiceAccessPolicyUpdate(ctx context.Context, l *Logger, c LogContext, serviceID, serviceName string, roles []string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionServiceAccessPolicyUpdate,
		SubjectID: serviceID, SubjectLabel: serviceName, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]any{"roles": roles}),
	})
}

// LogServiceAPIKeyCreate logs ActionServiceAPIKeyCreate.
func LogServiceAPIKeyCreate(ctx context.Context, l *Logger, c LogContext, serviceID, keyID, keyName string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionServiceAPIKeyCreate,
		SubjectID: keyID, SubjectLabel: keyName, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]string{"service_id": serviceID}),
	})
}

// LogServiceAPIKeyRevoke logs ActionServiceAPIKeyRevoke.
func LogServiceAPIKeyRevoke(ctx context.Context, l *Logger, c LogContext, serviceID, keyID, keyName string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionServiceAPIKeyRevoke,
		SubjectID: keyID, SubjectLabel: keyName, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]string{"service_id": serviceID}),
	})
}

// LogSessionCreate logs ActionSessionCreate.
func LogSessionCreate(ctx context.Context, l *Logger, c LogContext, sessionID, userEmail string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionSessionCreate,
		SubjectID: sessionID, SubjectLabel: userEmail, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogSessionDelete logs ActionSessionDelete.
func LogSessionDelete(ctx context.Context, l *Logger, c LogContext, sessionID, userEmail string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionSessionDelete,
		SubjectID: sessionID, SubjectLabel: userEmail, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogSessionRevokeOthers logs ActionSessionRevokeOthers.
func LogSessionRevokeOthers(ctx context.Context, l *Logger, c LogContext, userID, userEmail string, revoked int64) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionSessionRevokeOthers,
		SubjectID: userID, SubjectLabel: userEmail, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]int64{"revoked": revoked}),
	})
}

// LogAuditExport logs ActionAuditExport (audit log export self-audit).
func LogAuditExport(ctx context.Context, l *Logger, c LogContext, rangeFromID, rangeToID string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionAuditExport,
		Result:   "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]string{"from_id": rangeFromID, "to_id": rangeToID}),
	})
}

// LogRedactionApplied logs ActionRedactionApplied (sample-rated 1/hr).
func LogRedactionApplied(ctx context.Context, l *Logger, c LogContext, serviceID, ruleName string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionRedactionApplied,
		SubjectID: serviceID, SubjectLabel: ruleName, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogGuardrailRefused logs ActionGuardrailRefused (sample-rated 1/hr per
// service: this helper sets no Event.AggregationKey).
func LogGuardrailRefused(ctx context.Context, l *Logger, c LogContext, serviceID, patternID string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionGuardrailRefused,
		SubjectID: serviceID, SubjectLabel: patternID, Result: "denied",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
	})
}

// LogServiceUpstreamCredentialBind logs ActionServiceUpstreamCredentialBind.
// Payload carries {slot} only — the credential value is never logged.
func LogServiceUpstreamCredentialBind(ctx context.Context, l *Logger, c LogContext, serviceID, slot string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionServiceUpstreamCredentialBind,
		SubjectID: serviceID, SubjectLabel: serviceID, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]string{"slot": slot}),
	})
}

// LogServiceUpstreamCredentialUnbind logs ActionServiceUpstreamCredentialUnbind.
func LogServiceUpstreamCredentialUnbind(ctx context.Context, l *Logger, c LogContext, serviceID, slot string) {
	if l == nil {
		return
	}
	_ = l.Append(ctx, Event{
		ActorID: c.ActorID, ActorEmail: c.ActorEmail, Action: ActionServiceUpstreamCredentialUnbind,
		SubjectID: serviceID, SubjectLabel: serviceID, Result: "ok",
		SourceIP: c.SourceIP, UserAgent: c.UserAgent, RequestID: c.RequestID,
		Payload: mustJSON(map[string]string{"slot": slot}),
	})
}
