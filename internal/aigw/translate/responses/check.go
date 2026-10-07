package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// Stream is what a client holds after it read a Responses stream.
type Stream struct {
	ID, Model string
	Items     []StreamItem
	// Status is the status of the response in the last event: "completed",
	// "incomplete" or "failed".
	Status           string
	IncompleteReason string // "incomplete_details.reason" of an incomplete response
	ErrCode          string // "error.code" of a failed response
	ErrMessage       string
	// The usage of the last event; all 0 for a failed response.
	InputTokens, OutputTokens, TotalTokens int
	Completed                              bool // response.completed was read: the answer is complete
}

// StreamItem is one output item of a Stream.
type StreamItem struct {
	Type   string // "message", "function_call" or "reasoning"
	ID     string
	Status string // of the done item: "completed" or "incomplete"; "" for reasoning
	// Text is a message's text (the joined deltas, which the done events
	// repeat) or a reasoning item's summary text.
	Text string

	CallID, Name string
	// Arguments is a function_call's arguments: the joined deltas, which
	// the done event, the done item and the final response repeat.
	Arguments string

	Deltas []string // every delta, as it came
}

// CheckStream reads the bytes of a Responses stream the way the OpenAI SDKs
// and Codex do, and refuses what they would not take or would misread. It
// is the yardstick of this package's StreamEncoder: the self-checks that
// release a pair and the tests judge the encoder's output with it.
//
// It holds, and returns an error otherwise:
//   - every frame is "event: <name>", one "data: <JSON object>" line whose
//     "type" is that name and whose "sequence_number" counts the frames
//     from 0 without a gap, and a blank line. A frame that is one SSE
//     comment line is skipped;
//   - the first event is response.created with a response that has an id,
//     object "response", status "in_progress", a model and an empty output;
//     response.in_progress, when it comes, follows it at once;
//   - output items come one after the other, "output_index" 0, 1, 2…
//     without a gap, and no two are open at once:
//     a message is response.output_item.added (role assistant, status
//     in_progress, no content), response.content_part.added (an empty
//     output_text), response.output_text.delta events,
//     response.output_text.done, response.content_part.done and
//     response.output_item.done, the three of which repeat the joined
//     deltas;
//     a function_call is response.output_item.added (with id, call_id, name
//     and "arguments":""), response.function_call_arguments.delta events,
//     response.function_call_arguments.done and response.output_item.done,
//     both of which repeat the joined deltas and the same ids and name;
//     a reasoning item is response.output_item.added followed at once by
//     response.output_item.done with the same item.
//     Every event of an item names its id ("item_id") and its output_index;
//   - the stream ends, while no item is open, with exactly one of
//     response.completed (status "completed"), response.incomplete (status
//     "incomplete" and a known "incomplete_details.reason") and
//     response.failed (status "failed" and an error with a code and a
//     message). Its response has the id and model of the first event, and
//     its output is the items that were done, each as it was done. A
//     response that is not failed has a usage whose total is the sum, every
//     item of it is "completed", and the arguments of every function_call
//     are one JSON object;
//   - nothing follows the end.
func CheckStream(raw []byte) (Stream, error) {
	var (
		s       Stream
		started bool
		ended   bool
		open    *StreamItem // the item that is open
		itemRaw []string    // per item: the done item, compacted
		joined  strings.Builder
		step    int // within the open item: what must come next
		seq     int
		n       int
	)
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
		if ended {
			return fail("something after the end")
		}
		if bytes.HasPrefix(frame, []byte(":")) && !bytes.Contains(frame, []byte("\n")) {
			continue // a comment: a sign of life
		}
		eventLine, dataLine, ok := strings.Cut(string(frame), "\n")
		event, ok1 := strings.CutPrefix(eventLine, "event: ")
		data, ok2 := strings.CutPrefix(dataLine, "data: ")
		if !ok || !ok1 || !ok2 || strings.ContainsAny(data, "\r\n") {
			return fail("not an event line and one data line")
		}
		var f checkFrame
		if trimmed := strings.TrimSpace(data); !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(data), &f) != nil {
			return fail("the data is not a JSON object")
		}
		if f.Type != event {
			return fail("event %q with type %q", event, f.Type)
		}
		if f.Seq == nil || *f.Seq != seq {
			return fail("%s without sequence_number %d", event, seq)
		}
		seq++
		if started == (event == "response.created") {
			return fail("%s where response.created has to stand exactly once", event)
		}
		// here says whether the event names the open item.
		here := func() bool {
			return open != nil && f.ItemID == open.ID && f.OutputIndex != nil && *f.OutputIndex == len(s.Items)-1
		}
		switch event {
		case "response.created", "response.in_progress":
			r := f.Response
			if r == nil || r.ID == "" || r.Object != "response" || r.Status != statusInProgress || r.Model == "" || len(r.Output) != 0 || r.Output == nil {
				return fail("%s without a response that is in progress and empty", event)
			}
			if event == "response.in_progress" && (seq != 2 || r.ID != s.ID || r.Model != s.Model) {
				return fail("response.in_progress that does not follow response.created")
			}
			started = true
			s.ID, s.Model = r.ID, r.Model
		case "response.output_item.added":
			it := f.Item
			if open != nil {
				return fail("item %d is still open", len(s.Items)-1)
			}
			if it == nil || it.ID == "" || f.OutputIndex == nil || *f.OutputIndex != len(s.Items) {
				return fail("an item that is not number %d", len(s.Items))
			}
			item := StreamItem{Type: it.Type, ID: it.ID}
			switch it.Type {
			case "message":
				if it.Role != "assistant" || it.Status != statusInProgress || it.Content == nil || len(it.Content) != 0 {
					return fail("a message that does not start empty and in progress")
				}
			case "function_call":
				if it.CallID == "" || it.Name == "" || it.Arguments == nil || *it.Arguments != "" || it.Status != statusInProgress {
					return fail("a function_call without call_id, name or empty arguments")
				}
				item.CallID, item.Name = it.CallID, it.Name
			case "reasoning":
				if it.Summary == nil {
					return fail("a reasoning item without a summary")
				}
			default:
				return fail("an item of type %q", it.Type)
			}
			s.Items = append(s.Items, item)
			itemRaw = append(itemRaw, compact(f.ItemRaw))
			open = &s.Items[len(s.Items)-1]
			joined.Reset()
			step = 0
		case "response.content_part.added":
			if !here() || open.Type != "message" || step != 0 || f.ContentIndex == nil || *f.ContentIndex != 0 ||
				f.Part == nil || f.Part.Type != "output_text" || f.Part.Text == nil || *f.Part.Text != "" || f.Part.Annotations == nil {
				return fail("a content part that does not open the message with an empty output_text")
			}
			step = 1
		case "response.output_text.delta":
			if !here() || open.Type != "message" || step != 1 || f.ContentIndex == nil || *f.ContentIndex != 0 || f.Delta == nil {
				return fail("a text delta for a part that is not open")
			}
			open.Deltas = append(open.Deltas, *f.Delta)
			joined.WriteString(*f.Delta)
		case "response.output_text.done":
			if !here() || open.Type != "message" || step != 1 || f.ContentIndex == nil || *f.ContentIndex != 0 || f.Text == nil || *f.Text != joined.String() {
				return fail("a text that is not the joined deltas")
			}
			open.Text = *f.Text
			step = 2
		case "response.content_part.done":
			if !here() || open.Type != "message" || step != 2 || f.ContentIndex == nil || *f.ContentIndex != 0 ||
				f.Part == nil || f.Part.Type != "output_text" || f.Part.Text == nil || *f.Part.Text != open.Text || f.Part.Annotations == nil {
				return fail("a content part that is not the text that was streamed")
			}
			step = 3
		case "response.function_call_arguments.delta":
			if !here() || open.Type != "function_call" || step != 0 || f.Delta == nil {
				return fail("an argument delta for a call that is not open")
			}
			open.Deltas = append(open.Deltas, *f.Delta)
			joined.WriteString(*f.Delta)
		case "response.function_call_arguments.done":
			if !here() || open.Type != "function_call" || step != 0 || f.Arguments == nil || *f.Arguments != joined.String() || (f.Name != "" && f.Name != open.Name) {
				return fail("arguments that are not the joined deltas")
			}
			open.Arguments = *f.Arguments
			step = 3
		case "response.output_item.done":
			it := f.Item
			if open == nil || it == nil || it.ID != open.ID || it.Type != open.Type || f.OutputIndex == nil || *f.OutputIndex != len(s.Items)-1 {
				return fail("a done item that is not the open one")
			}
			switch open.Type {
			case "message":
				if step != 3 || it.Role != "assistant" || len(it.Content) != 1 || it.Content[0].Type != "output_text" ||
					it.Content[0].Text == nil || *it.Content[0].Text != open.Text || it.Content[0].Annotations == nil {
					return fail("a done message that is not the text that was streamed")
				}
			case "function_call":
				if step != 3 || it.CallID != open.CallID || it.Name != open.Name || it.Arguments == nil || *it.Arguments != open.Arguments {
					return fail("a done function_call that is not the one that was streamed")
				}
			case "reasoning":
				// Added and done at once, with the same item.
				if compact(f.ItemRaw) != itemRaw[len(itemRaw)-1] || it.Summary == nil {
					return fail("a done reasoning item that is not the one that was added")
				}
				for _, part := range it.Summary {
					if part.Type != "summary_text" || part.Text == nil {
						return fail("a reasoning summary that is no text")
					}
					open.Text += *part.Text
				}
			}
			if open.Type != "reasoning" {
				if it.Status != statusCompleted && it.Status != statusIncomplete {
					return fail("a done item with status %q", it.Status)
				}
				open.Status = it.Status
			}
			itemRaw[len(itemRaw)-1] = compact(f.ItemRaw)
			open = nil
		case "response.completed", "response.incomplete", "response.failed":
			r := f.Response
			if open != nil {
				return fail("%s while item %d is open", event, len(s.Items)-1)
			}
			if r == nil || "response."+r.Status != event || r.Object != "response" || r.ID != s.ID || r.Model != s.Model {
				return fail("%s without the response it ends", event)
			}
			if len(r.Output) != len(itemRaw) {
				return fail("%d items in the final output, %d were done", len(r.Output), len(itemRaw))
			}
			for i, o := range r.Output {
				if compact(o) != itemRaw[i] {
					return fail("item %d of the final output is not the item that was done", i)
				}
			}
			s.Status = r.Status
			if r.Status == statusFailed {
				if r.Error == nil || r.Error.Code == "" || r.Error.Message == nil || *r.Error.Message == "" {
					return fail("a failed response without an error code and message")
				}
				s.ErrCode, s.ErrMessage = r.Error.Code, *r.Error.Message
				ended = true
				continue
			}
			if !isNull(r.ErrorRaw) {
				return fail("an error in a response that did not fail")
			}
			u := r.Usage
			if u == nil || u.Input == nil || u.Output == nil || u.Total == nil || *u.Input < 0 || *u.Output < 0 || *u.Total != *u.Input+*u.Output {
				return fail("a response without a usage whose total is the sum")
			}
			s.InputTokens, s.OutputTokens, s.TotalTokens = *u.Input, *u.Output, *u.Total
			if r.Status == statusIncomplete {
				if r.Incomplete == nil || (r.Incomplete.Reason != "max_output_tokens" && r.Incomplete.Reason != "content_filter") {
					return fail("an incomplete response without a known reason")
				}
				s.IncompleteReason = r.Incomplete.Reason
			}
			for i, it := range s.Items {
				if it.Type != "reasoning" && it.Status != statusCompleted {
					return fail("item %d is %s in a response that did not fail", i, it.Status)
				}
				if it.Type == "function_call" && ir.CheckObject([]byte(it.Arguments)) != nil {
					return fail("item %d: the arguments are not one JSON object", i)
				}
			}
			s.Completed = r.Status == statusCompleted
			ended = true
		default:
			return fail("an event nobody knows: %q", event)
		}
	}
	if !ended {
		return Stream{}, errNotEnded
	}
	return s, nil
}

