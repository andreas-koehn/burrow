// Package messages is the Anthropic Messages codec of the format
// translators. This file and its siblings hold the half a caller needs: read
// a request into the neutral form (DecodeRequest), write an answer from it,
// whole (EncodeResponse) or streamed (StreamEncoder), and write an error in
// the Messages shape (EncodeError).
//
// Nothing in this package logs, and no error it returns carries request or
// answer content: an error names the field and the reason only.
package messages

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

// Names DecodeRequest adds to the dropped list besides those of package ir
// (all fixed, none chosen by a client).
const (
	droppedMetadata          = "metadata"
	droppedServiceTier       = "service_tier"
	droppedContainer         = "container"
	droppedContextManagement = "context_management"
	droppedMCPServers        = "mcp_servers"
	droppedOutputConfig      = "output_config"
	droppedParallelToolUse   = "tool_choice.disable_parallel_tool_use"

	// What is repaired in the history of tool calls and their results.
	droppedUnansweredUse = "input:tool_use.unanswered" // a tool_use without a tool_result got one
	droppedDuplicateUse  = "input:tool_use.duplicate"  // a tool_use whose id waits already: left out
	droppedOrphanResult  = "input:tool_result.orphan"  // a tool_result for no tool_use: left out
)

// BadRequestError is a client error: the request is not a Messages request,
// or holds something that cannot be translated and must not be left out.
// It is ir.BadRequestError with the format "messages".
type BadRequestError = ir.BadRequestError

func bad(field, reason string) error {
	return &BadRequestError{Format: "messages", Field: field, Reason: reason}
}

func tooMany(field string, limit int) error {
	return &BadRequestError{Format: "messages", Field: field, Reason: fmt.Sprintf("has more than %d elements", limit), Limit: true}
}

// DecodeRequest reads an Anthropic Messages request into the neutral form.
// Every field is carried, refused, or named in Request.Dropped.
//
// Carried: "model", "max_tokens" (required, as Anthropic requires it),
// "system" (a string or text blocks), "messages" (content as a string or as
// blocks: text, image with a base64 or url source, tool_use, tool_result
// with a string, or text and image blocks, and "is_error", thinking), "tools" (those
// without a "type" or of type "custom", the schema's bytes unchanged),
// "tool_choice" (auto, any, tool, none), "temperature", "top_p",
// "stop_sequences", "stream".
//
// Left out and reported:
//   - "cache_control" anywhere: ir.DroppedCacheControl;
//   - a thinking block's signature: ir.DroppedThinkingSignature (the text is
//     carried; the target's encoder decides about it); a redacted_thinking
//     block and the "thinking" setting unless it is switched off:
//     ir.DroppedThinking;
//   - "top_k": ir.DroppedTopK; "metadata", "service_tier", "container",
//     "context_management", "mcp_servers", "output_config": by their name;
//   - "tool_choice.disable_parallel_tool_use" when it is true;
//   - a tool of any other type (web search, bash, text editor, computer use,
//     code execution…): ir.DroppedTool(type);
//   - a history block of a tool the provider ran itself (server_tool_use,
//     mcp_tool_use, mcp_tool_result and every other …_tool_result):
//     ir.DroppedInput(type);
//   - any other key: ir.Unknown, at the four depths package ir describes,
//     and "tool_choice.<key>".
//
// A turn all of whose blocks were left out is left out as a whole.
//
// An image inside a tool_result (Claude Code's Read tool returns one) is
// carried, but not there: a tool result of the neutral form holds text. The
// result keeps its text (ir.ToolImageText when it has none), and the images
// of all tool results of the message stand after its last tool result, each
// tool's after a text part ir.ToolImageNote(id). Nothing is lost, so nothing
// is reported.
//
// Images are bounded by nothing but the gateway's limit on the request
// body: there is no separate limit on the size of a base64 payload.
//
// # Pairing
//
// A Chat Completions server wants an assistant message with tool calls to
// be followed by one tool message for each call, and by nothing else first;
// it answers 400 to anything else, and a client that sends its history with
// every request (Claude Code) would get that 400 for ever. So the
// conversation is made one the target takes, and what was repaired is
// reported:
//   - a tool_result belongs to the tool_use with its id, wherever it stands
//     (in a later user message, or before its call): the results of a turn
//     stand first in the user message that follows it, in the order of the
//     calls, and such a message is made when there is none;
//   - a tool_use without a tool_result — in the middle of the conversation
//     or at its end — gets the result ir.ToolNoOutput:
//     "input:tool_use.unanswered";
//   - a tool_result whose tool_use never comes, and a second one for one
//     call, are left out: "input:tool_result.orphan"; a user message that
//     held nothing else is left out with it;
//   - a tool_use whose id waits for its result already is left out:
//     "input:tool_use.duplicate";
//   - two assistant messages with nothing between them are one turn.
//
// Refused with a *BadRequestError: a request in which no turn is left; a body that is not a JSON object; a
// missing or mistyped required field; a role other than user and assistant;
// a block in a turn that cannot hold it; a tool_use without id or name or
// with an input that is not a JSON object; an image that cannot be carried
// (with a source that is neither base64 nor url); a
// block of any other type (document, search_result, …), because content
// must not vanish; anything over the ir limits. The request returned with
// an error is empty.
func DecodeRequest(body []byte) (ir.Request, error) {
	var d decoder
	req, err := d.request(body)
	if err != nil {
		return ir.Request{}, err
	}
	req.Dropped = d.dropped
	return req, nil
}

