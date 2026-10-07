package messages

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// ErrUnencodable is wrapped by the errors of EncodeResponse: the answer
// holds something a Messages answer cannot, or a tool call a client could
// not act on. The provider's answer is at fault, so the gateway answers 502.
var ErrUnencodable = errors.New("messages: the answer cannot be written as a Messages answer")

// fallbackID is the id of an answer whose upstream gave none.
const fallbackID = "msg_burrow"

// messageID makes a Messages id of the upstream's: "msg_" + id, the id
// itself when it is one already.
func messageID(id string) string {
	switch {
	case id == "":
		return fallbackID
	case strings.HasPrefix(id, "msg_"):
		return id
	}
	return "msg_" + id
}

// stopReason maps a neutral stop reason to Anthropic's "stop_reason". A
// provider's content filter and a refusal both arrive as ir.StopRefusal and
// become "refusal"; a reason no format knows becomes "end_turn", because a
// client must be given one it can act on.
func stopReason(stop ir.StopReason) string {
	switch stop {
	case ir.StopMaxTokens:
		return "max_tokens"
	case ir.StopToolUse:
		return "tool_use"
	case ir.StopSequence:
		return "stop_sequence"
	case ir.StopRefusal:
		return "refusal"
	}
	return "end_turn"
}

func appendUsage(b []byte, u ir.Usage) []byte {
	b = append(b, `"usage":{"input_tokens":`...)
	b = strconv.AppendInt(b, int64(max(u.InputTokens, 0)), 10)
	b = append(b, `,"output_tokens":`...)
	b = strconv.AppendInt(b, int64(max(u.OutputTokens, 0)), 10)
	return append(b, '}')
}

// appendMessageHead writes the fields a message begins with, up to and
// including the comma after "model".
func appendMessageHead(b []byte, id, model, fallbackModel string) []byte {
	if model == "" {
		model = fallbackModel
	}
	b = append(b, `{"id":`...)
	b = ir.AppendString(b, messageID(id))
	b = append(b, `,"type":"message","role":"assistant","model":`...)
	b = ir.AppendString(b, model)
	return append(b, ',')
}

// EncodeResponse writes a complete answer as a Messages response body:
//
//	{"id":"msg_…","type":"message","role":"assistant","model":…,"content":[…],
//	 "stop_reason":…,"stop_sequence":null,"usage":{"input_tokens":N,"output_tokens":M}}
//
// Text becomes a text block, a tool call a tool_use block whose "input" is
// the upstream's bytes unchanged, thinking a thinking block with an empty
// signature (there is none to give). The usage figures are the upstream's.
// "stop_sequence" is always null: no target tells which sequence matched.
// The model is the answer's own, or fallbackModel when it has none.
//
// An error wraps ErrUnencodable (and ir.ErrLimit when a limit is exceeded);
// its text holds nothing of the answer.
func EncodeResponse(resp ir.Response, fallbackModel string) ([]byte, error) {
	if len(resp.Parts) > ir.MaxParts {
		return nil, fmt.Errorf("%w: %w: more than %d parts", ErrUnencodable, ir.ErrLimit, ir.MaxParts)
	}
	b := appendMessageHead(make([]byte, 0, 512), resp.ID, resp.Model, fallbackModel)
	b = append(b, `"content":[`...)
	for i, p := range resp.Parts {
		if i > 0 {
			b = append(b, ',')
		}
		switch p.Kind {
		case ir.Text:
			b = append(b, `{"type":"text","text":`...)
			b = ir.AppendString(b, p.Text)
		case ir.Thinking:
			b = append(b, `{"type":"thinking","thinking":`...)
			b = ir.AppendString(b, p.Text)
			b = append(b, `,"signature":""`...)
		case ir.ToolUse:
			if p.ToolID == "" || p.ToolName == "" {
				return nil, fmt.Errorf("%w: a tool call without an id or a name", ErrUnencodable)
			}
			input, err := ir.ToolInput(p.Input)
			if err != nil {
				return nil, fmt.Errorf("%w: a tool call's input: %w", ErrUnencodable, err)
			}
			b = append(b, `{"type":"tool_use","id":`...)
			b = ir.AppendString(b, p.ToolID)
			b = append(b, `,"name":`...)
			b = ir.AppendString(b, p.ToolName)
			b = append(b, `,"input":`...)
			b = append(b, input...)
		default:
			return nil, fmt.Errorf("%w: a part an answer cannot hold", ErrUnencodable)
		}
		b = append(b, '}')
	}
	b = append(b, `],"stop_reason":"`...)
	b = append(b, stopReason(resp.Stop)...)
	b = append(b, `","stop_sequence":null,`...)
	b = appendUsage(b, resp.Usage)
	return append(b, '}'), nil
}

// ErrorType is the Anthropic error type that goes with an HTTP status.
func ErrorType(status int) string {
	switch status {
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	}
	if status >= 400 && status < 500 {
		return "invalid_request_error"
	}
	return "api_error"
}

// EncodeError writes the body of an error response in the Messages shape,
// with the error type that goes with status:
//
//	{"type":"error","error":{"type":"rate_limit_error","message":"…"}}
func EncodeError(status int, message string) []byte {
	return appendError(nil, ErrorType(status), message)
}

func appendError(b []byte, kind, message string) []byte {
	b = append(b, `{"type":"error","error":{"type":"`...)
	b = append(b, kind...)
	b = append(b, `","message":`...)
	b = ir.AppendString(b, message)
	return append(b, `}}`...)
}
