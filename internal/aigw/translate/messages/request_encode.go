package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// This file and response_decode.go, stream_decode.go hold the half a target
// needs: write a request from the neutral form (EncodeRequest), read an
// answer into it, whole (DecodeResponse) or streamed (StreamDecoder).

// DefaultMaxTokens is the "max_tokens" EncodeRequest sends when the caller
// set no cap: Anthropic requires the field, and the other formats do not.
// It is generous on purpose: a coding client (Codex sends no cap) whose
// answers are cut short retries them. A caller's own value is sent
// unchanged.
const DefaultMaxTokens = 32000

// maxToolName is the longest tool name Anthropic takes; its characters are
// those of a tool call id ([a-zA-Z0-9_-]).
const maxToolName = 128

// Version is the value of the "anthropic-version" request header the bodies
// of this package are written for. The header is the gateway's to set.
const Version = "2023-06-01"

// firstTurnText is the user turn that is put before a conversation which
// begins with the assistant.
const firstTurnText = "[no message]"

// Names EncodeRequest adds to the dropped list (all fixed, none chosen by a
// client).
const (
	droppedMaxTokensDefault = "max_tokens.default"  // no cap was given: DefaultMaxTokens was sent
	droppedFirstTurn        = "messages.start"      // the conversation began with the assistant: a user turn was put before it
	droppedLastTurn         = "messages.end"        // the conversation ended with the assistant: a user turn was put after it
	droppedToolChoice       = "tool_choice"         // a forced choice: the model decides
	droppedTemperature      = "temperature"         // not sent
	droppedTopP             = "top_p"               // not sent
	droppedStopBlank        = "stop.blank"          // stop sequences of white space only
	droppedToolUseName      = "input:tool_use.name" // a call in the history under a name Anthropic refuses: written with the characters it takes
)

// ErrUnsupported is wrapped by every error of EncodeRequest, followed by the
// field that cannot be expressed ("messages[2].image.media_type"). The
// request itself is at fault, so the gateway answers 400. An error for a
// request over one of the ir limits wraps ir.ErrLimit as well.
var ErrUnsupported = errors.New("messages: cannot be expressed in Anthropic Messages")

func unsupported(field string) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, field)
}

func unsupportedBecause(field string, cause error) error {
	return fmt.Errorf("%w: %s (%w)", ErrUnsupported, field, cause)
}

func overLimit(field string, limit int) error {
	return fmt.Errorf("%w: %s (%w: more than %d)", ErrUnsupported, field, ir.ErrLimit, limit)
}

// imageTypes are the media types of a base64 image Anthropic accepts.
var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}

