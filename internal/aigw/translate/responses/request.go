// Package responses is the OpenAI Responses codec of the format translators.
// This file and its siblings hold the half a caller needs: read a request
// into the neutral form (DecodeRequest), write an answer from it, whole
// (EncodeResponse) or streamed (StreamEncoder), and write an error in the
// OpenAI shape (EncodeError). CheckStream reads a stream the way a client
// does and is the yardstick of the encoder.
//
// No Responses state is kept anywhere: a request stands for itself, and only
// POST /v1/responses can be translated.
//
// Nothing in this package logs, and no error it returns carries request or
// answer content: an error names the field and the reason only.
package responses

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

// format is the Format of this package's *ir.BadRequestError.
const format = "responses"

// Names DecodeRequest adds to the dropped list besides those of package ir
// (all fixed, none chosen by a client).
const (
	droppedReasoning         = "reasoning"
	droppedInclude           = "include"
	droppedParallelToolCalls = "parallel_tool_calls"
	droppedToolChoice        = "tool_choice"
	droppedToolsStrict       = "tools.strict"
	droppedTextFormat        = "text.format"
	droppedTextVerbosity     = "text.verbosity"
	droppedTruncation        = "truncation"
	droppedServiceTier       = "service_tier"
	droppedBackground        = "background"
	droppedImageDetail       = "input_image.detail"
	droppedAnnotations       = "content.annotations"
	droppedLogprobs          = "content.logprobs"
	droppedPhase             = "input.phase"     // an assistant message's "phase"
	droppedNamespace         = "input.namespace" // a function_call's "namespace"

	// What is repaired in the history of calls and outputs.
	droppedCallArguments = "input:function_call.arguments"     // arguments that are no JSON object: the call has none
	droppedUnanswered    = "input:function_call.unanswered"    // a call without an output got one
	droppedDuplicateCall = "input:function_call.duplicate"     // a call whose id waits already: left out
	droppedOrphanOutput  = "input:function_call_output.orphan" // an output for no waiting call: left out
)

// droppedWhenAsked are the top-level fields that are left out and reported
// by their own name whenever they ask for something (see asked).
var droppedWhenAsked = []string{
	ir.DroppedPreviousResponseID, ir.DroppedStore, droppedReasoning, droppedInclude, droppedBackground,
	"metadata", "user", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "conversation",
	"prompt", "max_tool_calls", "top_logprobs", "stream_options", "context_management", "client_metadata",
}

func bad(field, reason string) error {
	return &ir.BadRequestError{Format: format, Field: field, Reason: reason}
}

func tooMany(field string, limit int) error {
	return &ir.BadRequestError{Format: format, Field: field, Reason: fmt.Sprintf("has more than %d elements", limit), Limit: true}
}

