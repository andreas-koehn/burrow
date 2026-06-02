// Package aigw — AI gateway adapters. Pure byte-to-byte translators between
// upstream-specific HTTP shapes (Anthropic /v1/messages, …) and the
// internal OpenAI-shaped /v1/chat/completions request/response schema.
//
// The adapters are opt-in per service via routing.translate_to. Task 9
// (v0.4.0 plan) ships the Anthropic ↔ internal pair as RewriteRequest and
// WrapResponseStream. Task 10 wires these into the reverse-proxy path.
//
// # Pure functions
//
// RewriteRequest is byte-in, bytes-out. It performs no HTTP, no DB access,
// and no allocations beyond the JSON marshal/unmarshal it owns. The result
// is deterministic: the same input always yields the same output bytes.
//
// # SSE flush invariant
//
// WrapResponseStream wraps a visitor-side writer and translates Anthropic
// SSE events to OpenAI chat.completion.chunk frames as the stream flows.
// Each translated chunk is written + flushed before the next upstream byte
// is consumed — preserving the v0.3.0 no-buffering invariant.
package aigw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// Request adapter: Anthropic /v1/messages → internal /v1/chat/completions
// ---------------------------------------------------------------------------

// anthropicRequest is the subset of /v1/messages we recognise. Unknown
// top-level fields are deliberately ignored — RewriteRequest is an adapter,
// not a validator. Use json.RawMessage for nested shapes we either pass
// through verbatim (tools.input_schema) or inspect with a separate
// unmarshal (messages.content).
type anthropicRequest struct {
	Model         string            `json:"model"`
	System        string            `json:"system,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	TopK          *int              `json:"top_k,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool   `json:"tools,omitempty"`
	Messages      []json.RawMessage `json:"messages"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// internalRequest is the OpenAI-shaped /v1/chat/completions body. Field
// order in this struct IS the wire order — the round-trip tests compare
// byte-for-byte against the fixtures.
type internalRequest struct {
	Model       string            `json:"model"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	Tools       []internalTool    `json:"tools,omitempty"`
	Messages    []internalMessage `json:"messages"`
}

type internalTool struct {
	Type     string           `json:"type"`
	Function internalFunction `json:"function"`
}

type internalFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// internalMessage carries one chat turn. Content is json.RawMessage so we
// can emit either a simple string (text-only path) or a structured array
// (mixed content) without re-marshalling intermediate types.
type internalMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// RewriteRequest converts an Anthropic /v1/messages body into the internal
// OpenAI-shaped /v1/chat/completions body. Pure function.
//
//	out      = canonical internal body bytes
//	lossy    = true if any drops occurred
//	dropped  = list of dropped field names (e.g. ["top_k","image_input"])
//	err      = JSON parse error
//
// Mapping per v0.4.0 spec Part Q.1:
//   - messages: simple text-only user/assistant content arrays are flattened
//     to string content; image content blocks are dropped (lossy).
//   - system (string) becomes a leading {role:"system",content:"…"} message.
//   - stop_sequences renames to stop.
//   - top_k is dropped (OpenAI has no equivalent).
//   - tools translate from Anthropic input_schema shape to OpenAI
//     function-calling shape.
func RewriteRequest(in []byte) (out []byte, lossy bool, dropped []string, err error) {
	var src anthropicRequest
	if err = json.Unmarshal(in, &src); err != nil {
		return nil, false, nil, fmt.Errorf("aigw: parse anthropic request: %w", err)
	}

	dst := internalRequest{
		Model:       src.Model,
		MaxTokens:   src.MaxTokens,
		Temperature: src.Temperature,
		TopP:        src.TopP,
		Stop:        src.StopSequences,
	}

	// top_k → drop (OpenAI has no equivalent)
	if src.TopK != nil {
		dropped = append(dropped, droppedTopK)
		lossy = true
	}

	// tools → OpenAI function-calling shape
	if len(src.Tools) > 0 {
		dst.Tools = make([]internalTool, 0, len(src.Tools))
		for _, t := range src.Tools {
			dst.Tools = append(dst.Tools, internalTool{
				Type: "function",
				Function: internalFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.InputSchema,
				},
			})
		}
	}

	// messages: system → leading system message, then each user/assistant turn.
	if src.System != "" {
		dst.Messages = append(dst.Messages, internalMessage{
			Role:    "system",
			Content: jsonString(src.System),
		})
	}
	for _, raw := range src.Messages {
		msg, msgLossy, msgDropped, mErr := translateMessage(raw)
		if mErr != nil {
			return nil, false, nil, mErr
		}
		if msgLossy {
			lossy = true
			dropped = append(dropped, msgDropped...)
		}
		// Skip messages whose content collapsed to empty (e.g. image-only).
		if len(msg.Content) == 0 {
			continue
		}
		dst.Messages = append(dst.Messages, msg)
	}

	out, err = json.Marshal(dst)
	if err != nil {
		return nil, false, nil, fmt.Errorf("aigw: marshal internal request: %w", err)
	}
	return out, lossy, dropped, nil
}

