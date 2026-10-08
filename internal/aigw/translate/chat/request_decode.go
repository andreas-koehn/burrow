package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// This file and response_encode.go, stream_encode.go hold the half a caller
// needs: read a request into the neutral form (DecodeRequest), write an
// answer from it, whole (EncodeResponse) or streamed (StreamEncoder), and
// write an error in the OpenAI shape (EncodeError). CheckStream reads a
// stream the way a client does and is the yardstick of the encoder.

// format is the Format of this package's *ir.BadRequestError.
const format = "chat"

// Names DecodeRequest adds to the dropped list besides those of package ir
// (all fixed, none chosen by a client).
const (
	droppedParallelToolCalls = "parallel_tool_calls"
	droppedToolsStrict       = "tools.strict"
	droppedResponseFormat    = "response_format"
	droppedN                 = "n"
	droppedImageDetail       = "image_url.detail"
	droppedMessageName       = "messages.name"
	droppedMessageAudio      = "messages.audio"
	droppedAnnotations       = "messages.annotations"

	// What is repaired in the history of tool calls and their results.
	droppedCallArguments  = "input:tool_call.arguments"  // arguments that are no JSON object: the call has none
	droppedUnanswered     = "input:tool_call.unanswered" // a call without a tool message got a result
	droppedLongID         = "input:tool_call.id"         // an id longer than ir.MaxToolIDBytes was cut
	droppedDuplicateCall  = "input:tool_call.duplicate"  // a call whose id waits already: left out
	droppedOrphanTool     = "input:tool.orphan"          // a tool message for no waiting call: left out
	droppedOrphanFunction = "input:function.orphan"      // a legacy function message for no call: left out
)

// droppedWhenAsked are the top-level fields that are left out and reported
// by their own name whenever they ask for something (see asked).
var droppedWhenAsked = []string{
	"logprobs", "top_logprobs", "logit_bias", "seed", "reasoning_effort", "user", "metadata", ir.DroppedStore,
	"prediction", "audio", "web_search_options", "verbosity", "prompt_cache_key", "prompt_cache_retention", "safety_identifier",
}

// legacyID is the id a legacy function_call is given: "call_legacy_<n>".
const legacyID = "call_legacy_"

func badRequest(field, reason string) error {
	return &ir.BadRequestError{Format: format, Field: field, Reason: reason}
}

func tooManyElements(field string, limit int) error {
	return &ir.BadRequestError{Format: format, Field: field, Reason: fmt.Sprintf("has more than %d elements", limit), Limit: true}
}

// IncludeUsage reports whether a Chat Completions request asks for the usage
// chunk of a stream ("stream_options": {"include_usage": true}). A body that
// cannot be read asks for none.
func IncludeUsage(body []byte) bool {
	var req struct {
		StreamOptions struct {
			IncludeUsage json.RawMessage `json:"include_usage"`
		} `json:"stream_options"`
	}
	if ir.Depth(body) > ir.MaxDepth || json.Unmarshal(body, &req) != nil {
		return false
	}
	return string(bytes.TrimSpace(req.StreamOptions.IncludeUsage)) == "true"
}

