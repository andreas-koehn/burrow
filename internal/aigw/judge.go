package aigw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/inspector"
	"github.com/ankoehn/burrow/internal/observe"
)

// What the app is told when the judge refuses its request.
const (
	codeJudgeRefused    = "guardrail_judge_refused"
	messageJudgeRefused = "the request was refused by a guardrail"
)

// tierJudge names the judge in observe.ModelRequest.GuardrailTier.
const tierJudge = "judge"

type noGuardrailsKey struct{}

// WithoutGuardrails marks a request the guardrails do not touch: no pattern
// tier, no judge and no answer inspection. The judge's own calls carry it, so
// a judge never judges itself.
func WithoutGuardrails(ctx context.Context) context.Context {
	return context.WithValue(ctx, noGuardrailsKey{}, true)
}

// GuardrailsSkipped reports whether WithoutGuardrails was set.
func GuardrailsSkipped(ctx context.Context) bool {
	off, _ := ctx.Value(noGuardrailsKey{}).(bool)
	return off
}

// PurposeGuardrailJudge is the purpose of a call the relay makes to the judge
// model (see WithPurpose).
const PurposeGuardrailJudge = "guardrail-judge"

type purposeKey struct{}

// WithPurpose says why the relay itself makes a request: PurposeGuardrailJudge
// for a judge call. The chain copies it into the request's observe event and
// inspector entry. A request of an app has none.
func WithPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, purposeKey{}, purpose)
}

// PurposeFrom returns what WithPurpose stored, or "".
func PurposeFrom(ctx context.Context) string {
	p, _ := ctx.Value(purposeKey{}).(string)
	return p
}

// JudgeEvent is a judgement that leaves an audit entry (see
// Chain.OnGuardrailJudge): the judge blocked a request or an answer, or it
// failed.
type JudgeEvent struct {
	Direction string // "request" or "answer"
	Verdict   string // JudgeBlock or JudgeFailed
	Reason    string // the judge's reason for a block; never the judged text
	Failure   string // the class of a failure: JudgeTimeout, JudgeError, JudgeMalformed, JudgeUnavailable
	Refused   bool   // the request was refused, or the answer withheld
}

// The verdicts the chain records.
const (
	JudgeAllow  = "allow"
	JudgeBlock  = "block"
	JudgeFailed = "failed"
	// JudgeNotJudged: the judge applied but there was no text to show it (an
	// image-only request, an answer of tool calls only). Recorded on the
	// inspector entry and the observe event, never audited.
	JudgeNotJudged = "not_judged"
)

// reasonNoText is the reason of a JudgeNotJudged mark.
const reasonNoText = "no text"

// The classes of a judge failure.
const (
	JudgeTimeout     = "timeout"     // no verdict within timeout_ms
	JudgeError       = "error"       // the call failed (an error answer, no text, the caller left)
	JudgeMalformed   = "malformed"   // the answer is no verdict (guardrails.ParseVerdict)
	JudgeUnavailable = "unavailable" // there is no judge to ask
	JudgeTooLarge    = "too_large"   // the answer was larger than the guard holds
)

// ErrJudgeUnavailable is what a judge function returns when there is no
// judge to ask (the gateway is not wired yet). The chain counts it as the
// failure class "unavailable", like a chain without a judge.
var ErrJudgeUnavailable = errors.New("aigw: no judge is available")

// judgement is what the chain made of one judge call.
type judgement struct {
	verdict string // JudgeAllow, JudgeBlock or JudgeFailed
	reason  string // the model's reason for a block
	failure string // the class of a failure
	refuse  bool   // the request is refused, or the answer withheld
}

// judgeFormat returns the settings of the judge and the text format of the
// request when the judge applies to it: the service has guardrail settings,
// the judge is on (on its own switch: the request patterns' Enabled does not
// gate it), the request does not carry WithoutGuardrails, and the request is
// a POST to an endpoint whose request text the guardrails read: Chat
// Completions, Responses, Messages, legacy completions, or a media or rerank
// endpoint.
func judgeFormat(r *http.Request, kind Kind, cfg ServiceAIConfig) (guardrails.JudgeSettings, string, bool) {
	g := cfg.Guardrails
	if g == nil || !g.Judge.Enabled || r.Method != http.MethodPost || GuardrailsSkipped(r.Context()) {
		return guardrails.JudgeSettings{}, "", false
	}
	format := answerFormat(kind, r.URL.Path)
	if format == "" {
		// Endpoints with request text only (the answers of media endpoints
		// are audio, images or scores; legacy completions' answers are not
		// read).
		format = requestOnlyFormat(r.URL.Path)
	}
	return g.Judge, format, format != ""
}