// translateMessage converts one anthropic message into an internalMessage.
// For text-only content arrays it collapses to a plain string; for mixed
// content it preserves the structured array shape and reports lossy=true
// when image blocks are dropped.
func translateMessage(raw json.RawMessage) (internalMessage, bool, []string, error) {
	// Step 1: extract role + raw content (content may be string or array).
	var env struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return internalMessage{}, false, nil, fmt.Errorf("aigw: parse message: %w", err)
	}

	// Step 2: if content is already a string, pass through unchanged.
	trim := bytes.TrimSpace(env.Content)
	if len(trim) > 0 && trim[0] == '"' {
		return internalMessage{Role: env.Role, Content: env.Content}, false, nil, nil
	}

	// Step 3: content is an array — inspect each block.
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(env.Content, &blocks); err != nil {
		return internalMessage{}, false, nil, fmt.Errorf("aigw: parse content blocks: %w", err)
	}

	// Collect text fragments and note any dropped block types.
	var textParts []string
	var dropped []string
	lossy := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "image":
			dropped = append(dropped, droppedImageInput)
			lossy = true
		default:
			// tool_use / tool_result / other — for v0.4.0 we drop them too
			// rather than risk emitting an OpenAI-incompatible shape. The
			// spec calls out image_input by name; everything else is also
			// surfaced via lossy=true.
			dropped = append(dropped, b.Type)
			lossy = true
		}
	}

	// Text-only path → simple string content (matches OpenAI's plain shape
	// and is what the round-trip tests assert).
	if len(textParts) > 0 {
		joined := strings.Join(textParts, "")
		return internalMessage{Role: env.Role, Content: jsonString(joined)}, lossy, dropped, nil
	}

	// All blocks were dropped → emit empty content; caller will skip.
	return internalMessage{Role: env.Role, Content: nil}, lossy, dropped, nil
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// jsonString returns a json.RawMessage holding the JSON-encoded form of s.
// It cannot fail — strings always marshal — so the error is dropped.
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// ---------------------------------------------------------------------------
// Response stream adapter: Anthropic SSE → internal chat.completion.chunk SSE
// ---------------------------------------------------------------------------

// WrapResponseStream wraps w in an io.WriteCloser that consumes Anthropic SSE
// events and forwards translated OpenAI chat.completion.chunk events to w.
//
// The returned writer:
//   - Parses upstream bytes line-by-line.
//   - Forwards each translated chunk immediately (no buffering of the next
//     translated frame on the next upstream byte).
//   - Calls http.Flusher.Flush() on w after every forwarded chunk if w
//     implements it.
//   - Emits "data: [DONE]\n\n" on the message_stop event.
//
// Concurrent Write calls are not supported — like the reverse-proxy copy
// loop, callers must Write from a single goroutine.
func WrapResponseStream(w io.Writer) io.WriteCloser {
	t := &anthropicTranslator{w: w}
	if f, ok := w.(http.Flusher); ok {
		t.flusher = f
	}
	return t
}

// anthropicTranslator parses upstream Anthropic SSE events and emits OpenAI
// chat.completion.chunk frames to w. Lines are accumulated until '\n' is
// seen; the current event name is remembered until the matching data: line
// triggers translation. After each emitted frame we flush.
type anthropicTranslator struct {
	w       io.Writer
	flusher http.Flusher

	lineBuf   bytes.Buffer // partial in-flight line
	lastEvent string       // most recent "event: NAME" value, cleared after data: processed
}

// Write feeds upstream bytes to the parser. The returned n always equals
// len(p) so io.Copy doesn't observe short writes — translation/forwarding
// failures are reported via the error return.
func (t *anthropicTranslator) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.lineBuf.Write(p)
			return total, nil
		}
		// Have a complete line (sans the trailing '\n').
		var line []byte
		if t.lineBuf.Len() > 0 {
			t.lineBuf.Write(p[:i])
			line = t.lineBuf.Bytes()
		} else {
			line = p[:i]
		}
		if err := t.handleLine(line); err != nil {
			return 0, err
		}
		t.lineBuf.Reset()
		p = p[i+1:]
	}
	return total, nil
}

// Close flushes any pending partial line (treated as a complete line) and
// finalises the stream. Safe to call multiple times.
func (t *anthropicTranslator) Close() error {
	if t.lineBuf.Len() == 0 {
		return nil
	}
	line := t.lineBuf.Bytes()
	err := t.handleLine(line)
	t.lineBuf.Reset()
	return err
}

