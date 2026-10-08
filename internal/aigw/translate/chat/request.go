// Package chat is the Chat Completions codec of the format translators.
// This file, response.go and stream.go hold the half a target needs: write
// a request from the neutral form (EncodeRequest), read an answer into it,
// whole (DecodeResponse) or streamed (StreamDecoder). The half a caller
// needs is in request_decode.go, response_encode.go and stream_encode.go.
//
// Nothing in this package logs, and no error it returns carries request or
// answer content: an error names the field or the reason only.
package chat

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// maxStop is the number of stop sequences Chat Completions accepts. A
// request with more keeps the first maxStop and reports "stop.extra".
const maxStop = 4

// Names EncodeRequest adds to the dropped list (all fixed, none chosen by a
// client).
const (
	droppedThinking   = ir.DroppedThinking // thinking parts of the history are not sent back
	droppedStopExtra  = "stop.extra"       // stop sequences beyond maxStop
	droppedToolChoice = "tool_choice"      // a choice that needs a tool the request does not carry
)

// ErrUnsupported is wrapped by every error of EncodeRequest, followed by the
// field that cannot be expressed ("messages[2].tool_result.image"). The
// request itself is at fault, so the gateway answers 400. An error for a
// request over one of the ir limits wraps ir.ErrLimit as well.
var ErrUnsupported = errors.New("chat: cannot be expressed in Chat Completions")

func unsupported(field string) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, field)
}

func unsupportedBecause(field string, cause error) error {
	return fmt.Errorf("%w: %s (%w)", ErrUnsupported, field, cause)
}

func tooMany(field string, limit int) error {
	return fmt.Errorf("%w: %s (%w: more than %d)", ErrUnsupported, field, ir.ErrLimit, limit)
}

// EncodeRequest writes req as a Chat Completions body for model. It returns
// the names of things Chat Completions cannot carry, together with the names
// req.Dropped already holds, sorted and without duplicates (nil when there
// are none).
//
// What is not carried, and how it is reported:
//   - Thinking parts of assistant messages are left out: "thinking". An
//     assistant message that holds nothing else, or nothing but empty text,
//     is left out as a whole.
//   - Stop sequences beyond maxStop are left out: "stop.extra".
//   - A tool choice that demands a tool ("required", or a named tool) when
//     the request carries no such tool is left out: "tool_choice". ("auto"
//     and "none" without tools are left out without a report: they mean the
//     same as nothing.)
//   - A ToolResult's error flag has no field: the content gets the prefix
//     "Error: ". Nothing is lost, so nothing is reported.
//   - Several Text parts of an assistant message, and the parts of the system
//     prompt, become one string joined by a blank line.
//
// What is refused (an error wrapping ErrUnsupported that names the field):
// an image anywhere but in a user message ("tool_result.image",
// "assistant.image", "system.image"), a part its role cannot hold, a tool
// call without id or name or with an input that is not a JSON object, a tool
// without a name or with a schema that is not a JSON object, a request
// without messages or over the ir limits. An image is never left out.
//
// Tool schemas are copied into the body byte for byte, and a tool call's
// input becomes the "arguments" string with its bytes unchanged. The body is
// written by hand for that reason: encoding/json would compact and escape
// them.
func EncodeRequest(req ir.Request, model string) (body []byte, dropped []string, err error) {
	e := encoder{dropped: append([]string(nil), req.Dropped...)}
	if err := e.request(req, model); err != nil {
		return nil, nil, err
	}
	return e.b, ir.Dropped(e.dropped), nil
}

type encoder struct {
	b        []byte
	dropped  []string
	messages int // messages written so far
	turns    int // those of them that are not the system message
}

func (e *encoder) raw(s string)     { e.b = append(e.b, s...) }
func (e *encoder) str(s string)     { e.b = ir.AppendString(e.b, s) }
func (e *encoder) drop(name string) { e.dropped = append(e.dropped, name) }

// message opens the next element of "messages".
func (e *encoder) message(role string) {
	if e.messages > 0 {
		e.raw(",")
	}
	e.messages++
	if role != "system" {
		e.turns++
	}
	e.raw(`{"role":"` + role + `"`)
}