// DecodeRequest reads a Chat Completions request into the neutral form.
// Every field is carried, refused, or named in Request.Dropped.
//
// Carried: "model"; "messages" (see below); "tools" of type "function"
// (name, description, the schema's bytes unchanged); "tool_choice" ("auto",
// "none", "required", a named function); "max_completion_tokens", else
// "max_tokens"; "temperature", "top_p"; "stop" (a string or a list);
// "stream". "stream_options.include_usage" is read by IncludeUsage for the
// answer and not reported.
//
// Messages:
//   - system and developer: text, as a string or text parts, gathered in the
//     system prompt in order; one that stands after the first turn of the
//     conversation loses its place: ir.DroppedSystemPosition;
//   - user: a string, or parts: "text", and "image_url" (a base64 data URL
//     is split into media type and data, any other URL is kept as a URL);
//   - assistant: "content" as a string, null or parts ("text", "refusal"),
//     "refusal" as text after it, "reasoning_content" as thinking before it
//     (the target's encoder decides about it), and "tool_calls" of type
//     "function" (id, name, "arguments" as the text of one JSON object, kept
//     byte for byte; an object in its place is taken as it is). The legacy
//     "function_call" is a tool call with the id "call_legacy_<n>". An
//     assistant message that holds none of this is no turn;
//   - tool: a tool result ("tool_call_id", "content" as a string or parts:
//     text, joined by "\n", and image_url). The legacy role "function"
//     answers the oldest legacy call of its name that waits.
//
// # Calls and their results
//
// A target wants every tool call answered, directly after the turn that made
// it, and a client that replays its history with every request would meet
// the same 400 for ever when it is not. So a conversation is built that both
// targets take, and what was repaired is reported:
//   - assistant messages that follow each other are ONE turn, up to the
//     first tool message that answers one of its calls;
//   - a tool message belongs to the call with its id, wherever it stands —
//     after a later message, or before its call: the results of a turn stand
//     first in the user message that follows it, in the order of the calls,
//     and such a message is made when there is none;
//   - the images a tool returned follow the results in that message, each
//     tool's after a text that names its call (ir.ToolImageNote); the result
//     keeps its text, or says ir.ToolImageText. Nothing is lost, so nothing
//     is reported;
//   - a call without a tool message gets the result ir.ToolNoOutput:
//     "input:tool_call.unanswered";
//   - an id longer than ir.MaxToolIDBytes is cut, the same way on the call
//     and on its tool message: "input:tool_call.id";
//   - a tool message whose call never comes, and a second one for one call,
//     are left out: "input:tool.orphan" ("input:function.orphan" for the
//     legacy role);
//   - a call whose id waits for its result already is left out:
//     "input:tool_call.duplicate". The limit of ir.MaxToolCalls calls in
//     one turn counts the calls that are kept;
//   - "arguments" that are not the text of one JSON object become {}:
//     "input:tool_call.arguments";
//   - a tool call of another type (custom) is left out: ir.DroppedInput(type);
//     its tool message is an orphan then.
//
// Left out and reported:
//   - "n" above 1 (one choice is answered): "n"; "response_format" other
//     than {"type":"text"} (structured output is not carried):
//     "response_format"; "parallel_tool_calls": false: "parallel_tool_calls";
//   - "logprobs", "top_logprobs", "logit_bias", "seed", "reasoning_effort",
//     "user", "metadata", "store", "prediction", "audio",
//     "web_search_options", "verbosity", "prompt_cache_key",
//     "prompt_cache_retention", "safety_identifier": by their name, when
//     they ask for something — a null, a false, an empty string, list or
//     object asks for nothing; "presence_penalty" and "frequency_penalty"
//     other than 0; "service_tier" other than "auto" and "default";
//     "modalities" other than ["text"];
//   - a tool of any other type: ir.DroppedTool(type); "strict": true on a
//     function: "tools.strict"; a "tool_choice" object that is not a named
//     function: "tool_choice";
//   - an image's "detail" other than "auto": "image_url.detail"; a message's
//     "name", "audio" and "annotations": "messages.name", "messages.audio",
//     "messages.annotations"; "cache_control" anywhere: ir.DroppedCacheControl;
//   - any other key: ir.Unknown — at the top level, in a message
//     ("messages.<key>"), a content part ("content.<key>"), a tool
//     ("tools.<key>"), and "stream_options.<key>".
//
// Images are bounded by nothing but the gateway's limit on the request
// body.
//
// Refused with a *ir.BadRequestError that names the field
// ("messages[3].tool_calls[0].id"): a body that is not a JSON object; a
// missing or mistyped field; a role nobody knows; a user message without
// content; a content part that cannot be carried and must not vanish
// (input_audio, file, any unknown part type, an image outside a user or tool
// message, a data URL that is not base64); a tool call without id or name; a
// tool message without "tool_call_id"; the legacy "functions" and
// "function_call" settings, which ask for an answer in a shape that is not
// written; a request in which no turn is left; anything over the ir limits.
// The request returned with an error is empty.
func DecodeRequest(body []byte) (ir.Request, error) {
	d := requestDecoder{pending: map[string]*callerTurn{}, early: map[string]*toolOutput{}}
	if err := d.request(body); err != nil {
		return ir.Request{}, err
	}
	d.req.Dropped = d.dropped
	return d.req, nil
}

type requestDecoder struct {
	req     ir.Request
	dropped []string

	system  []ir.Part
	turns   []*callerTurn
	pending map[string]*callerTurn // the calls that wait for their result: call id → the turn that made it
	early   map[string]*toolOutput // the results that wait for their call
	legacy  []legacyCall           // the legacy calls that wait for their function message
	legacyN int                    // legacy calls so far
}

// callerTurn is one message of the conversation while the request is read: a
// user message, or an assistant turn with the results that answer its calls.
type callerTurn struct {
	msg     ir.Message
	calls   []string               // an assistant turn: its call ids, in order
	outputs map[string]*toolOutput // by call id
	closed  bool                   // a result of its own came: what the assistant says next is a new turn
}