// DecodeRequest reads an OpenAI Responses request into the neutral form.
// Every field is carried, refused, or named in Request.Dropped.
//
// Carried: "model"; "instructions" (the first part of the system prompt);
// "input" as a string (one user message) or as a list of items; "tools" of
// type "function" (name, description, the schema's bytes unchanged);
// "tool_choice" ("auto", "none", "required", a named function);
// "max_output_tokens", "temperature", "top_p", "stream".
//
// Items of "input":
//   - a message (type "message", or no type and a role). Roles system and
//     developer go to the system prompt, after "instructions", in order; one
//     that stands after the first turn of the conversation loses its place
//     and is reported as ir.DroppedSystemPosition. Roles user and assistant
//     are turns. Content is a string or parts: input_text, output_text, text
//     and refusal are text; input_image with an "image_url" is an image (a
//     base64 data URL is split into media type and data, any other URL is
//     kept as a URL), in a user turn only;
//   - function_call: a tool call ("call_id", "name", "arguments" as the
//     text of one JSON object, kept byte for byte);
//   - function_call_output: a tool result ("call_id", "output" as a string
//     or as parts: text, joined by "\n", and input_image);
//   - every other item type (reasoning, item_reference, web_search_call,
//     local_shell_call, custom_tool_call and their outputs, …) is left out:
//     ir.DroppedInput(type).
//
// # Calls and their outputs
//
// A Chat Completions server wants an assistant message with tool calls to
// be followed by one tool message for each call, and by nothing else
// first. A Responses history is looser — the items of one answer are
// message, call, message in any order, a client adds the outputs when all
// of them are there, and what a client recorded of a stream that was cut is
// not always whole — and Codex sends its history again with every request,
// so a request that is refused for its history ends the session. The
// decoder therefore builds a conversation the target takes, and reports
// what it had to repair:
//   - the assistant's items — messages and calls — that follow each other
//     are ONE assistant turn, up to the first output that answers one of its
//     calls or the next user message;
//   - an output belongs to the call with its "call_id", wherever it stands
//     — after a later message, or before its call: the results of a turn
//     follow it directly, in the order of the calls;
//   - the images a tool returned follow the results of that turn in the
//     same user message, each tool's after a text that names its call
//     (ir.ToolImageNote); the result itself keeps its text, or says
//     ir.ToolImageText. Nothing is lost, so nothing is reported;
//   - a call without an output gets the result ir.ToolNoOutput:
//     "input:function_call.unanswered";
//   - an output whose call never comes, and a second output for one call,
//     are left out: "input:function_call_output.orphan";
//   - a call whose id waits for its output already is left out:
//     "input:function_call.duplicate";
//   - "arguments" that are not the text of one JSON object become {}:
//     "input:function_call.arguments".
//
// Left out and reported:
//   - "previous_response_id", "conversation", "store": true (no Responses
//     state is kept), "background": true (the answer comes at once),
//     "reasoning", "include", "metadata", "user", "prompt_cache_key",
//     "prompt_cache_retention", "safety_identifier", "prompt",
//     "max_tool_calls", "top_logprobs", "stream_options",
//     "context_management": by their name, when they ask for something — a
//     null, a false, an empty string, list or object asks for nothing and is
//     not reported;
//   - "truncation" other than "disabled" and "service_tier" other than
//     "auto" (both are the defaults): by their name;
//   - "parallel_tool_calls": false: "parallel_tool_calls";
//   - "text.format" other than {"type":"text"}: "text.format" (structured
//     output is not carried); "text.verbosity": "text.verbosity";
//   - a tool of any other type (web_search, local_shell, custom — a freeform
//     tool has no JSON Schema —, file_search, mcp, …): ir.DroppedTool(type);
//     "strict": true on a function tool: "tools.strict" (the neutral form
//     has no such flag; false and absent ask for nothing);
//   - a "tool_choice" object that is not a named function (allowed_tools, a
//     hosted or custom tool): "tool_choice";
//   - an image's "detail" other than "auto": "input_image.detail"; an
//     output_text part's non-empty "annotations" and "logprobs":
//     "content.annotations", "content.logprobs";
//   - any other key: ir.Unknown — at the top level, in an item
//     ("input.<key>"), a content part ("content.<key>"), a tool
//     ("tools.<key>"), and "text.<key>", "tool_choice.<key>";
//   - an assistant message's "phase" and a call's "namespace":
//     "input.phase", "input.namespace"; "client_metadata".
//
// Images are bounded by nothing but the gateway's limit on the request
// body: there is no separate limit on the size of a base64 payload.
//
// Read and neither carried nor reported: an item's "id" and "status". They
// name the item in the response it came from; without stored responses they
// say nothing, and a model never saw them.
//
// Refused with a *ir.BadRequestError that names the item ("input[3]"): a
// body that is not a JSON object; a missing "input"; a mistyped field; an
// item without a type and without a role; a role nobody knows; a content
// part that cannot be carried and must not vanish (input_file, input_audio,
// an image given by "file_id", an image in an assistant, system or
// developer message, any unknown part type); a function_call without
// "call_id" or "name"; a function_call_output without "call_id"; an input
// in which no turn is left; anything over the ir limits. The request
// returned with an error is empty.
func DecodeRequest(body []byte) (ir.Request, error) {
	d := decoder{pending: map[string]*turn{}, early: map[string]*toolOutput{}}
	if err := d.request(body); err != nil {
		return ir.Request{}, err
	}
	d.req.Dropped = d.dropped
	return d.req, nil
}

