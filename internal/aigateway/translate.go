package aigateway

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/aigw/translate"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
	"github.com/ankoehn/burrow/internal/aimeter"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
	"github.com/ankoehn/burrow/internal/version"
)

// Translation between formats, for models that opt in (db.AIModel.Translate).
//
// The rule, in one place: a request is translated only when the model has no
// target that can serve it as it is. "As it is" means a target of the
// caller's dialect that offers the endpoint; when there is one, the
// candidates are those targets and nothing else, with the flag or without
// it. A native target that fails (a 5xx, an open breaker, a timeout) is
// therefore not followed by a translated one: the model has a target of the
// caller's format, and the caller gets what a model without the flag would
// get. Only a model with no such target gets translated candidates, and then
// every candidate of the request is translated.
//
// What the chain's steps see of a translated request (the chain runs once per
// client request, outside the failover handler, as for a native one):
//
//   - rate limits, redaction and guardrails see the caller's body, in the
//     caller's format, before anything is translated; each attempt is
//     translated from the bytes the chain passes on, so an upstream gets the
//     redacted prompt. A "refuse_safe" guardrail answers in the caller's
//     format;
//   - the response cache is not used: neither read nor written
//     (aigw.WithoutCache). Its key is the first target's service and model,
//     which a native request for that target shares, and the stored answer
//     would be in the wrong format for one of the two;
//   - the inspector sees the caller's request and the answer the caller got;
//   - a token count for such a model is the gateway's estimate, and runs
//     through the chain like any request (see serveEstimate);
//   - the usage row holds the upstream's own figures: the upstream's bytes are
//     metered before they are translated (aigw.UpstreamUsage), in the
//     target's format, and the row names the pair and what it left out.
//
// A direct address ("<provider>/<model>") is never translated. It has no
// model row to opt in with and no failover timers, so no translated path
// exists without a timer: every translated attempt belongs to a synthetic
// model and runs under its attempt and total timeouts (or the defaults).

const (
	headerTranslated = "Burrow-Translated" // the id of the pair that translated the answer
	headerDropped    = "Burrow-Dropped"    // what the translation left out; absent when nothing
	headerEstimated  = "Burrow-Estimated"  // "1": the token count is the gateway's estimate
	headerErrorCode  = "Burrow-Error-Code"

	pathCountTokens = "/v1/messages/count_tokens"
)

// Error codes of an attempt row that only a translated attempt has. Two are
// the pair's own (see (*translatedWriter).failure):
//
//   - translate.CodeUpstreamInvalid, "upstream_invalid": the provider answered
//     a success status and the answer could not be used: malformed, cut off,
//     compressed, too large, or not a stream where one was asked for;
//   - translate.CodeUpstreamError, "upstream_error": the provider answered a
//     success status with an error in place of an answer.
//
// The row's status is the provider's in both. The third is the gateway's:
const (
	// The request could not be written for this target for a reason that is
	// not the caller's fault; nothing was sent.
	attemptTranslateError = "translate_error"
)

// What bounds the error body of a translated attempt once its status is the
// caller's answer: the response is then committed and no attempt timer can
// end it, so the gateway stops reading itself. The pair keeps at most 64 KiB
// of an error body to read the message from.
const (
	errorBodyWaitUnits = 5 // units (seconds) from the status to the end of the body
	maxErrorBodyRead   = 64 << 10
)

// userAgent is what a translated request says it comes from. The caller's
// own User-Agent names a client of another vendor's API and stays behind.
var userAgent = "burrow/" + version.Version

// Why a request has no candidate.
const (
	whyFormatMismatch      = "format_mismatch"
	whyEndpointUnsupported = "endpoint_unsupported"
	whyModelNotFound       = "model_not_found"
)

// lookupPair finds the released pair for a caller and a target format. A
// variable for the tests only.
var lookupPair = translate.Lookup

