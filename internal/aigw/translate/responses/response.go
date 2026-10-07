package responses

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// ErrUnencodable is wrapped by the errors of EncodeResponse: the answer
// holds something a Responses answer cannot, or a tool call a client could
// not act on. The provider's answer is at fault, so the gateway answers 502.
var ErrUnencodable = errors.New("responses: the answer cannot be written as a Responses answer")

// fallbackID is the id of an answer whose upstream gave none.
const fallbackID = "resp_burrow"

// The status of a response and of an output item.
const (
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusFailed     = "failed"
)

// failedCode is the "code" of the error of a failed response. A stream that
// fails has no HTTP status left to tell more.
const failedCode = "server_error"

// head is what every response object of one answer begins with.
type head struct {
	id, model string
	created   int64
}

// responseID makes a Responses id of the upstream's: "resp_" + id, the id
// itself when it is one already.
func responseID(id string) string {
	switch {
	case id == "":
		return fallbackID
	case strings.HasPrefix(id, "resp_"):
		return id
	}
	return "resp_" + id
}

// stopStatus maps a neutral stop reason to the response's "status" and, for
// an incomplete one, the reason of "incomplete_details". The token cap is
// "max_output_tokens"; a provider's content filter and a refusal both arrive
// as ir.StopRefusal and become "content_filter". Everything else — the
// model was done, it asks for tools, a stop sequence, a reason no format
// knows — is a completed response: a Responses answer has no other word for
// it, and a client tells a tool turn by the function_call items.
func stopStatus(stop ir.StopReason) (status, reason string) {
	switch stop {
	case ir.StopMaxTokens:
		return statusIncomplete, "max_output_tokens"
	case ir.StopRefusal:
		return statusIncomplete, "content_filter"
	}
	return statusCompleted, ""
}

// appendResponse writes a response object:
//
//	{"id":…,"object":"response","created_at":N,"status":…,"error":…,
//	 "incomplete_details":…,"model":…,"output":[…],
//	 "parallel_tool_calls":true,"tool_choice":"auto","tools":[],"usage":…}
//
// output is the items, comma-joined, without the brackets. usage nil writes
// null; so do an empty failure message and an empty reason.
//
// "parallel_tool_calls", "tool_choice" and "tools" are there because the
// SDKs' Response type requires them; they are the API's defaults, not an
// echo of the request, which the encoders do not see. The usage has the
// upstream's two figures and their sum; "input_tokens_details" and
// "output_tokens_details" are required by the SDKs' ResponseUsage as well
// and say 0: no target tells cached or reasoning tokens apart here.
func appendResponse(b []byte, h head, status string, output []byte, usage *ir.Usage, failure, reason string) []byte {
	b = append(b, `{"id":`...)
	b = ir.AppendString(b, h.id)
	b = append(b, `,"object":"response","created_at":`...)
	b = strconv.AppendInt(b, h.created, 10)
	b = append(b, `,"status":"`...)
	b = append(b, status...)
	b = append(b, `","error":`...)
	if failure != "" {
		b = append(b, `{"code":"`+failedCode+`","message":`...)
		b = ir.AppendString(b, failure)
		b = append(b, '}')
	} else {
		b = append(b, `null`...)
	}
	b = append(b, `,"incomplete_details":`...)
	if reason != "" {
		b = append(b, `{"reason":"`...)
		b = append(b, reason...)
		b = append(b, `"}`...)
	} else {
		b = append(b, `null`...)
	}
	b = append(b, `,"model":`...)
	b = ir.AppendString(b, h.model)
	b = append(b, `,"output":[`...)
	b = append(b, output...)
	b = append(b, `],"parallel_tool_calls":true,"tool_choice":"auto","tools":[],"usage":`...)
	if usage == nil {
		return append(b, `null}`...)
	}
	in, out := int64(max(usage.InputTokens, 0)), int64(max(usage.OutputTokens, 0))
	b = append(b, `{"input_tokens":`...)
	b = strconv.AppendInt(b, in, 10)
	b = append(b, `,"input_tokens_details":{"cached_tokens":0},"output_tokens":`...)
	b = strconv.AppendInt(b, out, 10)
	b = append(b, `,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":`...)
	b = strconv.AppendInt(b, in+out, 10)
	return append(b, `}}`...)
}

// appendItemID writes the id of output item n: "msg_3", "fc_3", "rs_3".
func appendItemID(b []byte, kind ir.PartKind, n int) []byte {
	switch kind {
	case ir.ToolUse:
		b = append(b, `fc_`...)
	case ir.Thinking:
		b = append(b, `rs_`...)
	default:
		b = append(b, `msg_`...)
	}
	return strconv.AppendInt(b, int64(n), 10)
}

// appendTextPart writes an output_text content part; quoted is the text as
// a JSON string.
func appendTextPart(b, quoted []byte) []byte {
	b = append(b, `{"type":"output_text","text":`...)
	b = append(b, quoted...)
	return append(b, `,"annotations":[]}`...)
}

// appendMessageItem writes a message item; content is its parts,
// comma-joined.
func appendMessageItem(b []byte, n int, status string, content []byte) []byte {
	b = append(b, `{"id":"`...)
	b = appendItemID(b, ir.Text, n)
	b = append(b, `","type":"message","role":"assistant","status":"`...)
	b = append(b, status...)
	b = append(b, `","content":[`...)
	b = append(b, content...)
	return append(b, `]}`...)
}