type decoder struct {
	req     ir.Request
	dropped []string

	system  []ir.Part              // from system and developer messages
	turns   []*turn                // the conversation so far
	pending map[string]*turn       // the calls that wait for their output: call id → the turn that made it
	early   map[string]*toolOutput // the outputs that wait for their call
}

func (d *decoder) drop(name string) { d.dropped = append(d.dropped, name) }

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

func isNull(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
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

// string reads an optional string field.
func (o object) string(key, field string) (string, error) {
	raw, ok := o.take(key)
	if !ok {
		return "", nil
	}
	s, ok := asString(raw)
	if !ok {
		return "", bad(field, "is not a string")
	}
	return s, nil
}

// required reads a string field that must be there and not empty.
func (o object) required(key, field string) (string, error) {
	s, err := o.string(key, field)
	if err == nil && s == "" {
		err = bad(field, "is required")
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
		return nil, bad(key, "is not a number")
	}
	return &f, nil
}

// list splits a JSON list into its elements, one at a time: a list over the
// limit is refused before it is held in memory.
func list(raw []byte, limit int, field string) ([]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, bad(field, "is not a list")
	}
	var out []json.RawMessage
	for dec.More() {
		if len(out) >= limit {
			return nil, tooMany(field, limit)
		}
		var el json.RawMessage
		if err := dec.Decode(&el); err != nil {
			return nil, bad(field, "is not a list")
		}
		out = append(out, el)
	}
	return out, nil
}

// stringOrList is list for a field that may be a string as well: the reason
// of a plain type error says so.
func stringOrList(raw []byte, limit int, field, what string) ([]json.RawMessage, error) {
	items, err := list(raw, limit, field)
	var e *ir.BadRequestError
	if errors.As(err, &e) && !e.Limit {
		e.Reason = "is neither a string nor a list of " + what
	}
	return items, err
}

// unknown reports the keys that are left in o, each with the prefix of the
// place it stands at.
func (d *decoder) unknown(where string, o object) {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.drop(ir.Unknown(where + k))
	}
}