// judge asks the judge about text (cut to guardrails.MaxJudgedText bytes) and
// returns what it decided. It returns within the judge's timeout whatever the
// judge function does: the call runs on its own goroutine, and one that
// outlives its time is left to end on its cancelled context. A failure (no
// judge, a timeout, an error, an answer that is no verdict) refuses when the
// settings say so.
func (c *Chain) judge(ctx context.Context, serviceID string, j guardrails.JudgeSettings, text string) judgement {
	fail := func(class string) judgement { return c.judgeFailure(serviceID, j, class) }
	judgeFn := c.Judge
	if judgeFn == nil {
		return fail(JudgeUnavailable)
	}
	timeout := time.Duration(j.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = guardrails.DefaultJudgeTimeoutMS * time.Millisecond
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		answer string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("aigw: judge panicked: %v", p)}
			}
		}()
		answer, err := judgeFn(jctx, serviceID, j.Model, j.Policy, guardrails.JudgedText(text))
		done <- result{answer, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-jctx.Done():
		res.err = jctx.Err()
	}
	switch {
	case res.err != nil && errors.Is(jctx.Err(), context.DeadlineExceeded):
		return fail(JudgeTimeout)
	case errors.Is(res.err, ErrJudgeUnavailable):
		return fail(JudgeUnavailable)
	case res.err != nil:
		return fail(JudgeError)
	}
	v, err := guardrails.ParseVerdict(res.answer)
	if err != nil {
		return fail(JudgeMalformed)
	}
	if v.Block {
		return judgement{verdict: JudgeBlock, reason: v.Reason, refuse: true}
	}
	return judgement{verdict: JudgeAllow}
}

// judgeFailure is a failed judgement of class: it refuses when the settings
// say so. It is logged, without the text.
func (c *Chain) judgeFailure(serviceID string, j guardrails.JudgeSettings, class string) judgement {
	c.Log.Warn("aigw: guardrail judge failed",
		slog.String("service_id", serviceID),
		slog.String("model", j.Model),
		slog.String("failure", class),
	)
	return judgement{verdict: JudgeFailed, failure: class, refuse: j.RefusesOnFailure()}
}

// notJudgedMark records on the event that the judge applied to direction but
// had no text to judge, and returns the inspector's mark for it.
func notJudgedMark(ev *observe.ModelRequest, direction string) *inspector.JudgeMark {
	ev.JudgeVerdict, ev.JudgeReason, ev.JudgeFailure, ev.JudgeDirection = JudgeNotJudged, reasonNoText, "", direction
	return &inspector.JudgeMark{Direction: direction, Verdict: JudgeNotJudged, Reason: reasonNoText}
}

// note records a judgement about direction ("request" or "answer") on the
// request's event, reports it to OnGuardrailJudge when it leaves an audit
// entry (a block, a failure), and returns the inspector's mark for it.
func (c *Chain) note(r *http.Request, svc Service, ev *observe.ModelRequest, jd judgement, direction string) *inspector.JudgeMark {
	ev.JudgeVerdict, ev.JudgeReason, ev.JudgeFailure, ev.JudgeDirection = jd.verdict, jd.reason, jd.failure, direction
	if jd.verdict != JudgeAllow && c.OnGuardrailJudge != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					c.Log.Error("aigw: guardrail judge hook panicked",
						slog.String("service_id", svc.ID), slog.Any("panic", rec))
				}
			}()
			c.OnGuardrailJudge(r.Context(), svc.ID, JudgeEvent{
				Direction: direction, Verdict: jd.verdict, Reason: jd.reason, Failure: jd.failure, Refused: jd.refuse,
			})
		}()
	}
	return &inspector.JudgeMark{Direction: direction, Verdict: jd.verdict, Reason: jd.reason, Failure: jd.failure,
		Refused: jd.verdict == JudgeFailed && jd.refuse}
}

// requestOnlyFormat is the guardrails text format of a request whose answer
// the guardrails do not read: legacy completions, media and rerank; "" for
// any other path.
func requestOnlyFormat(path string) string {
	switch strings.TrimRight(path, "/") {
	case "/v1/completions":
		return guardrails.FormatCompletions
	case "/v1/images/generations":
		return guardrails.FormatImages
	case "/v1/audio/speech":
		return guardrails.FormatSpeech
	case "/v1/rerank":
		return guardrails.FormatRerank
	case "/v1/audio/transcriptions", "/v1/audio/translations":
		return guardrails.FormatTranscription
	}
	return ""
}