type toolOutput struct {
	text   string
	images []ir.Part
}

type legacyCall struct{ id, name string }

func (d *requestDecoder) drop(name string) { d.dropped = append(d.dropped, name) }

// toolID returns a tool call's id as it is kept: one longer than a tool id
// may be is cut (ir.BoundToolID, the same way on a call and on the tool
// message that answers it) and reported. History is never refused for it.
func (d *requestDecoder) toolID(id string) string {
	id, cut := ir.BoundToolID(id)
	if cut {
		d.drop(droppedLongID)
	}
	return id
}

// object is a JSON object whose values are not decoded yet.
type object map[string]json.RawMessage

// take removes key and returns its value; ok is false when the key is absent
// or null.
func (o object) take(key string) (json.RawMessage, bool) {
	raw, ok := o[key]
	delete(o, key)
	if !ok || isNull(raw) {
		return nil, false
	}
	return raw, true
}

// asked reports whether a value asks for anything: not null, false, "", []
// or {}.
func asked(raw []byte) bool {
	switch string(bytes.Join(bytes.Fields(raw), nil)) {
	case "", "null", "false", `""`, "[]", "{}":
		return false
	}
	return true
}

func asObject(raw []byte) (object, bool) {
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var o object
	if json.Unmarshal(raw, &o) != nil {
		return nil, false
	}
	return o, true
}

func asString(raw []byte) (string, bool) {
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func asBool(raw []byte) (value, ok bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func asNumber(raw []byte) (float64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(trimmed), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// str reads an optional string field.
func (o object) str(key, field string) (string, error) {
	raw, ok := o.take(key)
	if !ok {
		return "", nil
	}
	s, ok := asString(raw)
	if !ok {
		return "", badRequest(field, "is not a string")
	}
	return s, nil
}

// required reads a string field that must be there and not empty.
func (o object) required(key, field string) (string, error) {
	s, err := o.str(key, field)
	if err == nil && s == "" {
		err = badRequest(field, "is required")
	}
	return s, err
}

// number reads an optional top-level number.
func (o object) number(key string) (*float64, error) {
	raw, ok := o.take(key)
	if !ok {
		return nil, nil
	}
	f, ok := asNumber(raw)
	if !ok {
		return nil, badRequest(key, "is not a number")
	}
	return &f, nil
}

// list splits a JSON list into its elements, one at a time: a list over the
// limit is refused before it is held in memory.
func list(raw []byte, limit int, field, reason string) ([]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, badRequest(field, reason)
	}
	var out []json.RawMessage
	for dec.More() {
		if len(out) >= limit {
			return nil, tooManyElements(field, limit)
		}
		var el json.RawMessage
		if err := dec.Decode(&el); err != nil {
			return nil, badRequest(field, reason)
		}
		out = append(out, el)
	}
	return out, nil
}

// unknown reports the keys that are left in o, each with the prefix of the
// place it stands at.
func (d *requestDecoder) unknown(where string, o object) {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.drop(ir.Unknown(where + k))
	}
}

func (d *requestDecoder) cacheControl(o object) {
	if _, ok := o.take("cache_control"); ok {
		d.drop(ir.DroppedCacheControl)
	}
}

func (d *requestDecoder) request(body []byte) error {
	if ir.Depth(body) > ir.MaxDepth {
		return &ir.BadRequestError{Format: format, Field: "body", Reason: fmt.Sprintf("is nested deeper than %d", ir.MaxDepth), Limit: true}
	}
	top, ok := asObject(body)
	if !ok {
		return badRequest("body", "is not a JSON object")
	}
	req := &d.req
	var err error
	if req.Model, err = top.str("model", "model"); err != nil {
		return err
	}
	raw, ok := top.take("messages")
	if !ok {
		return badRequest("messages", "is required")
	}
	if err := d.messages(raw); err != nil {
		return err
	}
	req.System = d.system
	if raw, ok := top.take("tools"); ok {
		if req.Tools, err = d.tools(raw); err != nil {
			return err
		}
	}
	if raw, ok := top.take("tool_choice"); ok {
		if req.ToolChoice, err = d.toolChoice(raw); err != nil {
			return err
		}
	}
	// "max_completion_tokens" is the newer name of the same cap and wins.
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if raw, ok := top.take(key); ok {
			n, ok := asNumber(raw)
			if !ok || n < 1 || n > math.MaxInt32 || n != math.Trunc(n) {
				return badRequest(key, "is not a whole number above 0")
			}
			req.MaxTokens = int(n)
		}
	}
	if req.Temperature, err = top.number("temperature"); err != nil {
		return err
	}
	if req.TopP, err = top.number("top_p"); err != nil {
		return err
	}
	if raw, ok := top.take("stop"); ok {
		if s, ok := asString(raw); ok {
			req.Stop = []string{s}
		} else {
			items, err := list(raw, ir.MaxParts, "stop", "is neither a string nor a list of strings")
			if err != nil {
				return err
			}
			for i, item := range items {
				s, ok := asString(item)
				if !ok {
					return badRequest(fmt.Sprintf("stop[%d]", i), "is not a string")
				}
				req.Stop = append(req.Stop, s)
			}
		}
	}
	if raw, ok := top.take("stream"); ok {
		if req.Stream, ok = asBool(raw); !ok {
			return badRequest("stream", "is not true or false")
		}
	}
	if raw, ok := top.take("stream_options"); ok {
		o, ok := asObject(raw)
		if !ok {
			return badRequest("stream_options", "is not an object")
		}
		delete(o, "include_usage")
		d.unknown("stream_options.", o)
	}
	if raw, ok := top.take("parallel_tool_calls"); ok {
		parallel, ok := asBool(raw)
		if !ok {
			return badRequest("parallel_tool_calls", "is not true or false")
		}
		if !parallel {
			d.drop(droppedParallelToolCalls)
		}
	}
	if raw, ok := top.take("n"); ok {
		n, ok := asNumber(raw)
		if !ok {
			return badRequest("n", "is not a number")
		}
		if n > 1 {
			d.drop(droppedN)
		}
	}
	if raw, ok := top.take("response_format"); ok {
		f, _ := asObject(raw)
		if kind, _ := asString(f["type"]); kind != "text" {
			d.drop(droppedResponseFormat)
		}
	}
	// The legacy function interface: its answer has a shape of its own
	// ("function_call" in place of "tool_calls") that is not written.
	for _, key := range []string{"functions", "function_call"} {
		if raw, ok := top.take(key); ok && asked(raw) {
			return badRequest(key, "the legacy function interface cannot be translated: use tools")
		}
	}
	for _, key := range []string{"presence_penalty", "frequency_penalty"} {
		if raw, ok := top.take(key); ok {
			if n, isNumber := asNumber(raw); !isNumber || n != 0 {
				d.drop(key)
			}
		}
	}
	if raw, ok := top.take("service_tier"); ok {
		if s, isString := asString(raw); asked(raw) && !(isString && (s == "auto" || s == "default")) {
			d.drop("service_tier")
		}
	}
	if raw, ok := top.take("modalities"); ok {
		if string(bytes.Join(bytes.Fields(raw), nil)) != `["text"]` && asked(raw) {
			d.drop("modalities")
		}
	}
	for _, name := range droppedWhenAsked {
		if raw, ok := top.take(name); ok && asked(raw) {
			d.drop(name)
		}
	}
	d.unknown("", top)
	return nil
}