func (d *decoder) request(body []byte) error {
	if ir.Depth(body) > ir.MaxDepth {
		return &ir.BadRequestError{Format: format, Field: "body", Reason: fmt.Sprintf("is nested deeper than %d", ir.MaxDepth), Limit: true}
	}
	top, ok := asObject(body)
	if !ok {
		return bad("body", "is not a JSON object")
	}
	req := &d.req
	var err error
	if req.Model, err = top.string("model", "model"); err != nil {
		return err
	}
	instructions, err := top.string("instructions", "instructions")
	if err != nil {
		return err
	}
	raw, ok := top.take("input")
	if !ok {
		return bad("input", "is required")
	}
	if err := d.input(raw); err != nil {
		return err
	}
	if instructions != "" {
		req.System = append(req.System, ir.Part{Kind: ir.Text, Text: instructions})
	}
	req.System = append(req.System, d.system...)
	if len(req.System) > ir.MaxParts {
		return tooMany("input", ir.MaxParts)
	}
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
	if raw, ok := top.take("max_output_tokens"); ok {
		n, ok := asNumber(raw)
		if !ok || n < 1 || n > math.MaxInt32 || n != math.Trunc(n) {
			return bad("max_output_tokens", "is not a whole number above 0")
		}
		req.MaxTokens = int(n)
	}
	if req.Temperature, err = top.number("temperature"); err != nil {
		return err
	}
	if req.TopP, err = top.number("top_p"); err != nil {
		return err
	}
	if raw, ok := top.take("stream"); ok {
		if req.Stream, ok = asBool(raw); !ok {
			return bad("stream", "is not true or false")
		}
	}
	if raw, ok := top.take("parallel_tool_calls"); ok {
		parallel, ok := asBool(raw)
		if !ok {
			return bad("parallel_tool_calls", "is not true or false")
		}
		if !parallel {
			d.drop(droppedParallelToolCalls)
		}
	}
	if raw, ok := top.take("text"); ok {
		if err := d.text(raw); err != nil {
			return err
		}
	}
	// The two settings whose default says "nothing special".
	for name, standard := range map[string]string{droppedTruncation: "disabled", droppedServiceTier: "auto"} {
		if raw, ok := top.take(name); ok {
			if s, isString := asString(raw); asked(raw) && !(isString && s == standard) {
				d.drop(name)
			}
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

// text reads the "text" setting: nothing of it is carried.
func (d *decoder) text(raw []byte) error {
	o, ok := asObject(raw)
	if !ok {
		return bad("text", "is not an object")
	}
	if raw, ok := o.take("format"); ok {
		f, _ := asObject(raw)
		if kind, _ := asString(f["type"]); kind != "text" {
			d.drop(droppedTextFormat)
		}
	}
	if raw, ok := o.take("verbosity"); ok && asked(raw) {
		d.drop(droppedTextVerbosity)
	}
	d.unknown("text.", o)
	return nil
}

// turn is one message of the conversation while the input is read: a user
// message, or an assistant turn — the assistant's text and calls, and the
// outputs that answer those calls, wherever in the input they stood.
type turn struct {
	msg     ir.Message
	calls   []string               // an assistant turn: its call ids, in order
	outputs map[string]*toolOutput // by call id
	closed  bool                   // an output of its own came: what the assistant says next is a new turn
}

// toolOutput is what a function_call_output item holds.
type toolOutput struct {
	text   string
	images []ir.Part
}

// input reads "input": a string, or the list of items.
func (d *decoder) input(raw []byte) error {
	if s, ok := asString(raw); ok {
		d.req.Messages = []ir.Message{{Role: ir.User, Parts: []ir.Part{{Kind: ir.Text, Text: s}}}}
		return nil
	}
	items, err := stringOrList(raw, ir.MaxMessages, "input", "items")
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return bad("input", "is empty")
	}
	for i, item := range items {
		at := fmt.Sprintf("input[%d]", i)
		o, ok := asObject(item)
		if !ok {
			return bad(at, "is not an object")
		}
		kind := ""
		if raw, ok := o.take("type"); ok {
			if kind, ok = asString(raw); !ok {
				return bad(at+".type", "is not a string")
			}
		}
		_, hasRole := o["role"]
		switch {
		case kind == "message" || (kind == "" && hasRole):
			err = d.message(o, at)
		case kind == "":
			err = bad(at, "has neither a type nor a role")
		case kind == "function_call":
			err = d.call(o, at)
		case kind == "function_call_output":
			err = d.output(o, at)
		default:
			// Reasoning, a reference to a stored item, or the history of a
			// tool that is not emulated (its declaration is left out too).
			d.drop(ir.DroppedInput(kind))
		}
		if err != nil {
			return err
		}
	}
	return d.conversation()
}

// conversation writes the turns as messages. After an assistant turn with
// calls stands ONE user message: a tool result for every call, in the order
// of the calls, and after them the images those tools returned (see
// ir.ToolImageNote). A call the input has no output for gets the result
// ir.ToolNoOutput, so that the conversation is one a Chat Completions
// server takes.
func (d *decoder) conversation() error {
	for _, t := range d.turns {
		d.req.Messages = append(d.req.Messages, t.msg)
		if len(t.calls) == 0 {
			continue
		}
		results := ir.Message{Role: ir.User}
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
				images = append(images, ir.Part{Kind: ir.Text, Text: ir.ToolImageNote(id)})
				images = append(images, out.images...)
			}
			results.Parts = append(results.Parts, ir.Part{Kind: ir.ToolResult, ToolID: id, Text: out.text})
		}
		if results.Parts = append(results.Parts, images...); len(results.Parts) > ir.MaxParts {
			return tooMany("input", ir.MaxParts)
		}
		d.req.Messages = append(d.req.Messages, results)
	}
	if len(d.early) > 0 {
		// Outputs whose call never came: a tool message without its call is
		// refused by the target.
		d.drop(droppedOrphanOutput)
	}
	d.turns, d.pending, d.early = nil, nil, nil
	if len(d.req.Messages) == 0 {
		return bad("input", "holds nothing that can be translated")
	}
	if len(d.req.Messages) > ir.MaxMessages {
		return tooMany("input", ir.MaxMessages)
	}
	return nil
}

