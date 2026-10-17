package guardrails

import (
	"encoding/json"
	"strings"
)

// The formats whose text the guardrails can read. The values are those of
// internal/aigw/translate.Format.
const (
	FormatChat      = "chat"      // OpenAI Chat Completions
	FormatResponses = "responses" // OpenAI Responses
	FormatMessages  = "messages"  // Anthropic Messages
)

// The request formats of the media and rerank endpoints. Only requests have
// text there (their answers are audio, images or scores): RequestText reads
// them, AnswerText and DeltaText do not know them.
const (
	FormatImages        = "images"        // POST /v1/images/generations: prompt
	FormatSpeech        = "speech"        // POST /v1/audio/speech: input
	FormatRerank        = "rerank"        // POST /v1/rerank: query and documents
	FormatTranscription = "transcription" // audio uploads: multipart, no text
	// FormatCompletions is the legacy POST /v1/completions: its "prompt" (a
	// string or a list of strings) is read; its answers are not.
	FormatCompletions = "completions"
)

// The text lives where the translate package's codecs read and write it:
//
//   - Chat: choices[].message.content (a string or text parts) and, streamed,
//     choices[].delta.content;
//   - Responses: output[].content[].text and, streamed, the delta of a
//     "response.output_text.delta" event;
//   - Messages: content[].text of text blocks and, streamed, the text of a
//     "content_block_delta" whose delta is a "text_delta".
//
// encoding/json decodes the string escapes, so "sk-…" is read as
// "sk-…". Tool calls, their arguments and reasoning are not text in this
// sense. Several pieces of text are joined with "\n", so that the end of one
// and the start of the next cannot form a match together.

// AnswerText returns the text of an answer that came in one piece. ok is
// false when the body is not an answer of that format or has no text.
func AnswerText(format string, body []byte) (string, bool) {
	var parts []string
	switch format {
	case FormatChat:
		var a struct {
			Choices []struct {
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &a) != nil {
			return "", false
		}
		for _, c := range a.Choices {
			parts = appendContent(parts, c.Message.Content)
		}
	case FormatResponses:
		var a struct {
			Output []struct {
				Content []textBlock `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(body, &a) != nil {
			return "", false
		}
		for _, o := range a.Output {
			parts = appendBlocks(parts, o.Content, "")
		}
	case FormatMessages:
		var a struct {
			Content []textBlock `json:"content"`
		}
		if json.Unmarshal(body, &a) != nil {
			return "", false
		}
		parts = appendBlocks(parts, a.Content, "text")
	default:
		return "", false
	}
	return joined(parts)
}

// DeltaText returns the text one event of a streamed answer adds: frame is
// the data of one server-sent event. ok is false for an event that adds no
// text (a role, a tool call, usage, the end, "[DONE]").
func DeltaText(format string, frame []byte) (string, bool) {
	switch format {
	case FormatChat:
		var c struct {
			Choices []struct {
				Delta struct {
					Content json.RawMessage `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(frame, &c) != nil {
			return "", false
		}
		var parts []string
		for _, ch := range c.Choices {
			parts = appendContent(parts, ch.Delta.Content)
		}
		return joined(parts)
	case FormatResponses:
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(frame, &ev) != nil || ev.Type != "response.output_text.delta" || ev.Delta == "" {
			return "", false
		}
		return ev.Delta, true
	case FormatMessages:
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(frame, &ev) != nil || ev.Type != "content_block_delta" || ev.Delta.Type != "text_delta" || ev.Delta.Text == "" {
			return "", false
		}
		return ev.Delta.Text, true
	}
	return "", false
}

// RequestText returns the text of a request: what the app wrote as system
// text, instructions, messages and tool results, in order. ok is false when
// the body is no request of that format or has no text.
//
// Tool results are text the app hands the model: a Messages "tool_result"
// block's content (a string or text blocks), a Responses
// "function_call_output" item's output, a Chat message of role "tool".
func RequestText(format string, body []byte) (string, bool) {
	parts, ok := requestParts(format, body)
	if !ok {
		return "", false
	}
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.text
	}
	return joined(texts)
}

// textPart is one piece of a request's text; user marks the text of a
// message of role "user" (and a prompt or input string, which is the user's).
type textPart struct {
	text string
	user bool
}

// parts collects the pieces of a request's text.
type parts []textPart

func (ps *parts) add(text string, user bool) {
	if text != "" {
		*ps = append(*ps, textPart{text, user})
	}
}

// addContent adds the text of a request's content field: a string, or parts
// and blocks with a "text", and the content of "tool_result" blocks.
func (ps *parts) addContent(raw json.RawMessage, user bool) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		ps.add(s, user)
		return
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch {
		case b.Text != "":
			ps.add(b.Text, user)
		case b.Type == "tool_result":
			// One level: a tool result holds a string or text blocks.
			for _, t := range appendContent(nil, b.Content) {
				ps.add(t, user)
			}
		}
	}
}