type decoder struct {
	dropped []string
	// images holds what the tool results of the message being read returned
	// as images, each tool's after a text that names its call: see content.
	images []ir.Part
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

// cacheControl reports a cache_control key.
func (d *decoder) cacheControl(o object) {
	if _, ok := o.take("cache_control"); ok {
		d.drop(ir.DroppedCacheControl)
	}
}

func (d *decoder) request(body []byte) (ir.Request, error) {
	if ir.Depth(body) > ir.MaxDepth {
		return ir.Request{}, &BadRequestError{Format: "messages", Field: "body", Reason: fmt.Sprintf("is nested deeper than %d", ir.MaxDepth), Limit: true}
	}
	top, ok := asObject(body)
	if !ok {
		return ir.Request{}, bad("body", "is not a JSON object")
	}
	var (
		req ir.Request
		err error
	)
	if req.Model, err = top.string("model", "model"); err != nil {
		return req, err
	}
	raw, ok := top.take("max_tokens")
	if !ok {
		return req, bad("max_tokens", "is required")
	}
	if n, ok := asNumber(raw); !ok || n < 1 || n > math.MaxInt32 || n != math.Trunc(n) {
		return req, bad("max_tokens", "is not a whole number above 0")
	} else {
		req.MaxTokens = int(n)
	}
	raw, ok = top.take("messages")
	if !ok {
		return req, bad("messages", "is required")
	}
	if req.Messages, err = d.messages(raw); err != nil {
		return req, err
	}
	if req.Messages, err = d.pair(req.Messages); err != nil {
		return req, err
	}
	if raw, ok := top.take("system"); ok {
		if req.System, err = d.system(raw); err != nil {
			return req, err
		}
	}
	if raw, ok := top.take("tools"); ok {
		if req.Tools, err = d.tools(raw); err != nil {
			return req, err
		}
	}
	if raw, ok := top.take("tool_choice"); ok {
		if req.ToolChoice, err = d.toolChoice(raw); err != nil {
			return req, err
		}
	}
	if req.Temperature, err = top.number("temperature"); err != nil {
		return req, err
	}
	if req.TopP, err = top.number("top_p"); err != nil {
		return req, err
	}
	if raw, ok := top.take("stop_sequences"); ok {
		items, err := list(raw, ir.MaxParts, "stop_sequences")
		if err != nil {
			return req, err
		}
		for i, item := range items {
			s, ok := asString(item)
			if !ok {
				return req, bad(fmt.Sprintf("stop_sequences[%d]", i), "is not a string")
			}
			req.Stop = append(req.Stop, s)
		}
	}
	if raw, ok := top.take("stream"); ok {
		if req.Stream, ok = asBool(raw); !ok {
			return req, bad("stream", "is not true or false")
		}
	}
	if raw, ok := top.take("thinking"); ok {
		// Thinking that is switched off loses nothing on the way.
		setting, _ := asObject(raw)
		if kind, _ := asString(setting["type"]); kind != "disabled" {
			d.drop(ir.DroppedThinking)
		}
	}
	for _, name := range []string{ir.DroppedTopK, ir.DroppedCacheControl, droppedMetadata, droppedServiceTier,
		droppedContainer, droppedContextManagement, droppedMCPServers, droppedOutputConfig} {
		if _, ok := top.take(name); ok {
			d.drop(name)
		}
	}
	d.unknown("", top)
	return req, nil
}

// callTurn is an assistant message with tool calls while they are paired
// with their results.
type callTurn struct {
	calls   []string           // the ids, in order
	results map[string]ir.Part // by id
}

// pair makes the conversation one in which every tool call has its result
// where a Chat Completions server wants it — see Pairing at DecodeRequest.
func (d *decoder) pair(in []ir.Message) ([]ir.Message, error) {
	var (
		turns      = make([]*callTurn, len(in)) // per assistant message with calls
		hadResults = make([]bool, len(in))      // per user message
		pending    = map[string]*callTurn{}     // the calls that wait for their result
		early      = map[string]ir.Part{}       // the results that wait for their call
	)
	// What belongs together, by id.
	for i := range in {
		m := &in[i]
		kept := m.Parts[:0]
		if m.Role == ir.Assistant {
			t := &callTurn{results: map[string]ir.Part{}}
			seen := map[string]bool{}
			for _, p := range m.Parts {
				if p.Kind != ir.ToolUse {
					kept = append(kept, p)
					continue
				}
				if seen[p.ToolID] || pending[p.ToolID] != nil {
					// One result cannot answer two calls: the first stays.
					d.drop(droppedDuplicateUse)
					continue
				}
				seen[p.ToolID] = true
				kept = append(kept, p)
				t.calls = append(t.calls, p.ToolID)
				if r, ok := early[p.ToolID]; ok {
					t.results[p.ToolID] = r
					delete(early, p.ToolID)
				} else {
					pending[p.ToolID] = t
				}
			}
			if len(t.calls) > 0 {
				turns[i] = t
			}
		} else {
			for _, p := range m.Parts {
				if p.Kind != ir.ToolResult {
					kept = append(kept, p)
					continue
				}
				hadResults[i] = true
				if t := pending[p.ToolID]; t != nil {
					t.results[p.ToolID] = p
					delete(pending, p.ToolID)
				} else if _, twice := early[p.ToolID]; twice {
					d.drop(droppedOrphanResult)
				} else {
					early[p.ToolID] = p
				}
			}
		}
		m.Parts = kept
	}
	if len(early) > 0 {
		d.drop(droppedOrphanResult) // results whose call never came
	}
	// The conversation: after a turn with calls, their results.
	out := make([]ir.Message, 0, len(in)+1)
	var results []ir.Part // of the assistant turn just written; nil when it made no calls
	for i, m := range in {
		if m.Role != ir.Assistant {
			switch {
			case results != nil:
				if len(results)+len(m.Parts) > ir.MaxParts {
					return nil, tooMany(fmt.Sprintf("messages[%d].content", i), ir.MaxParts)
				}
				m.Parts, results = append(results, m.Parts...), nil
			case hadResults[i] && len(m.Parts) == 0:
				continue // its results went to their calls, or answered none
			}
			out = append(out, m)
			continue
		}
		if results != nil {
			out, results = append(out, ir.Message{Role: ir.User, Parts: results}), nil
		}
		if n := len(out); n > 0 && out[n-1].Role == ir.Assistant {
			// Two assistant messages with nothing between them (any more)
			// are one turn.
			if len(out[n-1].Parts)+len(m.Parts) > ir.MaxParts {
				return nil, tooMany(fmt.Sprintf("messages[%d].content", i), ir.MaxParts)
			}
			out[n-1].Parts = append(out[n-1].Parts[:len(out[n-1].Parts):len(out[n-1].Parts)], m.Parts...)
		} else {
			out = append(out, m)
		}
		if t := turns[i]; t != nil {
			results = make([]ir.Part, 0, len(t.calls))
			for _, id := range t.calls {
				r, ok := t.results[id]
				if !ok {
					d.drop(droppedUnansweredUse)
					r = ir.Part{Kind: ir.ToolResult, ToolID: id, Text: ir.ToolNoOutput}
				}
				results = append(results, r)
			}
		}
	}
	if results != nil {
		out = append(out, ir.Message{Role: ir.User, Parts: results})
	}
	switch {
	case len(out) == 0:
		return nil, bad("messages", "holds nothing that can be translated")
	case len(out) > ir.MaxMessages:
		return nil, tooMany("messages", ir.MaxMessages)
	}
	return out, nil
}

func (d *decoder) system(raw []byte) ([]ir.Part, error) {
	if s, ok := asString(raw); ok {
		if s == "" {
			return nil, nil
		}
		return []ir.Part{{Kind: ir.Text, Text: s}}, nil
	}
	items, err := list(raw, ir.MaxParts, "system")
	if err != nil {
		if e := err.(*BadRequestError); !e.Limit {
			e.Reason = "is neither a string nor a list of text blocks"
		}
		return nil, err
	}
	var parts []ir.Part
	for i, item := range items {
		at := fmt.Sprintf("system[%d]", i)
		o, ok := asObject(item)
		if !ok {
			return nil, bad(at, "is not an object")
		}
		if kind, _ := asString(o["type"]); kind != "text" {
			return nil, bad(at+".type", "a system block that is not text cannot be translated")
		}
		p, err := d.textBlock(o, at)
		if err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// textBlock reads a block of type "text".
func (d *decoder) textBlock(o object, at string) (ir.Part, error) {
	delete(o, "type")
	raw, ok := o.take("text")
	if !ok {
		return ir.Part{}, bad(at+".text", "is required")
	}
	s, ok := asString(raw)
	if !ok {
		return ir.Part{}, bad(at+".text", "is not a string")
	}
	d.cacheControl(o)
	d.unknown("content.", o)
	return ir.Part{Kind: ir.Text, Text: s}, nil
}

func (d *decoder) messages(raw []byte) ([]ir.Message, error) {
	items, err := list(raw, ir.MaxMessages, "messages")
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, bad("messages", "is empty")
	}
	out := make([]ir.Message, 0, len(items))
	for i, item := range items {
		at := fmt.Sprintf("messages[%d]", i)
		o, ok := asObject(item)
		if !ok {
			return nil, bad(at, "is not an object")
		}
		role, _ := asString(o["role"])
		delete(o, "role")
		if role != string(ir.User) && role != string(ir.Assistant) {
			return nil, bad(at+".role", "is neither user nor assistant")
		}
		m := ir.Message{Role: ir.Role(role)}
		content, ok := o.take("content")
		if !ok {
			return nil, bad(at+".content", "is required")
		}
		leftOut := 0
		if s, ok := asString(content); ok {
			m.Parts = []ir.Part{{Kind: ir.Text, Text: s}}
		} else if m.Parts, leftOut, err = d.content(content, m.Role, at+".content"); err != nil {
			return nil, err
		}
		d.unknown("messages.", o)
		if len(m.Parts) == 0 && leftOut > 0 {
			// Every block of the turn was left out (and reported): there is
			// no turn to send. An empty one would reach the target as a
			// message that says nothing.
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, bad("messages", "holds nothing that can be translated")
	}
	return out, nil
}

// content reads the blocks of a message. leftOut counts the blocks that
// were left out (and reported).
func (d *decoder) content(raw []byte, role ir.Role, at string) (parts []ir.Part, leftOut int, err error) {
	items, err := list(raw, ir.MaxParts, at)
	if err != nil {
		if e := err.(*BadRequestError); !e.Limit {
			e.Reason = "is neither a string nor a list of blocks"
		}
		return nil, 0, err
	}
	calls, lastResult := 0, 0
	for i, item := range items {
		here := fmt.Sprintf("%s[%d]", at, i)
		o, ok := asObject(item)
		if !ok {
			return nil, 0, bad(here, "is not an object")
		}
		p, keep, err := d.block(o, role, here)
		if err != nil {
			return nil, 0, err
		}
		if !keep {
			leftOut++
			continue
		}
		if p.Kind == ir.ToolUse {
			if calls++; calls > ir.MaxToolCalls {
				return nil, 0, &BadRequestError{Format: "messages", Field: at, Reason: fmt.Sprintf("has more than %d tool calls", ir.MaxToolCalls), Limit: true}
			}
		}
		parts = append(parts, p)
		if p.Kind == ir.ToolResult {
			lastResult = len(parts)
		}
	}
	if len(d.images) > 0 {
		// The images tools returned stand after the last tool result: a
		// tool result holds text only (see ir.ToolImageNote), and both
		// targets want the results of a turn before anything else.
		if len(parts)+len(d.images) > ir.MaxParts {
			return nil, 0, tooMany(at, ir.MaxParts)
		}
		rest := append([]ir.Part(nil), parts[lastResult:]...)
		parts = append(append(parts[:lastResult], d.images...), rest...)
		d.images = nil
	}
	return parts, leftOut, nil
}

// providerToolBlock reports whether a block type belongs to a tool the
// provider runs itself. Its declaration is left out (see tools), so its
// history is too.
func providerToolBlock(kind string) bool {
	return kind == "server_tool_use" || kind == "mcp_tool_use" ||
		(kind != "tool_result" && strings.HasSuffix(kind, "_tool_result"))
}

// The block types that are refused by name; any other unknown type is
// refused without repeating what the client wrote.
var refusedBlocks = map[string]bool{"document": true, "search_result": true, "container_upload": true}

// block reads one content block of a message. keep is false for a block
// that was left out (and reported).
func (d *decoder) block(o object, role ir.Role, at string) (p ir.Part, keep bool, err error) {
	kind, _ := asString(o["type"])
	delete(o, "type")
	wrongTurn := func() (ir.Part, bool, error) {
		return ir.Part{}, false, bad(at, "a "+kind+" block cannot stand in a turn of this role")
	}
	switch {
	case kind == "text":
		p, err = d.textBlock(o, at)
		return p, err == nil, err
	case kind == "image":
		if role != ir.User {
			return wrongTurn()
		}
		if p, err = imageBlock(o, at); err != nil {
			return ir.Part{}, false, err
		}
	case kind == "tool_use":
		if role != ir.Assistant {
			return wrongTurn()
		}
		if p, err = toolUseBlock(o, at); err != nil {
			return ir.Part{}, false, err
		}
	case kind == "tool_result":
		if role != ir.User {
			return wrongTurn()
		}
		if p, err = d.toolResultBlock(o, at); err != nil {
			return ir.Part{}, false, err
		}
	case kind == "thinking":
		if role != ir.Assistant {
			return wrongTurn()
		}
		raw, _ := o.take("thinking")
		s, ok := asString(raw)
		if !ok && raw != nil {
			return ir.Part{}, false, bad(at+".thinking", "is not a string")
		}
		if sig, _ := asString(o["signature"]); sig != "" {
			d.drop(ir.DroppedThinkingSignature)
		}
		delete(o, "signature")
		p = ir.Part{Kind: ir.Thinking, Text: s}
	case kind == "redacted_thinking":
		d.drop(ir.DroppedThinking)
		return ir.Part{}, false, nil
	case providerToolBlock(kind):
		d.drop(ir.DroppedInput(kind))
		return ir.Part{}, false, nil
	case refusedBlocks[kind]:
		return ir.Part{}, false, bad(at+".type", "a "+kind+" block cannot be translated")
	default:
		return ir.Part{}, false, bad(at+".type", "a block of this type cannot be translated")
	}
	d.cacheControl(o)
	d.unknown("content.", o)
	return p, true, nil
}

func imageBlock(o object, at string) (ir.Part, error) {
	raw, _ := o.take("source")
	src, ok := asObject(raw)
	if !ok {
		return ir.Part{}, bad(at+".source", "is required")
	}
	field := func(key string) (string, error) {
		s, _ := asString(src[key])
		if s == "" {
			return "", bad(at+".source."+key, "is required")
		}
		return s, nil
	}
	p := ir.Part{Kind: ir.Image}
	var err error
	switch kind, _ := asString(src["type"]); kind {
	case "base64":
		if p.MediaType, err = field("media_type"); err != nil {
			return ir.Part{}, err
		}
		p.Data, err = field("data")
	case "url":
		p.Data, err = field("url")
	default:
		err = bad(at+".source.type", "an image source that is neither base64 nor url cannot be translated")
	}
	if err != nil {
		return ir.Part{}, err
	}
	return p, nil
}

func toolUseBlock(o object, at string) (ir.Part, error) {
	p := ir.Part{Kind: ir.ToolUse}
	p.ToolID, _ = asString(o["id"])
	p.ToolName, _ = asString(o["name"])
	delete(o, "id")
	delete(o, "name")
	switch {
	case p.ToolID == "":
		return ir.Part{}, bad(at+".id", "is required")
	case p.ToolName == "":
		return ir.Part{}, bad(at+".name", "is required")
	}
	raw, _ := o.take("input")
	input, err := ir.ToolInput(raw)
	if err != nil {
		reason, limit := "is not a JSON object", false
		if errors.Is(err, ir.ErrLimit) {
			reason, limit = "is too large", true
		}
		return ir.Part{}, &BadRequestError{Format: "messages", Field: at + ".input", Reason: reason, Limit: limit}
	}
	p.Input = input
	return p, nil
}

func (d *decoder) toolResultBlock(o object, at string) (ir.Part, error) {
	p := ir.Part{Kind: ir.ToolResult}
	p.ToolID, _ = asString(o["tool_use_id"])
	delete(o, "tool_use_id")
	if p.ToolID == "" {
		return ir.Part{}, bad(at+".tool_use_id", "is required")
	}
	if raw, ok := o.take("is_error"); ok {
		if p.IsError, ok = asBool(raw); !ok {
			return ir.Part{}, bad(at+".is_error", "is not true or false")
		}
	}
	raw, ok := o.take("content")
	if !ok {
		return p, nil
	}
	if s, ok := asString(raw); ok {
		p.Text = s
		return p, nil
	}
	at += ".content"
	items, err := list(raw, ir.MaxParts, at)
	if err != nil {
		if e := err.(*BadRequestError); !e.Limit {
			e.Reason = "is neither a string nor a list of text blocks"
		}
		return ir.Part{}, err
	}
	texts := make([]string, 0, len(items))
	var images []ir.Part
	for i, item := range items {
		here := fmt.Sprintf("%s[%d]", at, i)
		inner, ok := asObject(item)
		if !ok {
			return ir.Part{}, bad(here, "is not an object")
		}
		switch kind, _ := asString(inner["type"]); kind {
		case "text":
			t, err := d.textBlock(inner, here)
			if err != nil {
				return ir.Part{}, err
			}
			texts = append(texts, t.Text)
		case "image":
			delete(inner, "type")
			img, err := imageBlock(inner, here)
			if err != nil {
				return ir.Part{}, err
			}
			d.cacheControl(inner)
			d.unknown("content.", inner)
			images = append(images, img)
		default:
			return ir.Part{}, bad(here, "only text and images in tool results can be translated")
		}
	}
	p.Text = strings.Join(texts, "\n")
	if len(images) > 0 {
		if p.Text == "" {
			p.Text = ir.ToolImageText
		}
		d.images = append(append(d.images, ir.Part{Kind: ir.Text, Text: ir.ToolImageNote(p.ToolID)}), images...)
	}
	return p, nil
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
		if raw, ok := o.take("type"); ok {
			kind, ok := asString(raw)
			if !ok {
				return nil, bad(at+".type", "is not a string")
			}
			if kind != "custom" {
				// A tool the provider would have to run: not emulated.
				d.drop(ir.DroppedTool(kind))
				continue
			}
		}
		var t ir.Tool
		if t.Name, err = o.string("name", at+".name"); err != nil {
			return nil, err
		}
		if t.Name == "" {
			return nil, bad(at+".name", "is required")
		}
		if t.Description, err = o.string("description", at+".description"); err != nil {
			return nil, err
		}
		if schema, ok := o.take("input_schema"); ok {
			if ir.CheckObject(schema) != nil {
				return nil, bad(at+".input_schema", "is not a JSON object")
			}
			t.Schema = schema
		}
		d.cacheControl(o)
		d.unknown("tools.", o)
		out = append(out, t)
	}
	return out, nil
}

func (d *decoder) toolChoice(raw []byte) (ir.ToolChoice, error) {
	o, ok := asObject(raw)
	if !ok {
		return ir.ToolChoice{}, bad("tool_choice", "is not an object")
	}
	var c ir.ToolChoice
	kind, _ := asString(o["type"])
	delete(o, "type")
	switch kind {
	case "auto":
		c.Mode = ir.ChoiceAuto
	case "any":
		c.Mode = ir.ChoiceRequired
	case "none":
		c.Mode = ir.ChoiceNone
	case "tool":
		c.Mode = ir.ChoiceTool
		c.Name, _ = asString(o["name"])
		delete(o, "name")
		if c.Name == "" {
			return ir.ToolChoice{}, bad("tool_choice.name", "is required")
		}
	default:
		return ir.ToolChoice{}, bad("tool_choice.type", "is not auto, any, tool or none")
	}
	if raw, ok := o.take("disable_parallel_tool_use"); ok {
		off, ok := asBool(raw)
		if !ok {
			return ir.ToolChoice{}, bad("tool_choice.disable_parallel_tool_use", "is not true or false")
		}
		if off {
			d.drop(droppedParallelToolUse)
		}
	}
	d.unknown("tool_choice.", o)
	return c, nil
}