// EncodeRequest writes req as an Anthropic Messages body for model. It
// returns the names of what was left out or changed on the way, together
// with the names req.Dropped already holds, sorted and without duplicates
// (nil when there are none).
//
// The Messages API is stricter than the formats a caller may speak, and a
// client that sends its history with every request would meet the same 400
// for ever. So the conversation is written as Anthropic wants it:
//   - "max_tokens" is required: without a cap DefaultMaxTokens is sent and
//     "max_tokens.default" is reported;
//   - roles alternate: messages of one role that follow each other become
//     one message;
//   - the user speaks first: a conversation that begins with the assistant
//     (a greeting, in a Chat history) gets the user turn "[no message]"
//     before it: "messages.start". One that ends with the assistant would
//     be a prefill, which current models refuse: the user turn
//     ir.ContinueText is put after it: "messages.end";
//   - in a user message the tool_result blocks stand first, whatever the
//     order of the parts, and the rest follows in its order;
//   - the images a tool returned go back into its tool_result: a Text part
//     ir.ToolImageNote(id) that follows the results of its message and is
//     followed by Image parts names the result they belong to, when exactly
//     one result of the message has that note. The note itself is not sent
//     then, nor a result text that is only ir.ToolImageText. Otherwise note
//     and images stay content after the results;
//   - a text that is empty or white space is no block (Anthropic refuses
//     one), and a message without blocks is not written. A tool result may
//     be empty;
//   - Thinking parts are not sent back — without their signature the API
//     refuses them: "thinking";
//   - a tool call's id is written with the characters Anthropic takes
//     ([a-zA-Z0-9_-]); any other becomes "_". Every id stands once in the
//     whole conversation: a call whose id was used before — by a provider
//     that numbers its calls anew in every answer, or by an id that reads
//     the same once cleaned — gets a number added, and its result, in the
//     turn after it, the same. A history that began on another provider
//     keeps working; nothing is lost, so nothing is reported;
//   - a tool call in the history under a name Anthropic refuses (it cannot
//     be one of the declared tools) is written with the characters it takes,
//     cut to 128: "input:tool_use.name";
//   - a tool without a schema gets {"type":"object","properties":{}}; a
//     schema without a "type" gets "type":"object" as its first key and
//     keeps its bytes otherwise. A schema of any other type is refused;
//   - "tool_choice": "auto" and "none" are sent ("none" is accepted by every
//     model) and left out without tools, where they mean nothing. A forced
//     choice — "required", a named tool — is answered 400 by current models:
//     "auto" is sent in its place (nothing without tools): "tool_choice";
//   - "temperature" and "top_p" are not sent: "temperature", "top_p".
//     Current models refuse any value but their default, older ones both
//     together, and the encoder does not know the model. (Sending them to
//     targets that are known to take them is a later, per-target setting.)
//   - stop sequences of white space only are left out: "stop.blank".
//
// What is refused (an error wrapping ErrUnsupported that names the field):
// an image that is not base64 of a type Anthropic accepts or an http(s) URL
// ("messages[0].image.media_type", ".image.url"), an image anywhere but in
// a user message, a part its role cannot hold, a tool call without id or
// name or with an input that is not a JSON object, a tool with a schema
// that is not an object schema, a request in which no message is left or
// over the ir limits. An image is never left out. A tool declared under a
// name that does not match ^[a-zA-Z0-9_-]{1,128}$ is refused with a
// *ir.BadRequestError that names "tools[n].name".
//
// Tool schemas and tool inputs are copied into the body byte for byte; the
// body is written by hand for that reason.
func EncodeRequest(req ir.Request, model string) (body []byte, dropped []string, err error) {
	e := requestEncoder{dropped: append([]string(nil), req.Dropped...), ids: map[string]string{}, taken: map[string]bool{}, next: map[string]int{}}
	if err := e.request(req, model); err != nil {
		return nil, nil, err
	}
	return e.b, ir.Dropped(e.dropped), nil
}

type requestEncoder struct {
	b       []byte
	dropped []string

	ids   map[string]string // a tool call id of the request → the id its latest call was written with
	taken map[string]bool   // the ids that were written for a call
	next  map[string]int    // per cleaned id: the first number callID has not tried yet
}

func (e *requestEncoder) raw(s string)     { e.b = append(e.b, s...) }
func (e *requestEncoder) str(s string)     { e.b = ir.AppendString(e.b, s) }
func (e *requestEncoder) drop(name string) { e.dropped = append(e.dropped, name) }

// wireMessage is one message as it will be written: its blocks, each the
// JSON text of one content block. results come first.
type wireMessage struct {
	role    ir.Role
	results [][]byte
	blocks  [][]byte
}

func (m *wireMessage) size() int { return len(m.results) + len(m.blocks) }