var errNotEnded = errors.New("the stream has no end")

// compact returns raw JSON without insignificant white space, "" when it is
// not JSON.
func compact(raw []byte) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return ""
	}
	return b.String()
}

// checkFrame is the data of any event, as far as CheckStream looks.
type checkFrame struct {
	Type         string          `json:"type"`
	Seq          *int            `json:"sequence_number"`
	OutputIndex  *int            `json:"output_index"`
	ContentIndex *int            `json:"content_index"`
	ItemID       string          `json:"item_id"`
	Delta        *string         `json:"delta"`
	Text         *string         `json:"text"`
	Name         string          `json:"name"`
	Arguments    *string         `json:"arguments"`
	Part         *checkPart      `json:"part"`
	Item         *checkItem      `json:"-"`
	ItemRaw      json.RawMessage `json:"item"`
	Response     *checkResponse  `json:"response"`
}

func (f *checkFrame) UnmarshalJSON(data []byte) error {
	type plain checkFrame
	if err := json.Unmarshal(data, (*plain)(f)); err != nil {
		return err
	}
	if len(f.ItemRaw) > 0 && !isNull(f.ItemRaw) {
		f.Item = &checkItem{}
		return json.Unmarshal(f.ItemRaw, f.Item)
	}
	return nil
}

type checkPart struct {
	Type        string          `json:"type"`
	Text        *string         `json:"text"`
	Annotations json.RawMessage `json:"annotations"`
}

