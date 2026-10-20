package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/ankoehn/burrow/internal/aigateway"
	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/observe"
)

// A judge block is a guardrail.refused entry of tier judge; a failure is a
// guardrail.judge_failed entry. Both carry the verdict and a reason class
// only: the judge's own words (model text) stay off the hash-chained audit
// log, in the inspector entry, and the judged text appears nowhere. Both are
// sampled like the other guardrail entries.
func TestGuardrailAuditor_Judge(t *testing.T) {
	aud := &blockingAudit{}
	g := newGuardrailAuditor(aud, nil)
	t.Cleanup(g.stop)
	route := aigw.NewRoute("gk-9", "openai", "m", "req-1")
	g.judged(aigw.WithRoute(context.Background(), route), "svc-1", aigw.JudgeEvent{
		Direction: "request", Verdict: aigw.JudgeBlock, Refused: true,
	})
	g.judged(context.Background(), "svc-1", aigw.JudgeEvent{Direction: "answer", Verdict: aigw.JudgeFailed, Failure: aigw.JudgeTimeout})
	g.judged(context.Background(), "svc-1", aigw.JudgeEvent{Direction: "request", Verdict: aigw.JudgeFailed, Failure: aigw.JudgeMalformed, Refused: true})
	flushed(t, g)
	got := aud.all()
	if len(got) != 3 {
		t.Fatalf("events %+v", got)
	}
	if e := got[0]; e.Action != audit.ActionGuardrailRefused || e.SubjectID != "svc-1" || e.SubjectLabel != "judge" || e.Result != "denied" {
		t.Fatalf("block event = %+v", e)
	}
	if p := string(got[0].Payload); p != `{"action":"block","direction":"request","gateway_key_id":"gk-9","reason":"policy","tier":"judge","verdict":"block"}` {
		t.Fatalf("block payload = %s", p)
	}
	if e := got[1]; e.Action != audit.ActionGuardrailJudgeFailed || e.SubjectLabel != "timeout" || e.Result != "ok" {
		t.Fatalf("failure event = %+v", e)
	}
	if p := string(got[1].Payload); p != `{"direction":"answer","on_failure":"allow","reason":"timeout","verdict":"failed"}` {
		t.Fatalf("failure payload = %s", p)
	}
	if e := got[2]; e.Action != audit.ActionGuardrailJudgeFailed || e.Result != "denied" {
		t.Fatalf("refused failure event = %+v", e)
	}
	if p := string(got[2].Payload); p != `{"direction":"request","on_failure":"refuse","reason":"malformed","verdict":"failed"}` {
		t.Fatalf("refused failure payload = %s", p)
	}
	if !audit.IsAggregated(audit.ActionGuardrailJudgeFailed) {
		t.Fatal("guardrail.judge_failed is not sampled: a judge that is down would write a row per request")
	}
	if !slices.Contains(audit.AllActions, audit.ActionGuardrailJudgeFailed) {
		t.Fatal("guardrail.judge_failed is not in AllActions")
	}
}

// The webhook names the judge as the tier, with no pattern.
func TestObserver_GuardrailJudge(t *testing.T) {
	o, _, em, _ := newTestObserver(t)
	o.ObserveModelRequest(observe.ModelRequest{
		ServiceID: "svc-j", Outcome: observe.ModelGuardrailRefused, Status: 403,
		GuardrailAction: "refuse_403", GuardrailDirection: "request", GuardrailTier: "judge",
		JudgeVerdict: "block", JudgeReason: "r",
	})
	o.ObserveModelRequest(observe.ModelRequest{
		ServiceID: "svc-j", Outcome: observe.ModelAnswered, Status: 502, Metered: true,
		GuardrailAction: "block", GuardrailDirection: "answer", GuardrailTier: "judge",
		JudgeVerdict: "block", JudgeReason: "r",
	})
	// A judge that allowed, or failed and let the request through, announces
	// nothing.
	o.ObserveModelRequest(observe.ModelRequest{ServiceID: "svc-j", Outcome: observe.ModelAnswered, Status: 200, JudgeVerdict: "allow"})
	o.ObserveModelRequest(observe.ModelRequest{ServiceID: "svc-j", Outcome: observe.ModelAnswered, Status: 200, JudgeVerdict: "failed", JudgeFailure: "timeout"})
	got := em.named("guardrail.refused")
	if len(got) != 2 {
		t.Fatalf("%d guardrail.refused events: %v", len(got), got)
	}
	want := []map[string]any{
		{"ServiceID": "svc-j", "Pattern": "", "direction": "request", "tier": "judge", "action": "refuse_403"},
		{"ServiceID": "svc-j", "Pattern": "", "direction": "answer", "tier": "judge", "action": "block"},
	}
	for i, w := range want {
		fieldsOf(t, "guardrail.refused", got[i])
		for k, v := range w {
			if got[i][k] != v {
				t.Errorf("event %d: %v, want %s=%v", i, got[i], k, v)
			}
		}
	}
}

// The chain is built before the gateway: until the gateway is handed over,
// the judge is unavailable.
func TestJudgeAdapter(t *testing.T) {
	var j judgeAdapter
	if _, err := j.judge(context.Background(), "svc", "m", "p", "t"); !errors.Is(err, aigw.ErrJudgeUnavailable) {
		t.Fatalf("before set: %v", err)
	}
	j.set(&aigateway.Gateway{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	// A gateway without models answers with an error of its own.
	if _, err := j.judge(context.Background(), "svc", "m", "p", "t"); err == nil || errors.Is(err, aigw.ErrJudgeUnavailable) {
		t.Fatalf("after set: %v", err)
	}
}