// candidatesForRequest returns what to try for a resolved request on path,
// in order, or why there is nothing to try. The rule is chooseTargets; here
// its result is expanded into candidates.
func (g *Gateway) candidatesForRequest(ctx context.Context, res Resolution, d *Dialect, path string) (cands []candidate, why string) {
	other := func() []Target {
		if len(res.Targets) > 0 && res.Other == nil {
			// Only now, when the request's own dialect has nothing that
			// offers the endpoint, are the other dialect's providers read.
			return g.otherTargets(ctx, res)
		}
		return res.Other
	}
	native, translated, why := chooseTargets(res.Targets, other, res.Synthetic && res.Model.Translate, d.Name, path)
	if len(native) > 0 {
		return candidatesFor(native), ""
	}
	for _, t := range translated {
		for _, c := range candidatesFor([]Target{t.Target}) {
			c.pair, c.pos = t.pair, len(cands)
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return nil, why
	}
	return cands, ""
}

// translatedTarget is a target reached through a translating pair.
type translatedTarget struct {
	Target
	pair translate.Pair
}

// chooseTargets is the one rule for how a model is served on an endpoint. The
// request path (candidatesForRequest) and the model view of the management
// API (ServedModes) both call it, so they cannot disagree.
//
// own are the usable targets of the request's dialect, in order; other
// returns those of the other dialect and is called only when they are
// needed. translate says the model is a synthetic one with translation
// turned on.
//
// Native targets are the ones in own that offer the endpoint. If there is
// one, they are the whole answer. Otherwise, and only with translate on an
// endpoint that has a caller format (messages, chat completions, creating a
// response), the answer is translated targets, each through the released
// pair for its provider's format: first the targets of the request's own
// dialect that do not offer the endpoint (an OpenAI-format provider without
// the Responses API), then the targets of the other dialect. A pair that is
// not released contributes nothing, as if translation were off. why says
// what a request is told when both lists are empty; when translated is not
// empty it is what the request would have been told without translation.
func chooseTargets(own []Target, other func() []Target, translateOn bool, dialect, path string) (native []Target, translated []translatedTarget, why string) {
	var lacking []Target
	for _, t := range own {
		if endpointSupported(path, t.Provider) {
			native = append(native, t)
		} else {
			lacking = append(lacking, t)
		}
	}
	if len(native) > 0 {
		return native, nil, ""
	}
	caller, translatable := translate.CallerFormat(dialect, path)
	var others []Target
	if len(own) == 0 || (translateOn && translatable) {
		others = other()
	}
	switch {
	case len(own) > 0:
		why = whyEndpointUnsupported
	case len(others) > 0:
		why = whyFormatMismatch
	default:
		why = whyModelNotFound
	}
	if !translateOn || !translatable {
		return nil, nil, why
	}
	for _, group := range [][]Target{lacking, others} {
		for _, t := range group {
			if pair, ok := lookupPair(caller, translate.TargetFormat(t.Provider.APIFormat)); ok {
				translated = append(translated, translatedTarget{Target: t, pair: pair})
			}
		}
	}
	return nil, translated, why
}

// The ways a model is served in a format, as the management API names them.
const (
	ModeNative     = "native"     // by a target of that format
	ModeTranslated = "translated" // by a target of another format, through a pair
	ModeNotServed  = "not_served" // a request is refused
)

// Mode says how a model is served on one endpoint. Pairs holds the ids of the
// pairs a translated request can go through, in the order its targets are
// tried, each once; nil unless the mode is translated.
type Mode struct {
	Mode  string
	Pairs []string
}

// Modes says how a synthetic model is served per endpoint a client is
// configured for: OpenAI is Chat Completions on /openai/v1, Anthropic is
// Messages on /anthropic, Responses is creating a response on /openai/v1
// (what Codex uses).
type Modes struct {
	OpenAI, Anthropic, Responses Mode
}

// ServedModes reports how m is served, by the rule a request is routed by
// (chooseTargets). providers maps a slug to its row; as in resolution, a
// target counts only when its provider exists and speaks the target's
// dialect, and a disabled model is served nowhere. It says nothing about
// whether a target can be reached right now: that is the model view's
// "available" and "serving".
func ServedModes(m db.AIModel, providers map[string]db.AIProvider) Modes {
	mode := func(d *Dialect, path string) Mode {
		if !m.Enabled {
			return Mode{Mode: ModeNotServed}
		}
		targets := func(own bool) []Target {
			var out []Target
			for _, t := range m.Targets {
				p, known := providers[t.ProviderSlug]
				if (t.Dialect == d.Name) == own && known && p.APIFormat == t.Dialect {
					out = append(out, Target{Provider: p, Model: t.TargetModel})
				}
			}
			return out
		}
		native, translated, _ := chooseTargets(targets(true), func() []Target { return targets(false) }, m.Translate, d.Name, path)
		switch {
		case len(native) > 0:
			return Mode{Mode: ModeNative}
		case len(translated) > 0:
			out := Mode{Mode: ModeTranslated}
			for _, t := range translated {
				if id := t.pair.ID(); !slices.Contains(out.Pairs, id) {
					out.Pairs = append(out.Pairs, id)
				}
			}
			return out
		}
		return Mode{Mode: ModeNotServed}
	}
	return Modes{
		OpenAI:    mode(DialectOpenAI, "/v1/chat/completions"),
		Anthropic: mode(DialectAnthropic, "/v1/messages"),
		Responses: mode(DialectOpenAI, pathResponses),
	}
}

// listedIn reports whether a model with these modes belongs in the model
// list of dialect d: it is served on one of the dialect's endpoints, natively
// or through translation. The modes are ServedModes', so the list, the model
// view and the request path share one rule (chooseTargets).
func listedIn(modes Modes, d *Dialect) bool {
	served := func(m Mode) bool { return m.Mode != ModeNotServed }
	if d == DialectAnthropic {
		return served(modes.Anthropic)
	}
	return served(modes.OpenAI) || served(modes.Responses)
}

// translation is one attempt's translated request.
type translation struct {
	pair    translate.Pair
	body    []byte   // the request in the target's format
	caller  []byte   // the caller's request, as the chain passed it on
	stream  bool     // the caller asked for a stream
	dropped []string // what the pair left out
}

// upstreamHeader returns the request header a translated attempt is sent
// with: an allow-list, built from nothing of the caller's. The body is the
// gateway's own JSON; the answer is asked for in the form the pair reads; the
// User-Agent is the gateway's; a Messages target gets the API version the
// pair writes for. The target's
// credential is added afterwards, by what adds it to a native attempt.
//
// Nothing else goes upstream: not the caller's credentials or cookies, not
// "anthropic-*", "OpenAI-*" or a coding client's session headers (they are
// addressed to another vendor), and not Accept-Encoding (a compressed answer
// cannot be translated).
func (t *translation) upstreamHeader(p db.AIProvider) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(t.body)))
	if t.stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("User-Agent", userAgent)
	if translate.TargetFormat(p.APIFormat) == translate.Messages {
		h.Set("Anthropic-Version", messages.Version)
	}
	return h
}