// appendCallItem writes a function_call item; arguments is the JSON text of
// the call's input, written as a JSON string.
func appendCallItem(b []byte, n int, callID, name, arguments, status string) []byte {
	b = append(b, `{"id":"`...)
	b = appendItemID(b, ir.ToolUse, n)
	b = append(b, `","type":"function_call","call_id":`...)
	b = ir.AppendString(b, callID)
	b = append(b, `,"name":`...)
	b = ir.AppendString(b, name)
	b = append(b, `,"arguments":`...)
	b = ir.AppendString(b, arguments)
	b = append(b, `,"status":"`...)
	b = append(b, status...)
	return append(b, `"}`...)
}

// appendReasoningItem writes a reasoning item whose summary is the thinking
// text; no text, no summary part.
func appendReasoningItem(b []byte, n int, text string) []byte {
	b = append(b, `{"id":"`...)
	b = appendItemID(b, ir.Thinking, n)
	b = append(b, `","type":"reasoning","summary":[`...)
	if text != "" {
		b = append(b, `{"type":"summary_text","text":`...)
		b = ir.AppendString(b, text)
		b = append(b, '}')
	}
	return append(b, `]}`...)
}

// EncodeResponse writes a complete answer as a Responses body:
//
//	{"id":"resp_…","object":"response","created_at":N,"status":"completed","error":null,
//	 "incomplete_details":null,"model":…,"output":[…],
//	 "parallel_tool_calls":true,"tool_choice":"auto","tools":[],
//	 "usage":{"input_tokens":N,"input_tokens_details":{"cached_tokens":0},
//	          "output_tokens":M,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":N+M}}
//
// The output items stand in part order and are numbered from 0 in their
// ids. Text becomes a message item with an output_text part (consecutive
// texts are the parts of one message), a tool call a function_call item
// whose "arguments" string holds the upstream's bytes unchanged ("{}" when
// it gave none), thinking a reasoning item whose summary is the text.
// "output_text" is not written: it is a convenience the SDKs compute. The
// usage figures are the upstream's, the total their sum. The stop reason
// decides the status: see stopStatus. The model is the answer's own, or
// fallbackModel when it has none; now is the response's "created_at".
//
// An error wraps ErrUnencodable (and ir.ErrLimit when a limit is exceeded);
// its text holds nothing of the answer.
func EncodeResponse(resp ir.Response, fallbackModel string, now time.Time) ([]byte, error) {
	if len(resp.Parts) > ir.MaxParts {
		return nil, fmt.Errorf("%w: %w: more than %d parts", ErrUnencodable, ir.ErrLimit, ir.MaxParts)
	}
	var (
		output  []byte
		content []byte // the parts of the message being gathered
		quoted  []byte
		n       int
		inText  bool
	)
	item := func() {
		if n > 0 {
			output = append(output, ',')
		}
	}
	closeMessage := func() {
		if inText {
			item()
			output = appendMessageItem(output, n, statusCompleted, content)
			n++
			content, inText = content[:0], false
		}
	}
	for _, p := range resp.Parts {
		if p.Kind != ir.Text {
			closeMessage()
		}
		switch p.Kind {
		case ir.Text:
			if inText {
				content = append(content, ',')
			}
			quoted = ir.AppendString(quoted[:0], p.Text)
			content = appendTextPart(content, quoted)
			inText = true
		case ir.Thinking:
			item()
			output = appendReasoningItem(output, n, p.Text)
			n++
		case ir.ToolUse:
			if p.ToolID == "" || p.ToolName == "" {
				return nil, fmt.Errorf("%w: a tool call without an id or a name", ErrUnencodable)
			}
			input, err := ir.ToolInput(p.Input)
			if err != nil {
				return nil, fmt.Errorf("%w: a tool call's input: %w", ErrUnencodable, err)
			}
			item()
			output = appendCallItem(output, n, p.ToolID, p.ToolName, string(input), statusCompleted)
			n++
		default:
			return nil, fmt.Errorf("%w: a part an answer cannot hold", ErrUnencodable)
		}
	}
	closeMessage()
	model := resp.Model
	if model == "" {
		model = fallbackModel
	}
	status, reason := stopStatus(resp.Stop)
	h := head{id: responseID(resp.ID), model: model, created: now.Unix()}
	return appendResponse(make([]byte, 0, len(output)+256), h, status, output, &resp.Usage, "", reason), nil
}

// EncodeError writes the body of an HTTP error response in the shape
// OpenAI-compatible clients parse, exactly as the gateway's own errors on
// the OpenAI endpoints have it (WriteError in internal/aigateway/errors.go;
// that package is not imported here, the shape is repeated):
//
//	{"error":{"message":"…","type":"burrow_error","code":"upstream_error"}}
//
// code is Burrow's own code. status is not written: it is the response's.
// The event that ends a failed stream does not come from here: see
// StreamEncoder.
func EncodeError(status int, code, message string) []byte {
	_ = status
	b := append(make([]byte, 0, len(message)+len(code)+64), `{"error":{"message":`...)
	b = ir.AppendString(b, message)
	b = append(b, `,"type":"burrow_error","code":`...)
	b = ir.AppendString(b, code)
	return append(b, `}}`...)
}