func (d *requestDecoder) messages(raw []byte) error {
	items, err := list(raw, ir.MaxMessages, "messages", "is not a list")
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return badRequest("messages", "is empty")
	}
	for i, item := range items {
		at := fmt.Sprintf("messages[%d]", i)
		o, ok := asObject(item)
		if !ok {
			return badRequest(at, "is not an object")
		}
		role, _ := asString(o["role"])
		delete(o, "role")
		switch role {
		case "system", "developer":
			err = d.systemMessage(o, at)
		case "user":
			err = d.userMessage(o, at)
		case "assistant":
			err = d.assistantMessage(o, at)
		case "tool":
			err = d.toolMessage(o, at)
		case "function":
			err = d.functionMessage(o, at)
		default:
			err = badRequest(at+".role", "is not system, developer, user, assistant, tool or function")
		}
		if err != nil {
			return err
		}
		if role != "function" { // there "name" is the function's
			if raw, ok := o.take("name"); ok && asked(raw) {
				d.drop(droppedMessageName)
			}
		}
		d.cacheControl(o)
		d.unknown("messages.", o)
	}
	return d.conversation()
}

func (d *requestDecoder) systemMessage(o object, at string) error {
	raw, ok := o.take("content")
	if !ok {
		return nil
	}
	parts, err := d.content(raw, "", at+".content")
	if err != nil {
		return err
	}
	if len(parts) > 0 && len(d.turns) > 0 {
		d.drop(ir.DroppedSystemPosition)
	}
	if len(d.system)+len(parts) > ir.MaxParts {
		return tooManyElements("messages", ir.MaxParts)
	}
	d.system = append(d.system, parts...)
	return nil
}

