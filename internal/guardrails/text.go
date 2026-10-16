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
// text, instructions and messages, in order. ok is false when the body is no
// request of that format or has no text.
func RequestText(format string, body []byte) (string, bool) {
	var parts []string
	switch format {
	case FormatChat:
		var r struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &r) != nil {
			return "", false
		}
		for _, m := range r.Messages {
			parts = appendContent(parts, m.Content)
		}
	case FormatResponses:
		var r struct {
			Instructions string          `json:"instructions"`
			Input        json.RawMessage `json:"input"`
		}
		if json.Unmarshal(body, &r) != nil {
			return "", false
		}
		if r.Instructions != "" {
			parts = append(parts, r.Instructions)
		}
		var s string
		var items []struct {
			Content json.RawMessage `json:"content"`
		}
		switch {
		case json.Unmarshal(r.Input, &s) == nil:
			if s != "" {
				parts = append(parts, s)
			}
		case json.Unmarshal(r.Input, &items) == nil:
			for _, it := range items {
				parts = appendContent(parts, it.Content)
			}
		}
	case FormatMessages:
		var r struct {
			System   json.RawMessage `json:"system"`
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &r) != nil {
			return "", false
		}
		parts = appendContent(parts, r.System)
		for _, m := range r.Messages {
			parts = appendContent(parts, m.Content)
		}
	case FormatImages:
		var r struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(body, &r) != nil {
			return "", false
		}
		if r.Prompt != "" {
			parts = append(parts, r.Prompt)
		}
	case FormatSpeech:
		var r struct {
			Input string `json:"input"`
		}
		if json.Unmarshal(body, &r) != nil {
			return "", false
		}
		if r.Input != "" {
			parts = append(parts, r.Input)
		}
	case FormatRerank:
		var r struct {
			Query     string            `json:"query"`
			Documents []json.RawMessage `json:"documents"`
		}
		if json.Unmarshal(body, &r) != nil {
			return "", false
		}
		if r.Query != "" {
			parts = append(parts, r.Query)
		}
		for _, d := range r.Documents {
			// A document is a string or an object with a "text".
			var s string
			var o struct {
				Text string `json:"text"`
			}
			switch {
			case json.Unmarshal(d, &s) == nil:
				if s != "" {
					parts = append(parts, s)
				}
			case json.Unmarshal(d, &o) == nil && o.Text != "":
				parts = append(parts, o.Text)
			}
		}
	default:
		// Includes FormatTranscription: a multipart form is not read.
		return "", false
	}
	return joined(parts)
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