func (e *encoder) request(req ir.Request, model string) error {
	switch {
	case model == "":
		return unsupported("model")
	case len(req.Messages) == 0:
		return unsupported("messages")
	case len(req.Messages) > ir.MaxMessages:
		return tooMany("messages", ir.MaxMessages)
	case len(req.System) > ir.MaxParts:
		return tooMany("system", ir.MaxParts)
	case len(req.Tools) > ir.MaxTools:
		return tooMany("tools", ir.MaxTools)
	}
	e.raw(`{"model":`)
	e.str(model)
	e.raw(`,"messages":[`)
	if err := e.system(req.System); err != nil {
		return err
	}
	for i, m := range req.Messages {
		at := func(field string) string { return fmt.Sprintf("messages[%d].%s", i, field) }
		if len(m.Parts) > ir.MaxParts {
			return tooMany(at("parts"), ir.MaxParts)
		}
		var err error
		switch m.Role {
		case ir.User:
			err = e.user(m, at)
		case ir.Assistant:
			err = e.assistant(m, at)
		default:
			err = unsupported(at("role"))
		}
		if err != nil {
			return err
		}
	}
	if e.turns == 0 {
		return unsupported("messages") // every message was an assistant turn with nothing to send
	}
	e.raw(`]`)
	if err := e.tools(req.Tools); err != nil {
		return err
	}
	if err := e.toolChoice(req.ToolChoice, req.Tools); err != nil {
		return err
	}
	if req.MaxTokens < 0 {
		return unsupported("max_tokens")
	}
	if req.MaxTokens > 0 {
		e.raw(`,"max_tokens":`)
		e.b = strconv.AppendInt(e.b, int64(req.MaxTokens), 10)
	}
	if err := e.number("temperature", req.Temperature); err != nil {
		return err
	}
	if err := e.number("top_p", req.TopP); err != nil {
		return err
	}
	if stop := req.Stop; len(stop) > 0 {
		if len(stop) > maxStop {
			stop = stop[:maxStop]
			e.drop(droppedStopExtra)
		}
		e.raw(`,"stop":[`)
		for i, s := range stop {
			if i > 0 {
				e.raw(",")
			}
			e.str(s)
		}
		e.raw(`]`)
	}
	if req.Stream {
		e.raw(`,"stream":true,"stream_options":{"include_usage":true}`)
	}
	e.raw(`}`)
	return nil
}

func (e *encoder) system(parts []ir.Part) error {
	if len(parts) == 0 {
		return nil
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case ir.Text:
			texts = append(texts, p.Text)
		case ir.Image:
			return unsupported("system.image")
		default:
			return unsupported("system.part")
		}
	}
	e.message("system")
	e.raw(`,"content":`)
	e.str(strings.Join(texts, "\n\n"))
	e.raw(`}`)
	return nil
}

// user writes a user message: first one "tool" message per ToolResult part,
// in order, then the remaining content as one "user" message (none when
// there were only tool results).
func (e *encoder) user(m ir.Message, at func(string) string) error {
	rest, onlyText := 0, true
	for _, p := range m.Parts {
		switch p.Kind {
		case ir.Text:
			rest++
		case ir.Image:
			if p.Data == "" {
				return unsupported(at("image.data"))
			}
			rest++
			onlyText = false
		case ir.ToolResult:
			if p.Data != "" || p.MediaType != "" {
				return unsupported(at("tool_result.image"))
			}
			if p.ToolID == "" {
				return unsupported(at("tool_result.id"))
			}
			e.message("tool")
			e.raw(`,"tool_call_id":`)
			e.str(p.ToolID)
			e.raw(`,"content":`)
			if p.IsError {
				e.str("Error: " + p.Text)
			} else {
				e.str(p.Text)
			}
			e.raw(`}`)
		case ir.ToolUse:
			return unsupported(at("tool_use"))
		case ir.Thinking:
			return unsupported(at("thinking"))
		default:
			return unsupported(at("part"))
		}
	}
	if rest == 0 && len(m.Parts) > 0 {
		return nil
	}
	e.message("user")
	e.raw(`,"content":`)
	switch {
	case rest == 0:
		e.raw(`""`)
	case rest == 1 && onlyText:
		for _, p := range m.Parts {
			if p.Kind == ir.Text {
				e.str(p.Text)
			}
		}
	default:
		e.raw(`[`)
		first := true
		for _, p := range m.Parts {
			if p.Kind != ir.Text && p.Kind != ir.Image {
				continue
			}
			if !first {
				e.raw(",")
			}
			first = false
			if p.Kind == ir.Text {
				e.raw(`{"type":"text","text":`)
				e.str(p.Text)
				e.raw(`}`)
				continue
			}
			e.raw(`{"type":"image_url","image_url":{"url":`)
			if p.MediaType != "" {
				e.str("data:" + p.MediaType + ";base64," + p.Data)
			} else {
				e.str(p.Data)
			}
			e.raw(`}}`)
		}
		e.raw(`]`)
	}
	e.raw(`}`)
	return nil
}

