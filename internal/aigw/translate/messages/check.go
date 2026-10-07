package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// Stream is what a client holds after it read a Messages stream.
type Stream struct {
	ID, Model        string
	Blocks           []StreamBlock
	StopReason       string // from message_delta; "" when there was none
	StartInputTokens int    // "input_tokens" of message_start
	InputTokens      int    // the final figure: message_delta's when it has one, else the start's
	OutputTokens     int
	Stopped          bool   // message_stop was read: the answer is complete
	ErrType          string // the type of the error event that ended the stream, or ""
	ErrMessage       string
}

// StreamBlock is one content block of a Stream.
type StreamBlock struct {
	Type string // "text", "thinking" or "tool_use"
	Text string // the joined text or thinking deltas

	ToolID, ToolName string
	// PartialJSON is the joined "partial_json" of a tool_use block. Input is
	// what the official SDKs' accumulators make of it: the start's {} when
	// PartialJSON is empty, else PartialJSON.
	PartialJSON string
	Input       string

	Deltas []string // every delta's text, as it came
}

// CheckStream reads the bytes of a Messages stream the way the official
// Anthropic SDKs and Claude Code do, and refuses what they would not take.
// It is the yardstick of this package's StreamEncoder: the self-checks that
// release a pair and the tests judge the encoder's output with it.
//
// It holds, and returns an error otherwise:
//   - every frame is "event: <name>", one "data: <JSON object>" line whose
//     "type" is that name, and a blank line;
//   - the first frame that is no ping is message_start, with a message that
//     has an id, type "message", role "assistant", a model, an empty
//     content, a null stop_reason and a usage — or it is the error event;
//   - content blocks come one after the other, numbered 0, 1, 2… without a
//     gap: content_block_start (a text block with empty text, a thinking
//     block with empty thinking, or a tool_use block with id, name and
//     "input":{}), deltas of that block's kind, content_block_stop; no two
//     blocks are open at once;
//   - the stream ends in exactly one of two ways. Well: message_delta with
//     a known stop_reason and a usage with output_tokens, then message_stop,
//     both while no block is open; the arguments of every tool_use block
//     are then one JSON object. Badly: one error event with an error type
//     and a message, while no block is open, without message_delta or
//     message_stop before it;
//   - nothing follows the end. ping events are allowed before it.
func CheckStream(raw []byte) (Stream, error) { return checkStream(raw, true) }

var errNotEnded = errors.New("the stream has no end")

var knownStops = map[string]bool{"end_turn": true, "max_tokens": true, "stop_sequence": true, "tool_use": true, "pause_turn": true, "refusal": true}