// requestParts reads the pieces of a request's text (see RequestText).
func requestParts(format string, body []byte) (parts, bool) {
	var ps parts
	switch format {
	case FormatChat:
		var r struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &r) != nil {
			return nil, false
		}
		for _, m := range r.Messages {
			ps.addContent(m.Content, m.Role == "user")
		}
	case FormatResponses:
		var r struct {
			Instructions string          `json:"instructions"`
			Input        json.RawMessage `json:"input"`
		}
		if json.Unmarshal(body, &r) != nil {
			return nil, false
		}
		ps.add(r.Instructions, false)
		var s string
		var items []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Output  json.RawMessage `json:"output"`
		}
		switch {
		case json.Unmarshal(r.Input, &s) == nil:
			ps.add(s, true)
		case json.Unmarshal(r.Input, &items) == nil:
			for _, it := range items {
				if it.Type == "function_call_output" {
					ps.addContent(it.Output, false)
					continue
				}
				ps.addContent(it.Content, it.Role == "user")
			}
		}
	case FormatMessages:
		var r struct {
			System   json.RawMessage `json:"system"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &r) != nil {
			return nil, false
		}
		ps.addContent(r.System, false)
		for _, m := range r.Messages {
			ps.addContent(m.Content, m.Role == "user")
		}
	case FormatCompletions:
		var r struct {
			Prompt json.RawMessage `json:"prompt"`
		}
		if json.Unmarshal(body, &r) != nil {
			return nil, false
		}
		// A string or a list of strings; a prompt of token ids has no text.
		var s string
		var list []string
		switch {
		case json.Unmarshal(r.Prompt, &s) == nil:
			ps.add(s, true)
		case json.Unmarshal(r.Prompt, &list) == nil:
			for _, p := range list {
				ps.add(p, true)
			}
		}
	case FormatImages:
		var r struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(body, &r) != nil {
			return nil, false
		}
		ps.add(r.Prompt, true)
	case FormatSpeech:
		var r struct {
			Input string `json:"input"`
		}
		if json.Unmarshal(body, &r) != nil {
			return nil, false
		}
		ps.add(r.Input, true)
	case FormatRerank:
		var r struct {
			Query     string            `json:"query"`
			Documents []json.RawMessage `json:"documents"`
		}
		if json.Unmarshal(body, &r) != nil {
			return nil, false
		}
		ps.add(r.Query, true)
		for _, d := range r.Documents {
			// A document is a string or an object with a "text".
			var s string
			var o struct {
				Text string `json:"text"`
			}
			switch {
			case json.Unmarshal(d, &s) == nil:
				ps.add(s, false)
			case json.Unmarshal(d, &o) == nil:
				ps.add(o.Text, false)
			}
		}
	default:
		// Includes FormatTranscription: a multipart form is not read.
		return nil, false
	}
	return ps, len(ps) > 0
}

// textBlock is a content part or block: its type and its text.
type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// appendContent appends the text of a content field that is a string or a
// list of parts with a "text" (any type of part that has one: "text",
// "input_text", "output_text").
func appendContent(parts []string, raw json.RawMessage) []string {
	if len(raw) == 0 {
		return parts
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s != "" {
			parts = append(parts, s)
		}
		return parts
	}
	var blocks []textBlock
	if json.Unmarshal(raw, &blocks) == nil {
		parts = appendBlocks(parts, blocks, "")
	}
	return parts
}

// appendBlocks appends the text of each block; with typ set, only of blocks
// of that type.
func appendBlocks(parts []string, blocks []textBlock, typ string) []string {
	for _, b := range blocks {
		if b.Text != "" && (typ == "" || b.Type == typ) {
			parts = append(parts, b.Text)
		}
	}
	return parts
}

func joined(parts []string) (string, bool) {
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}