func (e *encoder) assistant(m ir.Message, at func(string) string) error {
	var texts []string
	var calls []ir.Part
	ids := map[string]bool{}
	for _, p := range m.Parts {
		switch p.Kind {
		case ir.Text:
			texts = append(texts, p.Text)
		case ir.Thinking:
			e.drop(droppedThinking)
		case ir.ToolUse:
			if len(calls) >= ir.MaxToolCalls {
				return tooMany(at("tool_use"), ir.MaxToolCalls)
			}
			if p.ToolID == "" || ids[p.ToolID] {
				return unsupported(at("tool_use.id"))
			}
			if p.ToolName == "" {
				return unsupported(at("tool_use.name"))
			}
			input, err := ir.ToolInput(p.Input)
			if err != nil {
				return unsupportedBecause(at("tool_use.input"), err)
			}
			ids[p.ToolID] = true
			p.Input = input
			calls = append(calls, p)
		case ir.Image:
			return unsupported(at("assistant.image"))
		case ir.ToolResult:
			return unsupported(at("tool_result"))
		default:
			return unsupported(at("part"))
		}
	}
	content := strings.Join(texts, "\n\n")
	if content == "" && len(calls) == 0 {
		// Only thinking (reported above), only empty text, or nothing at all:
		// "content":"" without tool calls is refused by some servers, so the
		// turn is not written. Two user messages in a row are fine for Chat
		// Completions.
		return nil
	}
	e.message("assistant")
	e.raw(`,"content":`)
	if len(texts) > 0 {
		e.str(content)
	} else {
		e.raw(`null`)
	}
	if len(calls) > 0 {
		e.raw(`,"tool_calls":[`)
		for i, c := range calls {
			if i > 0 {
				e.raw(",")
			}
			e.raw(`{"id":`)
			e.str(c.ToolID)
			e.raw(`,"type":"function","function":{"name":`)
			e.str(c.ToolName)
			e.raw(`,"arguments":`)
			e.str(string(c.Input))
			e.raw(`}}`)
		}
		e.raw(`]`)
	}
	e.raw(`}`)
	return nil
}

func (e *encoder) tools(tools []ir.Tool) error {
	if len(tools) == 0 {
		return nil
	}
	e.raw(`,"tools":[`)
	for i, t := range tools {
		if t.Name == "" {
			return unsupported(fmt.Sprintf("tools[%d].name", i))
		}
		if i > 0 {
			e.raw(",")
		}
		e.raw(`{"type":"function","function":{"name":`)
		e.str(t.Name)
		if t.Description != "" {
			e.raw(`,"description":`)
			e.str(t.Description)
		}
		if len(t.Schema) > 0 {
			if err := ir.CheckObject(t.Schema); err != nil {
				return unsupportedBecause(fmt.Sprintf("tools[%d].schema", i), err)
			}
			e.raw(`,"parameters":`)
			e.b = append(e.b, t.Schema...)
		}
		e.raw(`}}`)
	}
	e.raw(`]`)
	return nil
}

func (e *encoder) toolChoice(c ir.ToolChoice, tools []ir.Tool) error {
	switch c.Mode {
	case ir.ChoiceUnset:
	case ir.ChoiceAuto, ir.ChoiceNone:
		if len(tools) > 0 {
			e.raw(`,"tool_choice":"` + string(c.Mode) + `"`)
		}
	case ir.ChoiceRequired:
		if len(tools) == 0 {
			e.drop(droppedToolChoice)
			return nil
		}
		e.raw(`,"tool_choice":"required"`)
	case ir.ChoiceTool:
		if c.Name == "" {
			return unsupported("tool_choice.name")
		}
		known := false
		for _, t := range tools {
			known = known || t.Name == c.Name
		}
		if !known {
			e.drop(droppedToolChoice)
			return nil
		}
		e.raw(`,"tool_choice":{"type":"function","function":{"name":`)
		e.str(c.Name)
		e.raw(`}}`)
	default:
		return unsupported("tool_choice")
	}
	return nil
}

func (e *encoder) number(key string, v *float64) error {
	if v == nil {
		return nil
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		return unsupported(key)
	}
	e.raw(`,"` + key + `":`)
	e.b = strconv.AppendFloat(e.b, *v, 'g', -1, 64)
	return nil
}