// checkStream is CheckStream; judgeArgs false leaves the arguments of tool
// calls unjudged.
func checkStream(raw []byte, judgeArgs bool) (Stream, error) {
	var (
		s        Stream
		started  bool
		open     = -1 // the index of the open block
		gotDelta bool // message_delta was read
		ended    bool
	)
	n := 0
	for len(raw) > 0 {
		n++
		fail := func(format string, args ...any) (Stream, error) {
			return Stream{}, fmt.Errorf("frame %d: %s", n, fmt.Sprintf(format, args...))
		}
		frame, rest, ok := bytes.Cut(raw, []byte("\n\n"))
		if !ok {
			return fail("no blank line after it")
		}
		raw = rest
		eventLine, dataLine, ok := strings.Cut(string(frame), "\n")
		event, ok1 := strings.CutPrefix(eventLine, "event: ")
		data, ok2 := strings.CutPrefix(dataLine, "data: ")
		if !ok || !ok1 || !ok2 || strings.ContainsAny(data, "\r\n") {
			return fail("not an event line and one data line")
		}
		var f struct {
			Type    string `json:"type"`
			Index   *int   `json:"index"`
			Message *struct {
				ID         string          `json:"id"`
				Type       string          `json:"type"`
				Role       string          `json:"role"`
				Model      string          `json:"model"`
				Content    json.RawMessage `json:"content"`
				StopReason json.RawMessage `json:"stop_reason"`
				Usage      *struct {
					Input  *int `json:"input_tokens"`
					Output *int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Block *struct {
				Type     string          `json:"type"`
				Text     *string         `json:"text"`
				Thinking *string         `json:"thinking"`
				ID       string          `json:"id"`
				Name     string          `json:"name"`
				Input    json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta *struct {
				Type        string  `json:"type"`
				Text        *string `json:"text"`
				Thinking    *string `json:"thinking"`
				PartialJSON *string `json:"partial_json"`
				StopReason  *string `json:"stop_reason"`
			} `json:"delta"`
			Usage *struct {
				Input  *int `json:"input_tokens"`
				Output *int `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Type    string  `json:"type"`
				Message *string `json:"message"`
			} `json:"error"`
		}
		if trimmed := strings.TrimSpace(data); !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(data), &f) != nil {
			return fail("the data is not a JSON object")
		}
		if f.Type != event {
			return fail("event %q with type %q", event, f.Type)
		}
		if ended {
			return fail("%s after the end", event)
		}
		if event == "ping" {
			continue
		}
		if event == "error" {
			if f.Error == nil || f.Error.Type == "" || f.Error.Message == nil {
				return fail("an error without a type or a message")
			}
			if open >= 0 {
				return fail("an error while block %d is open", open)
			}
			if gotDelta {
				return fail("an error after message_delta")
			}
			s.ErrType, s.ErrMessage, ended = f.Error.Type, *f.Error.Message, true
			continue
		}
		if started == (event == "message_start") {
			return fail("%s where message_start has to stand exactly once", event)
		}
		if gotDelta && event != "message_stop" {
			return fail("%s after message_delta", event)
		}
		switch event {
		case "message_start":
			m := f.Message
			if m == nil || m.ID == "" || m.Type != "message" || m.Role != "assistant" || m.Model == "" ||
				string(bytes.TrimSpace(m.Content)) != "[]" || string(bytes.TrimSpace(m.StopReason)) != "null" ||
				m.Usage == nil || m.Usage.Input == nil || m.Usage.Output == nil {
				return fail("message_start without a complete, empty message")
			}
			started = true
			s.ID, s.Model = m.ID, m.Model
			s.StartInputTokens, s.InputTokens = *m.Usage.Input, *m.Usage.Input
		case "content_block_start":
			if open >= 0 {
				return fail("block %d is still open", open)
			}
			if f.Index == nil || *f.Index != len(s.Blocks) || f.Block == nil {
				return fail("a block that is not number %d", len(s.Blocks))
			}
			b := StreamBlock{Type: f.Block.Type}
			switch b.Type {
			case "text":
				if f.Block.Text == nil || *f.Block.Text != "" {
					return fail("a text block that does not start empty")
				}
			case "thinking":
				if f.Block.Thinking == nil || *f.Block.Thinking != "" {
					return fail("a thinking block that does not start empty")
				}
			case "tool_use":
				if f.Block.ID == "" || f.Block.Name == "" || string(bytes.TrimSpace(f.Block.Input)) != "{}" {
					return fail("a tool_use block without id, name or an empty input")
				}
				b.ToolID, b.ToolName, b.Input = f.Block.ID, f.Block.Name, "{}"
			default:
				return fail("a block of type %q", b.Type)
			}
			open = *f.Index
			s.Blocks = append(s.Blocks, b)
		case "content_block_delta":
			if f.Index == nil || *f.Index != open || f.Delta == nil {
				return fail("a delta for a block that is not open")
			}
			b := &s.Blocks[open]
			var piece *string
			switch {
			case b.Type == "text" && f.Delta.Type == "text_delta":
				piece = f.Delta.Text
			case b.Type == "thinking" && f.Delta.Type == "thinking_delta":
				piece = f.Delta.Thinking
			case b.Type == "tool_use" && f.Delta.Type == "input_json_delta":
				piece = f.Delta.PartialJSON
			}
			if piece == nil {
				return fail("a %q delta for a %s block", f.Delta.Type, b.Type)
			}
			b.Deltas = append(b.Deltas, *piece)
			if b.Type == "tool_use" {
				b.PartialJSON += *piece
			} else {
				b.Text += *piece
			}
		case "content_block_stop":
			if f.Index == nil || *f.Index != open {
				return fail("a stop for a block that is not open")
			}
			if b := &s.Blocks[open]; b.PartialJSON != "" {
				b.Input = b.PartialJSON
			}
			open = -1
		case "message_delta":
			if open >= 0 {
				return fail("message_delta while block %d is open", open)
			}
			if f.Delta == nil || f.Delta.StopReason == nil || !knownStops[*f.Delta.StopReason] {
				return fail("message_delta without a known stop_reason")
			}
			if f.Usage == nil || f.Usage.Output == nil {
				return fail("message_delta without output_tokens")
			}
			gotDelta = true
			s.StopReason, s.OutputTokens = *f.Delta.StopReason, *f.Usage.Output
			if f.Usage.Input != nil && *f.Usage.Input > 0 {
				s.InputTokens = *f.Usage.Input
			}
		case "message_stop":
			if !gotDelta {
				return fail("message_stop without message_delta")
			}
			s.Stopped, ended = true, true
		default:
			return fail("an event nobody knows: %q", event)
		}
	}
	if !ended {
		return Stream{}, errNotEnded
	}
	if s.Stopped && judgeArgs {
		for i, b := range s.Blocks {
			if b.Type == "tool_use" && ir.CheckObject([]byte(b.Input)) != nil {
				return Stream{}, fmt.Errorf("block %d: the arguments are not one JSON object", i)
			}
		}
	}
	return s, nil
}