// assistant returns the assistant turn that what the assistant says or
// calls next belongs to: the newest turn while it is the assistant's and
// none of its calls has been answered, else a new one. So the items of one
// answer — message, call, message, call, in any order — are one assistant
// message, as a Chat Completions server wants them.
func (d *decoder) assistant() *turn {
	if n := len(d.turns); n > 0 && d.turns[n-1].msg.Role == ir.Assistant && !d.turns[n-1].closed {
		return d.turns[n-1]
	}
	t := &turn{msg: ir.Message{Role: ir.Assistant}}
	d.turns = append(d.turns, t)
	return t
}

func (d *decoder) message(o object, at string) error {
	role, _ := asString(o["role"])
	delete(o, "role")
	delete(o, "id")
	delete(o, "status")
	content, hasContent := o.take("content")
	switch role {
	case "system", "developer":
		if hasContent {
			parts, err := d.content(content, "", at+".content")
			if err != nil {
				return err
			}
			if len(parts) > 0 && len(d.turns) > 0 {
				d.drop(ir.DroppedSystemPosition)
			}
			if len(d.system)+len(parts) > ir.MaxParts {
				return tooMany("input", ir.MaxParts)
			}
			d.system = append(d.system, parts...)
		}
	case string(ir.User), string(ir.Assistant):
		if !hasContent {
			return bad(at+".content", "is required")
		}
		parts, err := d.content(content, ir.Role(role), at+".content")
		if err != nil {
			return err
		}
		if role == string(ir.User) {
			d.turns = append(d.turns, &turn{msg: ir.Message{Role: ir.User, Parts: parts}})
			break
		}
		t := d.assistant()
		if len(t.msg.Parts)+len(parts) > ir.MaxParts {
			return tooMany(at+".content", ir.MaxParts)
		}
		t.msg.Parts = append(t.msg.Parts, parts...)
		if raw, ok := o.take("phase"); ok && asked(raw) {
			d.drop(droppedPhase)
		}
	default:
		return bad(at+".role", "is not user, assistant, system or developer")
	}
	d.unknown("input.", o)
	return nil
}