func (e *requestEncoder) request(req ir.Request, model string) error {
	switch {
	case model == "":
		return unsupported("model")
	case len(req.Messages) == 0:
		return unsupported("messages")
	case len(req.Messages) > ir.MaxMessages:
		return overLimit("messages", ir.MaxMessages)
	case len(req.System) > ir.MaxParts:
		return overLimit("system", ir.MaxParts)
	case len(req.Tools) > ir.MaxTools:
		return overLimit("tools", ir.MaxTools)
	case req.MaxTokens < 0:
		return unsupported("max_tokens")
	}
	system, err := systemText(req.System)
	if err != nil {
		return err
	}
	var msgs []wireMessage
	for i, m := range req.Messages {
		at := func(field string) string { return fmt.Sprintf("messages[%d].%s", i, field) }
		if len(m.Parts) > ir.MaxParts {
			return overLimit(at("parts"), ir.MaxParts)
		}
		var w wireMessage
		switch m.Role {
		case ir.User:
			w, err = e.user(m, at)
		case ir.Assistant:
			w, err = e.assistant(m, at)
		default:
			err = unsupported(at("role"))
		}
		if err != nil {
			return err
		}
		if w.size() == 0 {
			continue // nothing of the message is sent: there is no turn
		}
		if n := len(msgs); n > 0 && msgs[n-1].role == w.role {
			// Roles alternate: one message.
			last := &msgs[n-1]
			if last.size()+w.size() > ir.MaxParts {
				return overLimit(at("parts"), ir.MaxParts)
			}
			last.results = append(last.results, w.results...)
			last.blocks = append(last.blocks, w.blocks...)
			continue
		}
		msgs = append(msgs, w)
	}
	if len(msgs) == 0 {
		return unsupported("messages")
	}
	if msgs[0].role != ir.User {
		e.drop(droppedFirstTurn)
		first := wireMessage{role: ir.User, blocks: [][]byte{encodeText(firstTurnText)}}
		msgs = append([]wireMessage{first}, msgs...)
	}
	if msgs[len(msgs)-1].role != ir.User {
		e.drop(droppedLastTurn)
		msgs = append(msgs, wireMessage{role: ir.User, blocks: [][]byte{encodeText(ir.ContinueText)}})
	}

	e.raw(`{"model":`)
	e.str(model)
	e.raw(`,"max_tokens":`)
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
		e.drop(droppedMaxTokensDefault)
	}
	e.b = strconv.AppendInt(e.b, int64(maxTokens), 10)
	if system != "" {
		e.raw(`,"system":`)
		e.str(system)
	}
	e.raw(`,"messages":[`)
	for i, m := range msgs {
		if i > 0 {
			e.raw(",")
		}
		e.raw(`{"role":"` + string(m.role) + `","content":[`)
		for j, block := range append(m.results[:len(m.results):len(m.results)], m.blocks...) {
			if j > 0 {
				e.raw(",")
			}
			e.b = append(e.b, block...)
		}
		e.raw(`]}`)
	}
	e.raw(`]`)
	if err := e.tools(req.Tools); err != nil {
		return err
	}
	if err := e.toolChoice(req.ToolChoice, req.Tools); err != nil {
		return err
	}
	if req.Temperature != nil {
		e.drop(droppedTemperature)
	}
	if req.TopP != nil {
		e.drop(droppedTopP)
	}
	var stops []string
	for _, s := range req.Stop {
		if strings.TrimSpace(s) == "" {
			e.drop(droppedStopBlank)
			continue
		}
		stops = append(stops, s)
	}
	if len(stops) > 0 {
		e.raw(`,"stop_sequences":[`)
		for i, s := range stops {
			if i > 0 {
				e.raw(",")
			}
			e.str(s)
		}
		e.raw(`]`)
	}
	if req.Stream {
		e.raw(`,"stream":true`)
	}
	e.raw(`}`)
	return nil
}

// systemText joins the parts of the system prompt by a blank line.
func systemText(parts []ir.Part) (string, error) {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case ir.Text:
			texts = append(texts, p.Text)
		case ir.Image:
			return "", unsupported("system.image")
		default:
			return "", unsupported("system.part")
		}
	}
	s := strings.Join(texts, "\n\n")
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	return s, nil
}

func encodeText(s string) []byte {
	return append(ir.AppendString([]byte(`{"type":"text","text":`), s), '}')
}

// encodeImage writes an image block, or refuses an image Anthropic does not
// take.
func encodeImage(p ir.Part, at func(string) string) ([]byte, error) {
	switch {
	case p.Data == "":
		return nil, unsupported(at("image.data"))
	case p.MediaType != "":
		if !imageTypes[p.MediaType] {
			return nil, unsupported(at("image.media_type"))
		}
		b := append([]byte(`{"type":"image","source":{"type":"base64","media_type":"`), p.MediaType...)
		b = ir.AppendString(append(b, `","data":`...), p.Data)
		return append(b, `}}`...), nil
	case strings.HasPrefix(p.Data, "https://") || strings.HasPrefix(p.Data, "http://"):
		b := ir.AppendString([]byte(`{"type":"image","source":{"type":"url","url":`), p.Data)
		return append(b, `}}`...), nil
	}
	return nil, unsupported(at("image.url"))
}

// clean writes s with "_" for every character Anthropic does not take in a
// tool call id or a tool name; ok says nothing had to be changed.
func clean(s string) (out string, ok bool) {
	b := []byte(s)
	ok = true
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			b[i], ok = '_', false
		}
	}
	return string(b), ok
}