// translatedWriter is what the upstream handler of a translated attempt
// writes to: the pair's response writer, with the upstream's status noted and
// the upstream's bytes metered on their way in.
//
// An upstream error status is put to the failover's decision the moment it
// arrives, as on a native attempt, not when its body has ended: a retryable
// one discards the attempt and cancels the upstream at once. One that is the
// caller's answer still needs its body, for the message; that body is read
// within errorBodyWaitUnits and maxErrorBodyRead, then the upstream is
// cancelled and the answer written with what had come.
type translatedWriter struct {
	translate.ResponseWriter
	cw      *commitWriter
	cancel  func() // ends the attempt's upstream call
	wait    time.Duration
	pair    string
	dropped []string
	usage   *aigw.UpstreamUsage
	status  int // the upstream's; 0 = it has not answered

	errorBody    bool        // the body being read is that of an error that is the caller's answer
	errorBytes   int         // how much of it has come
	errorTimer   *time.Timer // ends the wait for it
	clientFailed bool        // the caller's writer refused a write
}

func (t *translatedWriter) WriteHeader(status int) {
	if t.status == 0 && status >= 200 {
		t.status = status
		if status >= 400 && !t.cw.Decide(status) {
			t.errorBody = true
			t.errorTimer = time.AfterFunc(t.wait, t.cancel)
		}
	}
	t.ResponseWriter.WriteHeader(status)
}

func (t *translatedWriter) Write(p []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	if t.errorBody {
		if t.errorBytes += len(p); t.errorBytes > maxErrorBodyRead {
			t.cancel()
		}
	}
	_, _ = t.usage.Write(p)
	n, err := t.ResponseWriter.Write(p)
	if err != nil {
		// The pair's writer fails a write only when the caller's did.
		t.clientFailed = true
	}
	if !t.errorBody && t.Discarding() {
		// The answer has failed or outgrown the pair's limit: nothing the
		// upstream still sends will be used. A committed stream has no
		// attempt timer left to end it, and the meter above keeps a line
		// until its newline comes, so an upstream that goes on sending is
		// not read to its end, or without end: its call stops here. (An
		// error body has its own bounds, see WriteHeader.)
		t.cancel()
	}
	return n, err
}