// content reads the content of a message: a string or a list of parts. role
// is "" for a system or developer message, which holds text only. An empty
// string in a system message is no part.
func (d *decoder) content(raw []byte, role ir.Role, at string) ([]ir.Part, error) {
	if s, ok := asString(raw); ok {
		if s == "" && role == "" {
			return nil, nil
		}
		return []ir.Part{{Kind: ir.Text, Text: s}}, nil
	}
	items, err := stringOrList(raw, ir.MaxParts, at, "parts")
	if err != nil {
		return nil, err
	}
	parts := make([]ir.Part, 0, len(items))
	for i, item := range items {
		here := fmt.Sprintf("%s[%d]", at, i)
		o, ok := asObject(item)
		if !ok {
			return nil, bad(here, "is not an object")
		}
		p, err := d.part(o, role == ir.User, here)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// part reads one content part. images says whether an image may stand
// where the part stands: in a user message and in a tool's output.
func (d *decoder) part(o object, images bool, at string) (ir.Part, error) {
	kind, _ := asString(o["type"])
	delete(o, "type")
	var p ir.Part
	switch kind {
	case "input_text", "output_text", "text", "refusal":
		key := "text"
		if kind == "refusal" {
			key = "refusal"
		}
		raw, ok := o.take(key)
		if !ok {
			return ir.Part{}, bad(at+"."+key, "is required")
		}
		s, ok := asString(raw)
		if !ok {
			return ir.Part{}, bad(at+"."+key, "is not a string")
		}
		// What an answer's text part carries besides its text.
		for key, name := range map[string]string{"annotations": droppedAnnotations, "logprobs": droppedLogprobs} {
			if raw, ok := o.take(key); ok && asked(raw) {
				d.drop(name)
			}
		}
		p = ir.Part{Kind: ir.Text, Text: s}
	case "input_image":
		if !images {
			return ir.Part{}, bad(at, "an image cannot stand in a message of this role")
		}
		url, _ := asString(o["image_url"])
		delete(o, "image_url")
		if url == "" {
			if _, byFile := o["file_id"]; byFile {
				return ir.Part{}, bad(at+".file_id", "an image given by file_id cannot be translated")
			}
			return ir.Part{}, bad(at+".image_url", "is required")
		}
		p = ir.Part{Kind: ir.Image, Data: url}
		if rest, ok := strings.CutPrefix(url, "data:"); ok {
			meta, data, found := strings.Cut(rest, ",")
			mediaType, isBase64 := strings.CutSuffix(meta, ";base64")
			if !found || !isBase64 || mediaType == "" || data == "" {
				return ir.Part{}, bad(at+".image_url", "is a data URL that is not base64 with a media type")
			}
			p.MediaType, p.Data = mediaType, data
		}
		if raw, ok := o.take("detail"); ok {
			if s, isString := asString(raw); !isString || s != "auto" {
				d.drop(droppedImageDetail)
			}
		}
	case "input_file":
		return ir.Part{}, bad(at, "a file cannot be translated")
	case "input_audio":
		return ir.Part{}, bad(at, "audio cannot be translated")
	default:
		return ir.Part{}, bad(at+".type", "a part of this type cannot be translated")
	}
	d.unknown("content.", o)
	return p, nil
}

// call reads a function_call item: a tool call of an assistant turn.
func (d *decoder) call(o object, at string) error {
	delete(o, "id")
	delete(o, "status")
	p := ir.Part{Kind: ir.ToolUse, Input: json.RawMessage("{}")}
	var err error
	if p.ToolID, err = o.required("call_id", at+".call_id"); err != nil {
		return err
	}
	if p.ToolName, err = o.required("name", at+".name"); err != nil {
		return err
	}
	// The history is the client's record of an answer, and a client records
	// what it was sent: arguments that are not the text of one JSON object
	// (a call whose stream was cut) must not cost the session. The call
	// stays, with no arguments, and that is reported.
	if raw, ok := o.take("arguments"); ok {
		args, isString := asString(raw)
		input, err := ir.ToolInput([]byte(args))
		switch {
		case isString && err == nil:
			p.Input = input
		case isString && errors.Is(err, ir.ErrLimit):
			return &ir.BadRequestError{Format: format, Field: at + ".arguments", Reason: "is too large", Limit: true}
		default:
			d.drop(droppedCallArguments)
		}
	}
	if raw, ok := o.take("namespace"); ok && asked(raw) {
		d.drop(droppedNamespace)
	}
	d.unknown("input.", o)
	if d.pending[p.ToolID] != nil {
		// The id waits for its output already: one output cannot answer two
		// calls. The first call stays.
		d.drop(droppedDuplicateCall)
		return nil
	}
	t := d.assistant()
	if len(t.calls) >= ir.MaxToolCalls || len(t.msg.Parts) >= ir.MaxParts {
		return &ir.BadRequestError{Format: format, Field: at, Reason: fmt.Sprintf("is more than %d tool calls in one turn", ir.MaxToolCalls), Limit: true}
	}
	t.msg.Parts = append(t.msg.Parts, p)
	t.calls = append(t.calls, p.ToolID)
	if t.outputs == nil {
		t.outputs = map[string]*toolOutput{}
	}
	if out := d.early[p.ToolID]; out != nil {
		// Its output stood before it.
		t.outputs[p.ToolID] = out
		delete(d.early, p.ToolID)
		return nil
	}
	d.pending[p.ToolID] = t
	return nil
}

// output reads a function_call_output item and gives it to the turn whose
// call it answers: by "call_id", wherever the item stands.
func (d *decoder) output(o object, at string) error {
	delete(o, "id")
	delete(o, "status")
	id, err := o.required("call_id", at+".call_id")
	if err != nil {
		return err
	}
	var out toolOutput
	if raw, ok := o.take("output"); ok {
		if out, err = d.outputParts(raw, at+".output"); err != nil {
			return err
		}
	}
	d.unknown("input.", o)
	t := d.pending[id]
	if t == nil {
		// No call waits for it. Its call may still come (an output that
		// stands before its call is that call's): it is kept for it. A second
		// one for the same id answers nothing.
		if d.early[id] != nil {
			d.drop(droppedOrphanOutput)
			return nil
		}
		d.early[id] = &out
		return nil
	}
	delete(d.pending, id)
	t.outputs[id] = &out
	if t == d.turns[len(d.turns)-1] {
		t.closed = true
	}
	return nil
}

// outputParts reads a tool's output: a string, or parts — text, joined by
// "\n", and images.
func (d *decoder) outputParts(raw []byte, at string) (toolOutput, error) {
	if s, ok := asString(raw); ok {
		return toolOutput{text: s}, nil
	}
	items, err := stringOrList(raw, ir.MaxParts, at, "parts")
	if err != nil {
		return toolOutput{}, err
	}
	var out toolOutput
	texts := make([]string, 0, len(items))
	for i, item := range items {
		here := fmt.Sprintf("%s[%d]", at, i)
		o, ok := asObject(item)
		if !ok {
			return toolOutput{}, bad(here, "is not an object")
		}
		switch kind, _ := asString(o["type"]); kind {
		case "input_text", "output_text", "text", "input_image":
			p, err := d.part(o, true, here)
			if err != nil {
				return toolOutput{}, err
			}
			if p.Kind == ir.Image {
				out.images = append(out.images, p)
			} else {
				texts = append(texts, p.Text)
			}
		default:
			return toolOutput{}, bad(here, "only text and images in tool results can be translated")
		}
	}
	out.text = strings.Join(texts, "\n")
	return out, nil
}

func (d *decoder) tools(raw []byte) ([]ir.Tool, error) {
	items, err := list(raw, ir.MaxTools, "tools")
	if err != nil {
		return nil, err
	}
	var out []ir.Tool
	for i, item := range items {
		at := fmt.Sprintf("tools[%d]", i)
		o, ok := asObject(item)
		if !ok {
			return nil, bad(at, "is not an object")
		}
		raw, ok := o.take("type")
		if !ok {
			return nil, bad(at+".type", "is required")
		}
		kind, ok := asString(raw)
		if !ok {
			return nil, bad(at+".type", "is not a string")
		}
		if kind != "function" {
			// A tool the provider would have to run, or one without a JSON
			// Schema (custom): not emulated.
			d.drop(ir.DroppedTool(kind))
			continue
		}
		var t ir.Tool
		if t.Name, err = o.required("name", at+".name"); err != nil {
			return nil, err
		}
		if t.Description, err = o.string("description", at+".description"); err != nil {
			return nil, err
		}
		if schema, ok := o.take("parameters"); ok {
			if ir.CheckObject(schema) != nil {
				return nil, bad(at+".parameters", "is not a JSON object")
			}
			t.Schema = schema
		}
		if raw, ok := o.take("strict"); ok {
			strict, ok := asBool(raw)
			if !ok {
				return nil, bad(at+".strict", "is not true or false")
			}
			if strict {
				d.drop(droppedToolsStrict)
			}
		}
		d.unknown("tools.", o)
		out = append(out, t)
	}
	return out, nil
}

func (d *decoder) toolChoice(raw []byte) (ir.ToolChoice, error) {
	if s, ok := asString(raw); ok {
		switch s {
		case "auto":
			return ir.ToolChoice{Mode: ir.ChoiceAuto}, nil
		case "none":
			return ir.ToolChoice{Mode: ir.ChoiceNone}, nil
		case "required":
			return ir.ToolChoice{Mode: ir.ChoiceRequired}, nil
		}
		return ir.ToolChoice{}, bad("tool_choice", "is not auto, none, required or an object")
	}
	o, ok := asObject(raw)
	if !ok {
		return ir.ToolChoice{}, bad("tool_choice", "is not auto, none, required or an object")
	}
	if kind, _ := asString(o["type"]); kind != "function" {
		// A list of allowed tools, or a tool that is not carried.
		d.drop(droppedToolChoice)
		return ir.ToolChoice{}, nil
	}
	delete(o, "type")
	name, err := o.required("name", "tool_choice.name")
	if err != nil {
		return ir.ToolChoice{}, err
	}
	d.unknown("tool_choice.", o)
	return ir.ToolChoice{Mode: ir.ChoiceTool, Name: name}, nil
}
