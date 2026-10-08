package chat

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// ErrUnencodable is wrapped by the errors of EncodeResponse: the answer
// holds something a Chat Completions answer cannot, or a tool call a client
// could not act on. The provider's answer is at fault, so the gateway
// answers 502.
var ErrUnencodable = errors.New("chat: the answer cannot be written as a Chat Completions answer")

// fallbackID is the id of an answer whose upstream gave none.
const fallbackID = "chatcmpl-burrow"

// textGap stands between two texts of one answer: Chat Completions has one
// content string where the other formats have several blocks.
const textGap = "\n\n"

// completionID makes a Chat Completions id of the upstream's: "chatcmpl-" +
// id, the id itself when it is one already.
func completionID(id string) string {
	switch {
	case id == "":
		return fallbackID
	case strings.HasPrefix(id, "chatcmpl-"):
		return id
	}
	return "chatcmpl-" + id
}

// finishReason maps a neutral stop reason to "finish_reason". A stop
// sequence and a reason no format knows are "stop": a client must be given
// one it can act on. "tool_calls" is said only of an answer that holds a
// tool call.
func finishReason(stop ir.StopReason, toolCalls bool) string {
	switch stop {
	case ir.StopMaxTokens:
		return "length"
	case ir.StopRefusal:
		return "content_filter"
	case ir.StopToolUse:
		if toolCalls {
			return "tool_calls"
		}
	}
	return "stop"
}

// appendUsage writes {"prompt_tokens":N,"completion_tokens":M,"total_tokens":N+M}.
func appendUsage(b []byte, u ir.Usage) []byte {
	in, out := int64(max(u.InputTokens, 0)), int64(max(u.OutputTokens, 0))
	total := int64(math.MaxInt64)
	if in <= math.MaxInt64-out {
		total = in + out
	}
	b = append(b, `{"prompt_tokens":`...)
	b = strconv.AppendInt(b, in, 10)
	b = append(b, `,"completion_tokens":`...)
	b = strconv.AppendInt(b, out, 10)
	b = append(b, `,"total_tokens":`...)
	b = strconv.AppendInt(b, total, 10)
	return append(b, '}')
}

// EncodeResponse writes a complete answer as a Chat Completions body:
//
//	{"id":"chatcmpl-…","object":"chat.completion","created":N,"model":…,
//	 "choices":[{"index":0,"message":{"role":"assistant","content":…,"refusal":null,
//	             "reasoning_content":…,"tool_calls":[…]},"logprobs":null,"finish_reason":…}],
//	 "usage":{"prompt_tokens":N,"completion_tokens":M,"total_tokens":N+M}}
//
// "content" is the answer's text — several texts are set apart by a blank
// line — or null when it has none. Thinking is "reasoning_content", the field
// the providers that tell their reasoning use; it is left out when there is
// none. A tool call's "arguments" string holds the upstream's bytes unchanged
// ("{}" when it gave none); "tool_calls" is left out when there are none. The
// usage figures are the upstream's (cached input included in
// "prompt_tokens"), the total their sum. The model is the answer's own, or
// fallbackModel when it has none; now is "created".
//
// An error wraps ErrUnencodable (and ir.ErrLimit when a limit is exceeded);
// its text holds nothing of the answer.
func EncodeResponse(resp ir.Response, fallbackModel string, now time.Time) ([]byte, error) {
	if len(resp.Parts) > ir.MaxParts {
		return nil, fmt.Errorf("%w: %w: more than %d parts", ErrUnencodable, ir.ErrLimit, ir.MaxParts)
	}
	var (
		texts, thinking []string
		calls           []byte
		n               int
	)
	for _, p := range resp.Parts {
		switch p.Kind {
		case ir.Text:
			texts = append(texts, p.Text)
		case ir.Thinking:
			thinking = append(thinking, p.Text)
		case ir.ToolUse:
			if p.ToolID == "" || p.ToolName == "" {
				return nil, fmt.Errorf("%w: a tool call without an id or a name", ErrUnencodable)
			}
			input, err := ir.ToolInput(p.Input)
			if err != nil {
				return nil, toolInputError(err)
			}
			if n > 0 {
				calls = append(calls, ',')
			}
			n++
			calls = append(calls, `{"id":`...)
			calls = ir.AppendString(calls, p.ToolID)
			calls = append(calls, `,"type":"function","function":{"name":`...)
			calls = ir.AppendString(calls, p.ToolName)
			calls = append(calls, `,"arguments":`...)
			calls = ir.AppendString(calls, string(input))
			calls = append(calls, `}}`...)
		default:
			return nil, fmt.Errorf("%w: a part an answer cannot hold", ErrUnencodable)
		}
	}
	model := resp.Model
	if model == "" {
		model = fallbackModel
	}
	b := make([]byte, 0, len(calls)+512)
	b = append(b, `{"id":`...)
	b = ir.AppendString(b, completionID(resp.ID))
	b = append(b, `,"object":"chat.completion","created":`...)
	b = strconv.AppendInt(b, now.Unix(), 10)
	b = append(b, `,"model":`...)
	b = ir.AppendString(b, model)
	b = append(b, `,"choices":[{"index":0,"message":{"role":"assistant","content":`...)
	if len(texts) > 0 {
		b = ir.AppendString(b, strings.Join(texts, textGap))
	} else {
		b = append(b, `null`...)
	}
	b = append(b, `,"refusal":null`...)
	if len(thinking) > 0 {
		b = append(b, `,"reasoning_content":`...)
		b = ir.AppendString(b, strings.Join(thinking, textGap))
	}
	if n > 0 {
		b = append(b, `,"tool_calls":[`...)
		b = append(b, calls...)
		b = append(b, ']')
	}
	b = append(b, `},"logprobs":null,"finish_reason":"`...)
	b = append(b, finishReason(resp.Stop, n > 0)...)
	b = append(b, `"}],"usage":`...)
	b = appendUsage(b, resp.Usage)
	return append(b, '}'), nil
}

// EncodeError writes the body of an HTTP error response in the shape
// OpenAI-compatible clients parse, exactly as the gateway's own errors on
// the OpenAI endpoints have it (WriteError in internal/aigateway/errors.go;
// that package is not imported here, the shape is repeated):
//
//	{"error":{"message":"…","type":"burrow_error","code":"upstream_error"}}
//
// code is Burrow's own code. status is not written: it is the response's.
// The object that ends a failed stream does not come from here: see
// StreamEncoder.
func EncodeError(status int, code, message string) []byte {
	_ = status
	b := append(make([]byte, 0, len(message)+len(code)+64), `{"error":{"message":`...)
	b = ir.AppendString(b, message)
	b = append(b, `,"type":"burrow_error","code":`...)
	b = ir.AppendString(b, code)
	return append(b, `}}`...)
}

// toolInputError says why a tool call's input cannot be written, without
// its content.
func toolInputError(err error) error {
	return fmt.Errorf("%w: a tool call's input: %w", ErrUnencodable, err)
}
