package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
	"github.com/ankoehn/burrow/internal/aigw/translate/responses"
)

// This file holds the tool-call checks a pair is released by (Global
// Constraints: "a pair is released only when its tool-call tests pass").
// They run against the pair's own Request and Response — the code a
// gateway request goes through — the first time Released is asked, and in
// the tests, where they are also run against pairs that were broken on
// purpose.

// recorder is the caller's side of a checked answer.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *recorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(p)
}

// answer plays an upstream answer through the pair in pieces of the given
// size and returns what the caller got.
func answer(p *pair, stream bool, contentType string, body []byte, piece int) *recorder {
	rec := &recorder{header: http.Header{}}
	w := p.Response(rec, ResponseOptions{Stream: stream, RequestedModel: "asked-for"})
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	for i := 0; i < len(body); i += piece {
		if _, err := w.Write(body[i:min(i+piece, len(body))]); err != nil {
			break
		}
	}
	w.Finish()
	return rec
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// The vectors of messages-chat: a turn with a tool definition, two tool
// calls in one assistant turn and their two results; an answer with two
// tool calls; and the arguments those calls have in the streamed answer.
const (
	checkMessagesRequest = `{"model":"asked-for","max_tokens":64,
"tools":[{"name":"get_weather","description":"Weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
"tool_choice":{"type":"auto"},
"messages":[
 {"role":"user","content":"Weather in Oslo and Rome?"},
 {"role":"assistant","content":[{"type":"text","text":"Checking."},
  {"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Oslo"}},
  {"type":"tool_use","id":"toolu_2","name":"get_weather","input":{"city":"Rome"}}]},
 {"role":"user","content":[
  {"type":"tool_result","tool_use_id":"toolu_1","content":"4°C"},
  {"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"19°C"}]}]}]}`
	checkChatRequest = `{"model":"target-model",
"messages":[
 {"role":"user","content":"Weather in Oslo and Rome?"},
 {"role":"assistant","content":"Checking.","tool_calls":[
  {"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}},
  {"id":"toolu_2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Rome\"}"}}]},
 {"role":"tool","tool_call_id":"toolu_1","content":"4°C"},
 {"role":"tool","tool_call_id":"toolu_2","content":"19°C"}],
"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
"tool_choice":"auto","max_tokens":64}`

	checkChatAnswer = `{"id":"chatcmpl-check","model":"m-up","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"Checking.","tool_calls":[
 {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\",\"days\":[1,2,3]}"}},
 {"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{ \"city\": \"Rome\", \"unit\": \"°C\" }"}}]}}],
"usage":{"prompt_tokens":31,"completion_tokens":17}}`
	checkMessagesAnswer = `{"id":"msg_chatcmpl-check","type":"message","role":"assistant","model":"m-up","content":[
 {"type":"text","text":"Checking."},
 {"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Oslo","days":[1,2,3]}},
 {"type":"tool_use","id":"call_2","name":"get_weather","input":{"city":"Rome","unit":"°C"}}],
"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":31,"output_tokens":17}}`

	checkArgs1 = `{"city":"Oslo","days":[1,2,3]}`
	checkArgs2 = `{ "city": "Rome", "unit": "°C" }`
)

// checkChatStream writes a Chat Completions stream with the two tool calls
// of the vectors, their arguments cut into pieces of a few characters that
// arrive interleaved. cut leaves the end of the stream out.
func checkChatStream(cut bool) []byte {
	var b []byte
	frame := func(delta, finish string) {
		b = append(b, `data: {"id":"chatcmpl-check","model":"m-up","choices":[{"index":0,"delta":`...)
		b = append(b, delta...)
		b = append(b, `,"finish_reason":`...)
		b = append(b, finish...)
		b = append(b, "}]}\n\n"...)
	}
	// call writes one tool-call delta; id and name stand in a call's first delta only.
	call := func(index int, id, name, piece string) {
		delta := fmt.Appendf(nil, `{"tool_calls":[{"index":%d,`, index)
		if id != "" {
			delta = fmt.Appendf(delta, `"id":%q,"type":"function",`, id)
		}
		delta = append(delta, `"function":{`...)
		if name != "" {
			delta = fmt.Appendf(delta, `"name":%q,`, name)
		}
		delta = ir.AppendString(append(delta, `"arguments":`...), piece)
		frame(string(append(delta, `}}]}`...)), "null")
	}
	frame(`{"role":"assistant","content":"Checking."}`, "null")
	call(0, "call_1", "get_weather", "") // the first call opens without arguments
	left := [][]rune{[]rune(checkArgs1), []rune(checkArgs2)}
	for first := true; len(left[0]) > 0 || len(left[1]) > 0; first = false {
		for i := range left {
			n := min(len(left[i]), 3+i)
			if n == 0 {
				continue
			}
			id, name := "", ""
			if i == 1 && first { // the second call opens with its first piece, while the first is open
				id, name = "call_2", "get_weather"
			}
			call(i, id, name, string(left[i][:n]))
			left[i] = left[i][n:]
		}
		if cut && len(left[0]) < len(checkArgs1)/2 {
			return b
		}
	}
	frame(`{}`, `"tool_calls"`)
	b = append(b, `data: {"id":"chatcmpl-check","model":"m-up","choices":[],"usage":{"prompt_tokens":31,"completion_tokens":17}}`+"\n\n"...)
	return append(b, "data: [DONE]\n\n"...)
}

// checkMessagesChat is the tool-call check of the pair messages-chat.
func checkMessagesChat(p *pair) error {
	// Tool definitions, several calls in one turn and their results reach the target.
	out, stream, dropped, err := p.Request([]byte(checkMessagesRequest), nil, "target-model")
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if stream || dropped != nil || !jsonEqual(out, []byte(checkChatRequest)) {
		return errors.New("request: tools, tool calls or tool results are not carried as they must be")
	}
	// A whole answer with several calls.
	rec := answer(p, false, "application/json", []byte(checkChatAnswer), 1<<20)
	if rec.status != http.StatusOK || !jsonEqual(rec.body.Bytes(), []byte(checkMessagesAnswer)) {
		return errors.New("answer: tool calls are not carried as they must be")
	}
	// A streamed answer: the pieces of each call's arguments add up to the upstream's bytes,
	// however the upstream's bytes arrive.
	upstream := checkChatStream(false)
	for _, piece := range []int{1, 13, len(upstream)} {
		rec := answer(p, true, "text/event-stream", upstream, piece)
		s, err := messages.CheckStream(rec.body.Bytes())
		if err != nil {
			return fmt.Errorf("stream: %w", err)
		}
		if rec.status != http.StatusOK || !s.Stopped || s.StopReason != "tool_use" || s.InputTokens != 31 || s.OutputTokens != 17 || len(s.Blocks) != 3 {
			return errors.New("stream: the answer does not end as a tool call must")
		}
		text, one, two := s.Blocks[0], s.Blocks[1], s.Blocks[2]
		if text.Type != "text" || text.Text != "Checking." ||
			one.ToolID != "call_1" || one.ToolName != "get_weather" || one.PartialJSON != checkArgs1 || one.Input != checkArgs1 ||
			two.ToolID != "call_2" || two.ToolName != "get_weather" || two.PartialJSON != checkArgs2 || two.Input != checkArgs2 {
			return errors.New("stream: the arguments do not add up to the upstream's bytes")
		}
	}
	// A stream that is cut inside the calls does not end as a success.
	rec = answer(p, true, "text/event-stream", checkChatStream(true), 13)
	s, err := messages.CheckStream(rec.body.Bytes())
	if err != nil {
		return fmt.Errorf("cut stream: %w", err)
	}
	if s.Stopped || s.ErrType == "" || len(s.Blocks) != 3 {
		return errors.New("cut stream: the answer does not end as an error")
	}
	return nil
}

// The vector of responses-chat: the turn of checkMessagesRequest as a
// Responses caller sends it — the assistant's text and its two calls as
// three items, then the two outputs, one of them as parts. It must become
// checkChatRequest: one assistant message with both calls, then the tool
// messages. The answers are those of messages-chat.
const checkResponsesRequest = `{"model":"asked-for","max_output_tokens":64,
"tools":[{"type":"function","name":"get_weather","description":"Weather for a city","strict":false,"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
"tool_choice":"auto",
"input":[
 {"role":"user","content":"Weather in Oslo and Rome?"},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking.","annotations":[]}]},
 {"type":"function_call","call_id":"toolu_1","name":"get_weather","arguments":"{\"city\":\"Oslo\"}"},
 {"type":"function_call","call_id":"toolu_2","name":"get_weather","arguments":"{\"city\":\"Rome\"}"},
 {"type":"function_call_output","call_id":"toolu_1","output":"4°C"},
 {"type":"function_call_output","call_id":"toolu_2","output":[{"type":"input_text","text":"19°C"}]}]}`

// checkResponsesChat is the tool-call check of the pair responses-chat.
func checkResponsesChat(p *pair) error {
	// Tool definitions, several calls in one turn and their results reach the target.
	out, stream, dropped, err := p.Request([]byte(checkResponsesRequest), nil, "target-model")
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if stream || dropped != nil || !jsonEqual(out, []byte(checkChatRequest)) {
		return errors.New("request: tools, tool calls or tool results are not carried as they must be")
	}
	// A whole answer with several calls: "arguments" is a string that holds the upstream's bytes.
	rec := answer(p, false, "application/json", []byte(checkChatAnswer), 1<<20)
	var body struct {
		ID, Object, Status, Model string
		Output                    []struct {
			Type, Status string
			CallID       string `json:"call_id"`
			Name         string
			Arguments    string
			Content      []struct{ Type, Text string }
		}
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
			Total  int `json:"total_tokens"`
		}
	}
	if rec.status != http.StatusOK || json.Unmarshal(rec.body.Bytes(), &body) != nil || body.Object != "response" || body.Status != "completed" ||
		body.ID == "" || body.Model != "m-up" || body.Usage.Input != 31 || body.Usage.Output != 17 || body.Usage.Total != 48 || len(body.Output) != 3 {
		return errors.New("answer: the response is not carried as it must be")
	}
	text, one, two := body.Output[0], body.Output[1], body.Output[2]
	if text.Type != "message" || len(text.Content) != 1 || text.Content[0].Type != "output_text" || text.Content[0].Text != "Checking." ||
		one.Type != "function_call" || one.CallID != "call_1" || one.Name != "get_weather" || one.Arguments != checkArgs1 ||
		two.Type != "function_call" || two.CallID != "call_2" || two.Name != "get_weather" || two.Arguments != checkArgs2 {
		return errors.New("answer: tool calls are not carried as they must be")
	}
	// A streamed answer: the pieces of each call's arguments add up to the upstream's bytes, and
	// the done events and the final response repeat them, however the upstream's bytes arrive.
	upstream := checkChatStream(false)
	for _, piece := range []int{1, 13, len(upstream)} {
		rec := answer(p, true, "text/event-stream", upstream, piece)
		s, err := responses.CheckStream(rec.body.Bytes())
		if err != nil {
			return fmt.Errorf("stream: %w", err)
		}
		if rec.status != http.StatusOK || !s.Completed || s.InputTokens != 31 || s.OutputTokens != 17 || s.TotalTokens != 48 || len(s.Items) != 3 {
			return errors.New("stream: the answer does not end as a tool call must")
		}
		text, one, two := s.Items[0], s.Items[1], s.Items[2]
		if text.Type != "message" || text.Text != "Checking." ||
			one.Type != "function_call" || one.CallID != "call_1" || one.Name != "get_weather" || one.Arguments != checkArgs1 || strings.Join(one.Deltas, "") != checkArgs1 ||
			two.Type != "function_call" || two.CallID != "call_2" || two.Name != "get_weather" || two.Arguments != checkArgs2 || strings.Join(two.Deltas, "") != checkArgs2 {
			return errors.New("stream: the arguments do not add up to the upstream's bytes")
		}
	}
	// A stream that is cut inside the calls does not end as a success, and no call of it is
	// handed over: a client would run it and send it back with every later request.
	rec = answer(p, true, "text/event-stream", checkChatStream(true), 13)
	s, err := responses.CheckStream(rec.body.Bytes())
	if err != nil {
		return fmt.Errorf("cut stream: %w", err)
	}
	if s.Completed || s.Status != "failed" || s.ErrMessage == "" || len(s.Items) == 0 || s.Items[0].Text != "Checking." {
		return errors.New("cut stream: the answer does not end as a failure")
	}
	for _, it := range s.Items {
		if it.Type == "function_call" && it.Done {
			return errors.New("cut stream: a call that was cut is handed over")
		}
	}
	// A history as a client holds it after such answers — the items of an answer in the order
	// they were written, a call that was cut, a call without an output, an output without a call,
	// outputs after all items — is taken and becomes a conversation a strict server accepts.
	out, _, dropped, err = p.Request([]byte(checkResponsesHistory), nil, "target-model")
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	if chat.CheckRequest(out) != nil || !jsonEqual(out, []byte(checkChatHistory)) ||
		!reflect.DeepEqual(dropped, []string{"input:function_call.arguments", "input:function_call.unanswered", "input:function_call_output.orphan"}) {
		return errors.New("history: calls and outputs are not paired as a Chat Completions server wants them")
	}
	return nil
}

const (
	checkResponsesHistory = `{"model":"asked-for","input":[
 {"role":"user","content":"Weather in Oslo and Rome?"},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking."}]},
 {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Oslo\"}"},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"And Rome."}]},
 {"type":"function_call","call_id":"call_2","name":"get_weather","arguments":"{\"city\":"},
 {"type":"function_call","call_id":"call_3","name":"get_weather","arguments":"{}"},
 {"type":"function_call_output","call_id":"call_2","output":"19°C"},
 {"type":"function_call_output","call_id":"call_1","output":"4°C"},
 {"type":"function_call_output","call_id":"call_9","output":"?"},
 {"role":"user","content":"Thanks."}]}`
	checkChatHistory = `{"model":"target-model","messages":[
 {"role":"user","content":"Weather in Oslo and Rome?"},
 {"role":"assistant","content":"Checking.\n\nAnd Rome.","tool_calls":[
  {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}},
  {"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"{}"}},
  {"id":"call_3","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
 {"role":"tool","tool_call_id":"call_1","content":"4°C"},
 {"role":"tool","tool_call_id":"call_2","content":"19°C"},
 {"role":"tool","tool_call_id":"call_3","content":"[no output]"},
 {"role":"user","content":"Thanks."}]}`
)