func (d *requestDecoder) userMessage(o object, at string) error {
	raw, ok := o.take("content")
	if !ok {
		return badRequest(at+".content", "is required")
	}
	parts, err := d.content(raw, ir.User, at+".content")
	if err != nil {
		return err
	}
	d.turns = append(d.turns, &callerTurn{msg: ir.Message{Role: ir.User, Parts: parts}})
	return nil
}

// assistant returns the assistant turn that what the assistant says or calls
// next belongs to: the newest turn while it is the assistant's and none of
// its calls has been answered, else a new one.
func (d *requestDecoder) assistant() *callerTurn {
	if n := len(d.turns); n > 0 && d.turns[n-1].msg.Role == ir.Assistant && !d.turns[n-1].closed {
		return d.turns[n-1]
	}
	t := &callerTurn{msg: ir.Message{Role: ir.Assistant}}
	d.turns = append(d.turns, t)
	return t
}

// inOpenTurn reports whether the assistant turn a call would join now holds
// a call with that id.
func (d *requestDecoder) inOpenTurn(id string) bool {
	n := len(d.turns)
	if n == 0 || d.turns[n-1].msg.Role != ir.Assistant || d.turns[n-1].closed {
		return false
	}
	for _, have := range d.turns[n-1].calls {
		if have == id {
			return true
		}
	}
	return false
}

func (d *requestDecoder) assistantMessage(o object, at string) error {
	var parts []ir.Part
	// What an answer of this package's EncodeResponse carries besides its
	// content comes back with a history: the reasoning text.
	if raw, ok := o.take("reasoning_content"); ok {
		if s, isString := asString(raw); isString && s != "" {
			parts = append(parts, ir.Part{Kind: ir.Thinking, Text: s})
		}
	}
	if raw, ok := o.take("content"); ok {
		content, err := d.content(raw, ir.Assistant, at+".content")
		if err != nil {
			return err
		}
		parts = append(parts, content...)
	}
	refusal, err := o.str("refusal", at+".refusal")
	if err != nil {
		return err
	}
	if refusal != "" {
		parts = append(parts, ir.Part{Kind: ir.Text, Text: refusal})
	}
	for key, name := range map[string]string{"audio": droppedMessageAudio, "annotations": droppedAnnotations} {
		if raw, ok := o.take(key); ok && asked(raw) {
			d.drop(name)
		}
	}
	var calls []ir.Part
	if raw, ok := o.take("tool_calls"); ok {
		// The list is bounded like any list of a message; the limit on tool
		// calls is on those that are kept (see below).
		items, err := list(raw, ir.MaxParts, at+".tool_calls", "is not a list")
		if err != nil {
			return err
		}
		for i, item := range items {
			p, keep, err := d.toolCall(item, fmt.Sprintf("%s.tool_calls[%d]", at, i))
			if err != nil {
				return err
			}
			if keep {
				calls = append(calls, p)
			}
		}
	}
	legacy := false
	if raw, ok := o.take("function_call"); ok {
		fn, ok := asObject(raw)
		if !ok {
			return badRequest(at+".function_call", "is not an object")
		}
		p, err := d.function(fn, at+".function_call")
		if err != nil {
			return err
		}
		d.legacyN++
		p.ToolID = legacyID + strconv.Itoa(d.legacyN)
		calls, legacy = append(calls, p), true
	}
	if len(parts) == 0 && len(calls) == 0 {
		return nil // nothing was said: no turn
	}
	// The calls that can stand: an id that waits already, or that the turn
	// this message joins holds, stays with its first call.
	kept := calls[:0]
	seen := map[string]bool{}
	for _, p := range calls {
		if seen[p.ToolID] || d.pending[p.ToolID] != nil || d.inOpenTurn(p.ToolID) {
			d.drop(droppedDuplicateCall)
			continue
		}
		seen[p.ToolID] = true
		kept = append(kept, p)
	}
	if len(parts) == 0 && len(kept) == 0 {
		return nil
	}
	t := d.assistant()
	if len(t.calls)+len(kept) > ir.MaxToolCalls {
		return &ir.BadRequestError{Format: format, Field: at + ".tool_calls", Reason: fmt.Sprintf("is more than %d tool calls in one turn", ir.MaxToolCalls), Limit: true}
	}
	if len(t.msg.Parts)+len(parts)+len(kept) > ir.MaxParts {
		return tooManyElements(at+".content", ir.MaxParts)
	}
	t.msg.Parts = append(append(t.msg.Parts, parts...), kept...)
	for _, p := range kept {
		t.calls = append(t.calls, p.ToolID)
		if t.outputs == nil {
			t.outputs = map[string]*toolOutput{}
		}
		if out := d.early[p.ToolID]; out != nil {
			// Its result stood before it.
			t.outputs[p.ToolID] = out
			delete(d.early, p.ToolID)
			continue
		}
		d.pending[p.ToolID] = t
		if legacy && strings.HasPrefix(p.ToolID, legacyID) {
			d.legacy = append(d.legacy, legacyCall{id: p.ToolID, name: p.ToolName})
		}
	}
	return nil
}