type checkItem struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Role      string      `json:"role"`
	Status    string      `json:"status"`
	Content   []checkPart `json:"content"`
	CallID    string      `json:"call_id"`
	Name      string      `json:"name"`
	Arguments *string     `json:"arguments"`
	Summary   []checkPart `json:"summary"`
}

type checkResponse struct {
	ID       string            `json:"id"`
	Object   string            `json:"object"`
	Status   string            `json:"status"`
	Model    string            `json:"model"`
	Output   []json.RawMessage `json:"output"`
	ErrorRaw json.RawMessage   `json:"error"`
	Error    *struct {
		Code    string  `json:"code"`
		Message *string `json:"message"`
	} `json:"-"`
	Incomplete *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *struct {
		Input  *int `json:"input_tokens"`
		Output *int `json:"output_tokens"`
		Total  *int `json:"total_tokens"`
	} `json:"usage"`
}

func (r *checkResponse) UnmarshalJSON(data []byte) error {
	type plain checkResponse
	if err := json.Unmarshal(data, (*plain)(r)); err != nil {
		return err
	}
	if !isNull(r.ErrorRaw) {
		r.Error = &struct {
			Code    string  `json:"code"`
			Message *string `json:"message"`
		}{}
		return json.Unmarshal(r.ErrorRaw, r.Error)
	}
	return nil
}
