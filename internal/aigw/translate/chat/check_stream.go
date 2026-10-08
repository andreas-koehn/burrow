package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// Stream is what a client holds after it read a Chat Completions stream.
type Stream struct {
	ID, Model string
	Created   int64
	Content   string // the joined "content" deltas
	Reasoning string // the joined "reasoning_content" deltas
	Calls     []StreamCall
	// FinishReason is the finish_reason of the last chunk with choices; ""
	// when none came.
	FinishReason string
	Done         bool // "[DONE]" was read after a finish_reason: the answer is complete
	// The usage chunk, when one came.
	HasUsage                                    bool
	PromptTokens, CompletionTokens, TotalTokens int
	// The error object that ended the stream, or "".
	ErrMessage, ErrType string
}

// StreamCall is one tool call of a Stream, as the SDKs' accumulators build
// it: by its index.
type StreamCall struct {
	ID, Name  string
	Arguments string   // the joined "arguments" pieces
	Deltas    []string // every piece that is not empty, as it came
}

// CheckStream reads the bytes of a Chat Completions stream the way the
// OpenAI SDKs do, and refuses what they would not take or would misread. It
// is the yardstick of this package's StreamEncoder: the self-checks that
// release a pair and the tests judge the encoder's output with it.
//
// It holds, and returns an error otherwise:
//   - every frame is one "data: <JSON object>" line and a blank line, or
//     "data: [DONE]";
//   - a chunk has an id, the object "chat.completion.chunk", a created time
//     and a model, the same in every chunk, and a "choices" list;
//   - the first chunk has one choice with index 0 whose delta has the role
//     "assistant"; later chunks have one choice with index 0 and a delta, or
//     — once, after the finish_reason — no choice and a usage with
//     prompt_tokens, completion_tokens and total_tokens. A chunk with a
//     choice has no usage (null is fine);
//   - a tool call's first delta has the next index (0, 1, 2…), an id, the
//     type "function", a name and an "arguments" string; later deltas of
//     that index carry "arguments" pieces and no other id or name;
//   - the stream ends in exactly one of two ways. Well: a chunk with a
//     finish_reason of "stop", "length", "tool_calls" (only with a tool
//     call) or "content_filter", nothing but the usage chunk after it, then
//     "[DONE]"; the arguments of every tool call are then one JSON object.
//     Badly: one object {"error":{"message":…,"type":…}}, without a
//     finish_reason before it and without "[DONE]" after it;
//   - nothing follows the end.
func CheckStream(raw []byte) (Stream, error) { return checkStream(raw, true) }

var errNotEnded = errors.New("the stream has no end")

var knownFinish = map[string]bool{"stop": true, "length": true, "tool_calls": true, "content_filter": true}

// checkStream is CheckStream; judgeArgs false leaves the arguments of tool
// calls unjudged.
func checkStream(raw []byte, judgeArgs bool) (Stream, error) {
	var (
		s        Stream
		first    = true
		finished bool // a finish_reason was read
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
		data, ok := strings.CutPrefix(string(frame), "data: ")
		if !ok || strings.ContainsAny(data, "\r\n") {
			return fail("not one data line")
		}
		if ended {
			return fail("a frame after the end")
		}
		if data == "[DONE]" {
			if !finished {
				return fail("[DONE] without a finish_reason before it")
			}
			s.Done, ended = true, true
			continue
		}
		var f struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			Model   string `json:"model"`
			Choices *[]struct {
				Index *int `json:"index"`
				Delta *struct {
					Role      *string `json:"role"`
					Content   *string `json:"content"`
					Reasoning *string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function *struct {
							Name      string  `json:"name"`
							Arguments *string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt     *int `json:"prompt_tokens"`
				Completion *int `json:"completion_tokens"`
				Total      *int `json:"total_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if !strings.HasPrefix(data, "{") || json.Unmarshal([]byte(data), &f) != nil {
			return fail("the data is not a JSON object")
		}
		if f.Error != nil {
			if f.Error.Message == "" || f.Error.Type == "" {
				return fail("an error without a message or a type")
			}
			if finished {
				return fail("an error after the finish_reason")
			}
			s.ErrMessage, s.ErrType, ended = f.Error.Message, f.Error.Type, true
			continue
		}
		if f.ID == "" || f.Object != "chat.completion.chunk" || f.Created <= 0 || f.Model == "" || f.Choices == nil {
			return fail("not a complete chat.completion.chunk")
		}
		if first {
			s.ID, s.Model, s.Created = f.ID, f.Model, f.Created
		} else if f.ID != s.ID || f.Model != s.Model || f.Created != s.Created {
			return fail("id, model or created changed")
		}
		choices := *f.Choices
		if len(choices) == 0 {
			u := f.Usage
			if !finished || s.HasUsage || u == nil || u.Prompt == nil || u.Completion == nil || u.Total == nil {
				return fail("a chunk without choices that is not the one usage chunk after the finish_reason")
			}
			s.HasUsage, s.PromptTokens, s.CompletionTokens, s.TotalTokens = true, *u.Prompt, *u.Completion, *u.Total
			continue
		}
		c := choices[0]
		if len(choices) != 1 || c.Index == nil || *c.Index != 0 || c.Delta == nil {
			return fail("not one choice with index 0 and a delta")
		}
		if finished {
			return fail("a choice after the finish_reason")
		}
		if f.Usage != nil {
			return fail("a usage on a chunk with a choice")
		}
		if role := c.Delta.Role; first && (role == nil || *role != "assistant") || role != nil && *role != "assistant" {
			return fail("the first delta has to have the role assistant, and no delta another")
		}
		first = false
		if c.Delta.Content != nil {
			s.Content += *c.Delta.Content
		}
		if c.Delta.Reasoning != nil {
			s.Reasoning += *c.Delta.Reasoning
		}
		for _, tc := range c.Delta.ToolCalls {
			switch {
			case tc.Index == nil || *tc.Index < 0 || *tc.Index > len(s.Calls):
				return fail("a tool call with an index out of order")
			case tc.Function == nil || tc.Function.Arguments == nil:
				return fail("a tool call delta without an arguments string")
			case *tc.Index == len(s.Calls):
				if tc.ID == "" || tc.Type != "function" || tc.Function.Name == "" {
					return fail("a tool call that begins without id, type or name")
				}
				s.Calls = append(s.Calls, StreamCall{ID: tc.ID, Name: tc.Function.Name})
			case tc.ID != "" && tc.ID != s.Calls[*tc.Index].ID || tc.Function.Name != "":
				return fail("a later tool call delta with another id or a name")
			}
			call := &s.Calls[*tc.Index]
			if piece := *tc.Function.Arguments; piece != "" {
				call.Arguments += piece
				call.Deltas = append(call.Deltas, piece)
			}
		}
		if c.FinishReason != nil {
			if !knownFinish[*c.FinishReason] || *c.FinishReason == "tool_calls" && len(s.Calls) == 0 {
				return fail("a finish_reason nobody knows, or tool_calls without a tool call")
			}
			s.FinishReason, finished = *c.FinishReason, true
		}
	}
	if !ended {
		return Stream{}, errNotEnded
	}
	if s.Done && judgeArgs {
		for i, call := range s.Calls {
			if ir.CheckObject([]byte(call.Arguments)) != nil {
				return Stream{}, fmt.Errorf("tool call %d: the arguments are not one JSON object", i)
			}
		}
	}
	return s, nil
}