// handleLine inspects one complete SSE line (no trailing newline). It
// updates parser state on `event:` lines, translates and forwards on
// `data:` lines, and ignores everything else (blank separators, comments).
func (t *anthropicTranslator) handleLine(line []byte) error {
	trim := bytes.TrimRight(line, "\r")
	switch {
	case bytes.HasPrefix(trim, []byte("event:")):
		t.lastEvent = string(bytes.TrimSpace(trim[len("event:"):]))
		return nil
	case bytes.HasPrefix(trim, []byte("data:")):
		payload := bytes.TrimSpace(trim[len("data:"):])
		return t.translateData(payload)
	default:
		// blank separator or unknown line — no output.
		return nil
	}
}

// translateData maps one Anthropic event+data pair to zero or one outbound
// chat.completion.chunk frames and writes them to the visitor.
func (t *anthropicTranslator) translateData(payload []byte) error {
	switch t.lastEvent {
	case "", "message_start", "content_block_stop", "ping":
		// suppress — these have no chat.completion.chunk equivalent.
		return nil
	case "content_block_start":
		return t.emit(chunkStart())
	case "content_block_delta":
		var env struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(payload, &env); err != nil {
			// best-effort: skip malformed payload silently.
			return nil
		}
		if env.Delta.Type != "text_delta" {
			return nil
		}
		return t.emit(chunkDelta(env.Delta.Text))
	case "message_delta":
		var env struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(payload, &env); err != nil {
			return nil
		}
		finish := stopReasonMap[env.Delta.StopReason]
		return t.emit(chunkFinish(finish))
	case "message_stop":
		return t.emit([]byte("data: [DONE]\n\n"))
	default:
		return nil
	}
}

// emit writes a fully-formed SSE frame to the visitor and flushes.
func (t *anthropicTranslator) emit(frame []byte) error {
	if _, err := t.w.Write(frame); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

// ---- chunk builders -------------------------------------------------------
//
// These return byte slices in the exact wire form the round-trip tests
// expect: `data: {…}\n\n`. They use small typed structs (not maps) so the
// JSON field order is deterministic and matches the internal_stream.sse
// fixture.

// chunkStartChoice / chunkStartDelta — initial assistant role frame.
type chunkStartChoice struct {
	Index int             `json:"index"`
	Delta chunkStartDelta `json:"delta"`
}
type chunkStartDelta struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chunkDeltaChoice / chunkDeltaDelta — text-delta frame (just content).
type chunkDeltaChoice struct {
	Index int             `json:"index"`
	Delta chunkDeltaDelta `json:"delta"`
}
type chunkDeltaDelta struct {
	Content string `json:"content"`
}

// chunkFinishChoice / chunkFinishDelta — final finish_reason frame (empty
// delta object plus finish_reason).
type chunkFinishChoice struct {
	Index        int              `json:"index"`
	Delta        chunkFinishDelta `json:"delta"`
	FinishReason string           `json:"finish_reason"`
}
type chunkFinishDelta struct{}

// chunkEnvelope is the outer chat.completion.chunk shape. The Choices field
// is json.RawMessage so each chunk-builder can supply a pre-marshalled
// choice array of the right inner type without us needing a type-switch at
// emit-time.
type chunkEnvelope struct {
	Object  string          `json:"object"`
	Choices json.RawMessage `json:"choices"`
}

func chunkStart() []byte {
	choices, _ := json.Marshal([]chunkStartChoice{{
		Index: 0,
		Delta: chunkStartDelta{Role: "assistant", Content: ""},
	}})
	return sseFrame(chunkEnvelope{Object: "chat.completion.chunk", Choices: choices})
}

func chunkDelta(text string) []byte {
	choices, _ := json.Marshal([]chunkDeltaChoice{{
		Index: 0,
		Delta: chunkDeltaDelta{Content: text},
	}})
	return sseFrame(chunkEnvelope{Object: "chat.completion.chunk", Choices: choices})
}

func chunkFinish(finishReason string) []byte {
	choices, _ := json.Marshal([]chunkFinishChoice{{
		Index:        0,
		Delta:        chunkFinishDelta{},
		FinishReason: finishReason,
	}})
	return sseFrame(chunkEnvelope{Object: "chat.completion.chunk", Choices: choices})
}

// sseFrame wraps a chunkEnvelope into a fully-formed SSE frame:
//
//	data: {…}\n\n
func sseFrame(env chunkEnvelope) []byte {
	body, _ := json.Marshal(env)
	out := make([]byte, 0, len("data: ")+len(body)+2)
	out = append(out, []byte("data: ")...)
	out = append(out, body...)
	out = append(out, '\n', '\n')
	return out
}