// toolCall reads one element of "tool_calls". keep is false for a call of a
// type that is not carried (and reported).
func (d *requestDecoder) toolCall(raw []byte, at string) (p ir.Part, keep bool, err error) {
	o, ok := asObject(raw)
	if !ok {
		return ir.Part{}, false, badRequest(at, "is not an object")
	}
	kind := "function"
	if raw, ok := o.take("type"); ok {
		if kind, ok = asString(raw); !ok {
			return ir.Part{}, false, badRequest(at+".type", "is not a string")
		}
	} else if _, isFunction := o["function"]; !isFunction {
		return ir.Part{}, false, badRequest(at+".type", "is required")
	}
	if kind != "function" {
		// A call of a tool that is not emulated (its declaration is left out too).
		d.drop(ir.DroppedInput(kind))
		return ir.Part{}, false, nil
	}
	id, err := o.required("id", at+".id")
	if err != nil {
		return ir.Part{}, false, err
	}
	id = d.toolID(id)
	fn, ok := asObject(o["function"])
	delete(o, "function")
	if !ok {
		return ir.Part{}, false, badRequest(at+".function", "is required")
	}
	if p, err = d.function(fn, at+".function"); err != nil {
		return ir.Part{}, false, err
	}
	p.ToolID = id
	delete(o, "index") // a client that recorded a stream may send it back
	d.unknown("messages.tool_calls.", o)
	return p, true, nil
}

// function reads the "function" of a tool call, or a legacy function_call:
// its name and its arguments.
func (d *requestDecoder) function(fn object, at string) (ir.Part, error) {
	p := ir.Part{Kind: ir.ToolUse, Input: json.RawMessage("{}")}
	var err error
	if p.ToolName, err = fn.required("name", at+".name"); err != nil {
		return ir.Part{}, err
	}
	// The history is the client's record of an answer: arguments that are not
	// the text of one JSON object (a call whose stream was cut) must not cost
	// the session. The call stays, with no arguments, and that is reported.
	if raw, ok := fn.take("arguments"); ok {
		text := []byte(raw) // an object where the string should be is taken as it is
		if s, isString := asString(raw); isString {
			text = []byte(s)
		}
		input, err := ir.ToolInput(text)
		switch {
		case err == nil:
			p.Input = input
		case errors.Is(err, ir.ErrLimit):
			return ir.Part{}, &ir.BadRequestError{Format: format, Field: at + ".arguments", Reason: "is too large", Limit: true}
		default:
			d.drop(droppedCallArguments)
		}
	}
	d.unknown("messages.tool_calls.", fn)
	return p, nil
}

func (d *requestDecoder) toolMessage(o object, at string) error {
	id, err := o.required("tool_call_id", at+".tool_call_id")
	if err != nil {
		return err
	}
	id = d.toolID(id)
	out, err := d.toolContent(o, at)
	if err != nil {
		return err
	}
	d.result(id, out, droppedOrphanTool)
	return nil
}

// functionMessage reads a message of the legacy role "function": the result
// of the oldest legacy call of its name that waits, else of the oldest that
// waits at all.
func (d *requestDecoder) functionMessage(o object, at string) error {
	name, err := o.str("name", at+".name")
	if err != nil {
		return err
	}
	out, err := d.toolContent(o, at)
	if err != nil {
		return err
	}
	pick := -1
	for i, c := range d.legacy {
		if d.pending[c.id] == nil {
			continue
		}
		if c.name == name {
			pick = i
			break
		}
		if pick < 0 {
			pick = i
		}
	}
	if pick < 0 {
		d.drop(droppedOrphanFunction)
		return nil
	}
	id := d.legacy[pick].id
	d.legacy = append(d.legacy[:pick], d.legacy[pick+1:]...)
	d.result(id, out, droppedOrphanFunction)
	return nil
}

// toolContent reads the content of a tool or function message: a string, or
// parts — text, joined by "\n", and images.
func (d *requestDecoder) toolContent(o object, at string) (*toolOutput, error) {
	out := &toolOutput{}
	raw, ok := o.take("content")
	if !ok {
		return out, nil
	}
	parts, err := d.content(raw, "tool", at+".content")
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, p := range parts {
		if p.Kind == ir.Image {
			out.images = append(out.images, p)
		} else {
			texts = append(texts, p.Text)
		}
	}
	out.text = strings.Join(texts, "\n")
	return out, nil
}