// callID returns the id a tool call is written with: its own, cleaned, when
// no call before it was written with that, else with a number added. The
// result that answers the call asks resultID for it.
func (e *requestEncoder) callID(id string) string {
	base, _ := clean(id)
	w := base
	// The search goes on where the last one for this base ended: every
	// number below that is taken, so a history that repeats one id is not
	// searched from "_2" again each time.
	n := max(e.next[base], 2)
	for ; e.taken[w]; n++ {
		w = base + "_" + strconv.Itoa(n)
	}
	e.ids[id], e.taken[w], e.next[base] = w, true, n
	return w
}

// resultID returns the id a tool result is written with: that of the latest
// call with its id, which stands in the turn before it.
func (e *requestEncoder) resultID(id string) string {
	if w, ok := e.ids[id]; ok {
		return w
	}
	w, _ := clean(id)
	return w
}

// user writes the blocks of a user message: its tool results, and the rest.
func (e *requestEncoder) user(m ir.Message, at func(string) string) (wireMessage, error) {
	w := wireMessage{role: ir.User}
	// The results of the message, and which of them a note names: the index
	// of the one result with that note, or -1 when several have it.
	type result struct {
		part   ir.Part
		images [][]byte
	}
	var results []result
	notes := map[string]int{}
	lastResult := -1
	for i, p := range m.Parts {
		if p.Kind != ir.ToolResult {
			continue
		}
		if p.Data != "" || p.MediaType != "" {
			return w, unsupported(at("tool_result.image"))
		}
		if p.ToolID == "" {
			return w, unsupported(at("tool_result.id"))
		}
		note := ir.ToolImageNote(p.ToolID)
		if _, twice := notes[note]; twice {
			notes[note] = -1
		} else {
			notes[note] = len(results)
		}
		results = append(results, result{part: p})
		lastResult = i
	}
	for i := 0; i < len(m.Parts); i++ {
		p := m.Parts[i]
		switch p.Kind {
		case ir.ToolResult:
		case ir.Text:
			// A note after the results, with images behind it: they are that
			// result's.
			if n, ok := notes[p.Text]; ok && n >= 0 && i > lastResult && i+1 < len(m.Parts) && m.Parts[i+1].Kind == ir.Image {
				for i+1 < len(m.Parts) && m.Parts[i+1].Kind == ir.Image {
					block, err := encodeImage(m.Parts[i+1], at)
					if err != nil {
						return w, err
					}
					results[n].images = append(results[n].images, block)
					i++
				}
				continue
			}
			if strings.TrimSpace(p.Text) != "" {
				w.blocks = append(w.blocks, encodeText(p.Text))
			}
		case ir.Image:
			block, err := encodeImage(p, at)
			if err != nil {
				return w, err
			}
			w.blocks = append(w.blocks, block)
		case ir.ToolUse:
			return w, unsupported(at("tool_use"))
		case ir.Thinking:
			return w, unsupported(at("thinking"))
		default:
			return w, unsupported(at("part"))
		}
	}
	for _, r := range results {
		b := ir.AppendString([]byte(`{"type":"tool_result","tool_use_id":`), e.resultID(r.part.ToolID))
		b = append(b, `,"content":`...)
		if len(r.images) == 0 {
			b = ir.AppendString(b, r.part.Text)
		} else {
			b = append(b, '[')
			if r.part.Text != ir.ToolImageText && strings.TrimSpace(r.part.Text) != "" {
				b = append(append(b, encodeText(r.part.Text)...), ',')
			}
			b = append(append(b, bytes.Join(r.images, []byte{','})...), ']')
		}
		if r.part.IsError {
			b = append(b, `,"is_error":true`...)
		}
		w.results = append(w.results, append(b, '}'))
	}
	return w, nil
}