// Flush, FlushError, Unwrap and NoteUpstreamTimeout pass on what a provider
// handler and http.ResponseController ask of the writer below.
func (t *translatedWriter) Flush() { _ = t.FlushError() }
func (t *translatedWriter) FlushError() error {
	return http.NewResponseController(t.ResponseWriter).Flush()
}
func (t *translatedWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
func (t *translatedWriter) NoteUpstreamTimeout() {
	if tn, ok := t.ResponseWriter.(interface{ NoteUpstreamTimeout() }); ok {
		tn.NoteUpstreamTimeout()
	}
}

// newTranslatedWriter wraps an attempt's commit writer. The caller's status
// line is not written when the upstream's arrives but with the first event of
// the answer (see translate.ResponseWriter), so until then the attempt can
// still be discarded for the next candidate. cancel ends the attempt's
// upstream call.
func (f *failover) newTranslatedWriter(cw *commitWriter, t *translation, p db.AIProvider, cancel func()) *translatedWriter {
	return &translatedWriter{
		ResponseWriter: t.pair.Response(cw, translate.ResponseOptions{
			Stream:         t.stream,
			RequestedModel: f.requested,
			CallerRequest:  t.caller,
			OnError: func(_ int, code string) {
				// Before the status line only: an error that ends a stream
				// has no header to put it in.
				if !cw.committed && !cw.discarded {
					cw.Header().Set(headerErrorCode, code)
				}
			},
		}),
		cw:      cw,
		cancel:  cancel,
		wait:    errorBodyWaitUnits * f.g.unit(),
		pair:    t.pair.ID(),
		dropped: t.dropped,
		usage:   aigw.NewUpstreamUsage(aigw.Kind(p.APIFormat)),
	}
}

// serve calls the upstream of a translated attempt and ends the caller's
// answer. client is the context of the caller's request.
//
// A provider handler panics http.ErrAbortHandler when it cannot copy the rest
// of a response: the upstream broke off, or the gateway cancelled it. On a
// native attempt that aborts the caller's connection, the only way to say
// "this is not all of it". A translated answer has a better one: Finish ends
// the caller's stream with the error event of its format (or, before the
// first event, makes the attempt an error response the failover can move on
// from), and the response is then complete as HTTP. So that panic ends here,
// unless it is the caller who went away: its context says so, or its writer
// refused a write before the context did. Then, and for any other panic (a
// fault in a handler), the panic is passed on untouched, as on a native
// attempt; Finish is not called for it, so nothing is written for an attempt
// that did not get to answer.
func (t *translatedWriter) serve(upstream http.Handler, req *http.Request, client interface{ Err() error }) {
	defer func() {
		p := recover()
		if t.errorTimer != nil {
			t.errorTimer.Stop()
		}
		if p != nil && (p != http.ErrAbortHandler || client.Err() != nil || t.clientFailed) {
			panic(p)
		}
		t.Finish()
	}()
	upstream.ServeHTTP(t, req)
}

// commit sets what a translated answer says about itself, just before its
// status line: the pair, and what it left out.
func (t *translatedWriter) commit(h http.Header, route *aigw.Route) {
	h.Set(headerTranslated, t.pair)
	if dropped := aimeter.JoinDropped(t.dropped); dropped != "" {
		h.Set(headerDropped, dropped)
	}
	route.SetTranslation(t.pair, t.dropped)
}

// failure says how a translated attempt whose upstream was called went
// wrong, for the attempt log and the breaker; ok is false when it did not, or
// when the upstream's own error status says it all (the row is then
// "http_<status>", as for a native attempt).
//
//   - midStream: the caller's stream had begun and was ended with its error
//     event. For the breaker the start was the answer, as on a native attempt
//     whose stream breaks off.
//   - otherwise the upstream answered a status that is no error (or nothing
//     at all) and the answer was no use: that counts against the provider like
//     a 5xx. status is the upstream's own, not the 502 the caller is given.
func (t *translatedWriter) failure() (code string, status int, midStream, ok bool) {
	code, midStream = t.Failure()
	switch {
	case code == "":
		return "", 0, false, false
	case midStream:
		return attemptStreamAborted, t.status, true, true
	case t.status >= 400:
		return "", 0, false, false
	}
	return code, t.status, false, true
}

// estimateTargets reports whether POST /v1/messages/count_tokens for this
// resolution is answered by the gateway itself, and names the targets the
// request can run under: those a released pair reaches, in the order a message
// to the model would try them. That is so for a synthetic model that has no
// Anthropic-format target and has translation turned on. No target of another
// format counts tokens the way the caller's client expects.
func (g *Gateway) estimateTargets(res Resolution, d *Dialect, path string) []Target {
	if d.Name != DialectAnthropic.Name || path != pathCountTokens || !res.Synthetic || !res.Model.Translate || len(res.Targets) > 0 {
		return nil
	}
	var out []Target
	for _, t := range res.Other {
		if _, ok := lookupPair(translate.Messages, translate.TargetFormat(t.Provider.APIFormat)); ok {
			out = append(out, t)
		}
	}
	return out
}

// serveEstimate answers a token count with the gateway's own estimate, which
// says it is one (Burrow-Estimated: 1). Nothing is sent to a provider, but
// the request is a request like any other: the policy of the service it runs
// under (a target's, as for a message to that model) is checked first, and
// it goes through the chain, so rate limits, redaction and guardrails apply
// and it counts against the limits. The chain's "upstream" is the estimate
// itself. As for a native count there is no usage row, and the cache is not
// used.
//
// It runs under the first of targets whose policy can be read. A message to
// the model would leave a target whose policy cannot be read (its tunnel is
// offline, a lookup failed) for the next one; a count does the same, so it is
// answered whenever a message would be. A policy that is read and refuses
// ends the request, as it does for a message.
func (g *Gateway) serveEstimate(w http.ResponseWriter, r *http.Request, d *Dialect, key store.GatewayKey, requested string, targets []Target, body *requestBody) {
	route := aigw.NewRoute(key.ID, d.Name, requested, w.Header().Get(headerRequestID))
	r = r.WithContext(aigw.WithRoute(r.Context(), route))
	for _, t := range targets {
		route.SetTarget(t.Provider.Slug, t.Model)
		host, checked, ok := g.firstTargetPolicy(w, r, t.Provider)
		if !ok {
			return
		}
		if !checked {
			continue
		}
		r = r.WithContext(aigw.WithoutUsage(aigw.WithoutCache(aigw.WithOwnCredential(r.Context()))))
		setBody(r, body.WithModel(t.Model))
		stripCredentials(r)
		estimate := http.HandlerFunc(g.writeEstimate)
		if g.Chain == nil {
			estimate(w, r)
			return
		}
		g.Chain.DispatchMetered(w, r, t.Provider.ServiceID, host, "Authorization", "", false, estimate)
		return
	}
	// No target's policy could be read. There is no attempt here to fail in
	// its place: nothing is answered for a caller that could not be checked.
	g.fail(w, r, http.StatusServiceUnavailable, "provider_unavailable", "the provider of this model cannot be reached")
}

// writeEstimate stands where an upstream stands for a token count the
// gateway answers itself: it reads the body the chain passed on.
func (g *Gateway) writeEstimate(w http.ResponseWriter, r *http.Request) {
	body, err := readRequestBody(r, math.MaxInt64-1) // the size was checked before the chain
	if err != nil {
		g.fail(w, r, http.StatusBadRequest, "invalid_request", "could not read the request body")
		return
	}
	n, err := estimateTokens(body.Raw())
	if err != nil {
		message, ok := translate.BadRequest(err)
		if !ok {
			message = "the request cannot be read"
		}
		g.fail(w, r, http.StatusBadRequest, "invalid_request", message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(headerEstimated, "1")
	_, _ = w.Write([]byte(`{"input_tokens":` + strconv.Itoa(n) + `}`))
}

// estimateTokens estimates the input tokens of a Messages request: the UTF-8
// bytes of the system prompt, of every text, tool call (name and input) and
// tool result, and of every tool's name, description and schema, divided by
// four and rounded up. Images are not counted.
func estimateTokens(body []byte) (int, error) {
	// Counting needs no output cap, which a message does: the decoder is
	// given one when the request has none.
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) == nil && fields != nil {
		if _, has := fields["max_tokens"]; !has {
			fields["max_tokens"] = json.RawMessage("1")
			if with, err := json.Marshal(fields); err == nil {
				body = with
			}
		}
	}
	req, err := messages.DecodeRequest(body)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range req.System {
		n += len(p.Text)
	}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			n += len(p.Text) + len(p.ToolName) + len(p.Input)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	return (n + 3) / 4, nil
}