// result gives a tool's output to the turn whose call it answers.
func (d *requestDecoder) result(id string, out *toolOutput, orphan string) {
	t := d.pending[id]
	if t == nil {
		// No call waits for it. Its call may still come: it is kept for it. A
		// second one for the same id answers nothing.
		if d.early[id] != nil {
			d.drop(orphan)
			return
		}
		d.early[id] = out
		return
	}
	delete(d.pending, id)
	t.outputs[id] = out
	if t == d.turns[len(d.turns)-1] {
		t.closed = true
	}
}

// content reads the content of a message: a string or a list of parts. role
// is "" for a system or developer message and "tool" for a tool's output. An
// empty string is no part there and in an assistant message.
func (d *requestDecoder) content(raw []byte, role ir.Role, at string) ([]ir.Part, error) {
	if s, ok := asString(raw); ok {
		if s == "" && role != ir.User {
			return nil, nil
		}
		return []ir.Part{{Kind: ir.Text, Text: s}}, nil
	}
	items, err := list(raw, ir.MaxParts, at, "is neither a string nor a list of parts")
	if err != nil {
		return nil, err
	}
	parts := make([]ir.Part, 0, len(items))
	for i, item := range items {
		here := fmt.Sprintf("%s[%d]", at, i)
		o, ok := asObject(item)
		if !ok {
			return nil, badRequest(here, "is not an object")
		}
		p, err := d.part(o, role, here)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return parts, nil
}

func (d *requestDecoder) part(o object, role ir.Role, at string) (ir.Part, error) {
	kind, _ := asString(o["type"])
	delete(o, "type")
	var p ir.Part
	switch kind {
	case "text", "refusal":
		raw, ok := o.take(kind)
		if !ok {
			return ir.Part{}, badRequest(at+"."+kind, "is required")
		}
		s, ok := asString(raw)
		if !ok {
			return ir.Part{}, badRequest(at+"."+kind, "is not a string")
		}
		p = ir.Part{Kind: ir.Text, Text: s}
	case "image_url":
		if role != ir.User && role != "tool" {
			return ir.Part{}, badRequest(at, "an image cannot stand in a message of this role")
		}
		img, ok := asObject(o["image_url"])
		delete(o, "image_url")
		if !ok {
			return ir.Part{}, badRequest(at+".image_url", "is required")
		}
		url, _ := asString(img["url"])
		delete(img, "url")
		if url == "" {
			return ir.Part{}, badRequest(at+".image_url.url", "is required")
		}
		p = ir.Part{Kind: ir.Image, Data: url}
		if rest, ok := strings.CutPrefix(url, "data:"); ok {
			meta, data, found := strings.Cut(rest, ",")
			mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
			if !found || !isBase64 || mediaType == "" || data == "" {
				return ir.Part{}, badRequest(at+".image_url.url", "is a data URL that is not base64 with a media type")
			}
			p.MediaType, p.Data = mediaType, data
		}
		if raw, ok := img.take("detail"); ok {
			if s, isString := asString(raw); !isString || s != "auto" {
				d.drop(droppedImageDetail)
			}
		}
		d.unknown("content.image_url.", img)
	case "input_audio":
		return ir.Part{}, badRequest(at, "audio cannot be translated")
	case "file":
		return ir.Part{}, badRequest(at, "a file cannot be translated")
	default:
		return ir.Part{}, badRequest(at+".type", "a part of this type cannot be translated")
	}
	d.cacheControl(o)
	d.unknown("content.", o)
	return p, nil
}

// conversation writes the turns as messages. After an assistant turn with
// calls stands a user message that begins with a tool result for every call,
// in the order of the calls, followed by the images those tools returned
// (see ir.ToolImageNote) and by what the user said next, when the next turn
// is the user's.
func (d *requestDecoder) conversation() error {
	var results []ir.Part // of the assistant turn just written; nil when it made no calls
	flush := func() {
		if results != nil {
			d.req.Messages = append(d.req.Messages, ir.Message{Role: ir.User, Parts: results})
			results = nil
		}
	}
	for _, t := range d.turns {
		if t.msg.Role == ir.User {
			if results != nil {
				if len(results)+len(t.msg.Parts) > ir.MaxParts {
					return tooManyElements("messages", ir.MaxParts)
				}
				results = append(results, t.msg.Parts...)
				flush()
				continue
			}
			d.req.Messages = append(d.req.Messages, t.msg)
			continue
		}
		flush()
		d.req.Messages = append(d.req.Messages, t.msg)
		if len(t.calls) == 0 {
			continue
		}
		results = make([]ir.Part, 0, len(t.calls))
		var images []ir.Part
		for _, id := range t.calls {
			out := t.outputs[id]
			if out == nil {
				d.drop(droppedUnanswered)
				out = &toolOutput{text: ir.ToolNoOutput}
			}
			if len(out.images) > 0 {
				if out.text == "" {
					out.text = ir.ToolImageText
				}
				images = append(append(images, ir.Part{Kind: ir.Text, Text: ir.ToolImageNote(id)}), out.images...)
			}
			results = append(results, ir.Part{Kind: ir.ToolResult, ToolID: id, Text: out.text})
		}
		if results = append(results, images...); len(results) > ir.MaxParts {
			return tooManyElements("messages", ir.MaxParts)
		}
	}
	flush()
	if len(d.early) > 0 {
		d.drop(droppedOrphanTool) // results whose call never came
	}
	d.turns, d.pending, d.early, d.legacy = nil, nil, nil, nil
	if len(d.req.Messages) == 0 {
		return badRequest("messages", "holds nothing that can be translated")
	}
	if len(d.req.Messages) > ir.MaxMessages {
		return tooManyElements("messages", ir.MaxMessages)
	}
	return nil
}

func (d *requestDecoder) tools(raw []byte) ([]ir.Tool, error) {
	items, err := list(raw, ir.MaxTools, "tools", "is not a list")
	if err != nil {
		return nil, err
	}
	var out []ir.Tool
	for i, item := range items {
		at := fmt.Sprintf("tools[%d]", i)
		o, ok := asObject(item)
		if !ok {
			return nil, badRequest(at, "is not an object")
		}
		kind := "function"
		if raw, ok := o.take("type"); ok {
			if kind, ok = asString(raw); !ok {
				return nil, badRequest(at+".type", "is not a string")
			}
		} else if _, isFunction := o["function"]; !isFunction {
			return nil, badRequest(at+".type", "is required")
		}
		if kind != "function" {
			// A tool the provider would have to run, or one without a JSON
			// Schema (custom): not emulated.
			d.drop(ir.DroppedTool(kind))
			continue
		}
		fn, ok := asObject(o["function"])
		delete(o, "function")
		if !ok {
			return nil, badRequest(at+".function", "is required")
		}
		var t ir.Tool
		if t.Name, err = fn.required("name", at+".function.name"); err != nil {
			return nil, err
		}
		if t.Description, err = fn.str("description", at+".function.description"); err != nil {
			return nil, err
		}
		if schema, ok := fn.take("parameters"); ok {
			if ir.CheckObject(schema) != nil {
				return nil, badRequest(at+".function.parameters", "is not a JSON object")
			}
			t.Schema = schema
		}
		if raw, ok := fn.take("strict"); ok {
			strict, ok := asBool(raw)
			if !ok {
				return nil, badRequest(at+".function.strict", "is not true or false")
			}
			if strict {
				d.drop(droppedToolsStrict)
			}
		}
		d.cacheControl(o)
		d.unknown("tools.", fn)
		d.unknown("tools.", o)
		out = append(out, t)
	}
	return out, nil
}

func (d *requestDecoder) toolChoice(raw []byte) (ir.ToolChoice, error) {
	const reason = "is not auto, none, required or an object"
	if s, ok := asString(raw); ok {
		switch s {
		case "auto":
			return ir.ToolChoice{Mode: ir.ChoiceAuto}, nil
		case "none":
			return ir.ToolChoice{Mode: ir.ChoiceNone}, nil
		case "required":
			return ir.ToolChoice{Mode: ir.ChoiceRequired}, nil
		}
		return ir.ToolChoice{}, badRequest("tool_choice", reason)
	}
	o, ok := asObject(raw)
	if !ok {
		return ir.ToolChoice{}, badRequest("tool_choice", reason)
	}
	if kind, _ := asString(o["type"]); kind != "function" {
		// A list of allowed tools, or a tool that is not carried.
		d.drop(droppedToolChoice)
		return ir.ToolChoice{}, nil
	}
	delete(o, "type")
	fn, _ := asObject(o["function"])
	delete(o, "function")
	name, err := fn.required("name", "tool_choice.function.name")
	if err != nil {
		return ir.ToolChoice{}, err
	}
	d.unknown("tool_choice.", o)
	return ir.ToolChoice{Mode: ir.ChoiceTool, Name: name}, nil
}