func (e *requestEncoder) assistant(m ir.Message, at func(string) string) (wireMessage, error) {
	w := wireMessage{role: ir.Assistant}
	calls := 0
	ids := map[string]bool{}
	for _, p := range m.Parts {
		switch p.Kind {
		case ir.Text:
			if strings.TrimSpace(p.Text) != "" {
				w.blocks = append(w.blocks, encodeText(p.Text))
			}
		case ir.Thinking:
			e.drop(ir.DroppedThinking)
		case ir.ToolUse:
			if calls++; calls > ir.MaxToolCalls {
				return w, overLimit(at("tool_use"), ir.MaxToolCalls)
			}
			if p.ToolID == "" || ids[p.ToolID] {
				return w, unsupported(at("tool_use.id"))
			}
			if p.ToolName == "" {
				return w, unsupported(at("tool_use.name"))
			}
			input, err := ir.ToolInput(p.Input)
			if err != nil {
				return w, unsupportedBecause(at("tool_use.input"), err)
			}
			ids[p.ToolID] = true
			name, ok := clean(p.ToolName)
			if !ok || len(name) > maxToolName {
				// Not a tool the request declares (those are refused): a call
				// the client recorded from another provider.
				name = name[:min(len(name), maxToolName)]
				e.drop(droppedToolUseName)
			}
			b := ir.AppendString([]byte(`{"type":"tool_use","id":`), e.callID(p.ToolID))
			b = ir.AppendString(append(b, `,"name":`...), name)
			b = append(append(b, `,"input":`...), input...)
			w.blocks = append(w.blocks, append(b, '}'))
		case ir.Image:
			return w, unsupported(at("assistant.image"))
		case ir.ToolResult:
			return w, unsupported(at("tool_result"))
		default:
			return w, unsupported(at("part"))
		}
	}
	return w, nil
}

// objectSchema returns the schema as Anthropic takes it: an object schema.
func objectSchema(schema json.RawMessage) ([]byte, error) {
	if len(schema) == 0 {
		return []byte(`{"type":"object","properties":{}}`), nil
	}
	if err := ir.CheckObject(schema); err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(schema, &top); err != nil {
		return nil, ir.ErrBadJSON
	}
	kind, has := top["type"]
	switch {
	case has && string(bytes.TrimSpace(kind)) == `"object"`:
		return schema, nil
	case has:
		return nil, ir.ErrBadJSON
	case len(top) == 0:
		return []byte(`{"type":"object"}`), nil
	}
	// The first key of the object; the rest keeps its bytes.
	rest := bytes.TrimLeft(schema, " \t\r\n")[1:]
	out := make([]byte, 0, len(rest)+17)
	out = append(out, `{"type":"object",`...)
	return append(out, rest...), nil
}

func (e *requestEncoder) tools(tools []ir.Tool) error {
	if len(tools) == 0 {
		return nil
	}
	e.raw(`,"tools":[`)
	for i, t := range tools {
		if name, ok := clean(t.Name); !ok || name == "" || len(name) > maxToolName {
			return &ir.BadRequestError{Format: "messages", Field: fmt.Sprintf("tools[%d].name", i), Reason: "is not a name of 1 to 128 of the characters a-z, A-Z, 0-9, _ and - (Anthropic takes no other)"}
		}
		schema, err := objectSchema(t.Schema)
		if err != nil {
			return unsupportedBecause(fmt.Sprintf("tools[%d].input_schema", i), err)
		}
		if i > 0 {
			e.raw(",")
		}
		e.raw(`{"name":`)
		e.str(t.Name)
		if t.Description != "" {
			e.raw(`,"description":`)
			e.str(t.Description)
		}
		e.raw(`,"input_schema":`)
		e.b = append(e.b, schema...)
		e.raw(`}`)
	}
	e.raw(`]`)
	return nil
}

func (e *requestEncoder) toolChoice(c ir.ToolChoice, tools []ir.Tool) error {
	switch c.Mode {
	case ir.ChoiceUnset:
	case ir.ChoiceAuto, ir.ChoiceNone:
		if len(tools) > 0 {
			e.raw(`,"tool_choice":{"type":"` + string(c.Mode) + `"}`)
		}
	case ir.ChoiceRequired, ir.ChoiceTool:
		if c.Mode == ir.ChoiceTool && c.Name == "" {
			return unsupported("tool_choice.name")
		}
		// {"type":"any"} and {"type":"tool"} are answered 400 by current
		// models: the model decides.
		e.drop(droppedToolChoice)
		if len(tools) > 0 {
			e.raw(`,"tool_choice":{"type":"auto"}`)
		}
	default:
		return unsupported("tool_choice")
	}
	return nil
}
