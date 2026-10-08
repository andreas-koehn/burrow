// Package irtest holds what only the tests of the format translators need:
// a reader of event sequences (Collect) and a judge of the pairing in a Chat
// Completions request (CheckPairing). They are shared by the tests of
// several packages, which a _test.go file cannot be. No package outside
// tests imports this one, so nothing of it is linked into the server.
package irtest

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// Collect folds a well-formed event sequence into a Response. It returns a
// *ir.StreamError for a sequence that holds an Error event (the event's
// message is in its field), ir.ErrSequence for one that is not well formed
// or has no Finish, ir.ErrLimit for one over the limits, and ir.ErrBadJSON
// for a tool call whose arguments do not add up to a JSON object.
func Collect(events []ir.Event) (ir.Response, error) {
	type state struct {
		open bool
		buf  []byte
	}
	var (
		resp     ir.Response
		parts    []ir.Part
		states   []state
		ids      = map[string]bool{}
		calls    int
		argBytes int
		started  bool
		finished bool
	)
	bad := func(what string) (ir.Response, error) {
		return ir.Response{}, fmt.Errorf("%w: %s", ir.ErrSequence, what)
	}
	// at returns the open part an event addresses.
	at := func(ev ir.Event, kind ir.PartKind) (*state, bool) {
		if ev.Index < 0 || ev.Index >= len(states) || !states[ev.Index].open {
			return nil, false
		}
		if kind != "" && parts[ev.Index].Kind != kind {
			return nil, false
		}
		return &states[ev.Index], true
	}
	for _, ev := range events {
		if finished {
			return bad("an event after the finish")
		}
		if ev.Kind == ir.Error {
			return ir.Response{}, &ir.StreamError{Message: ev.Err}
		}
		if started == (ev.Kind == ir.Start) {
			return bad("the sequence must begin with exactly one start")
		}
		switch ev.Kind {
		case ir.Start:
			started = true
			resp.ID, resp.Model, resp.Usage = ev.ID, ev.Model, ev.Usage
		case ir.PartStart:
			if ev.Index != len(parts) {
				return bad("a part's number is out of order")
			}
			if len(parts) >= ir.MaxParts {
				return ir.Response{}, fmt.Errorf("%w: more than %d parts", ir.ErrLimit, ir.MaxParts)
			}
			p := ir.Part{Kind: ev.Part.Kind}
			switch ev.Part.Kind {
			case ir.Text, ir.Thinking:
			case ir.ToolUse:
				if ev.Part.ToolID == "" || ev.Part.ToolName == "" || ids[ev.Part.ToolID] {
					return bad("a tool call needs a name and an id of its own")
				}
				if calls++; calls > ir.MaxToolCalls {
					return ir.Response{}, fmt.Errorf("%w: more than %d tool calls", ir.ErrLimit, ir.MaxToolCalls)
				}
				ids[ev.Part.ToolID] = true
				p.ToolID, p.ToolName = ev.Part.ToolID, ev.Part.ToolName
			default:
				return bad("a part of a kind an answer cannot hold")
			}
			parts = append(parts, p)
			states = append(states, state{open: true})
		case ir.TextDelta, ir.ThinkingDelta:
			kind := ir.Text
			if ev.Kind == ir.ThinkingDelta {
				kind = ir.Thinking
			}
			st, ok := at(ev, kind)
			if !ok {
				return bad("a text delta for a part that is not open")
			}
			st.buf = append(st.buf, ev.Text...)
		case ir.ToolArgsDelta:
			st, ok := at(ev, ir.ToolUse)
			if !ok {
				return bad("an argument delta for a tool call that is not open")
			}
			if len(st.buf)+len(ev.ArgsJSON) > ir.MaxToolArgsBytes {
				return ir.Response{}, fmt.Errorf("%w: tool arguments over %d bytes", ir.ErrLimit, ir.MaxToolArgsBytes)
			}
			if argBytes += len(ev.ArgsJSON); argBytes > ir.MaxTotalToolArgsBytes {
				return ir.Response{}, fmt.Errorf("%w: tool arguments over %d bytes in all", ir.ErrLimit, ir.MaxTotalToolArgsBytes)
			}
			st.buf = append(st.buf, ev.ArgsJSON...)
		case ir.PartStop:
			st, ok := at(ev, "")
			if !ok {
				return bad("a stop for a part that is not open")
			}
			st.open = false
			if parts[ev.Index].Kind != ir.ToolUse {
				parts[ev.Index].Text = string(st.buf)
				st.buf = nil
			}
		case ir.Finish:
			for i := range states {
				if states[i].open {
					return bad("the finish came while a part was open")
				}
				// Arguments are judged here, not at the part's stop: a stream
				// that fails stops its parts first, and is then an error of
				// the stream, whatever the arguments looked like by then.
				if parts[i].Kind == ir.ToolUse {
					input, err := ir.ToolInput(states[i].buf)
					if err != nil {
						return ir.Response{}, err
					}
					parts[i].Input = input
				}
			}
			finished = true
			resp.Stop = ev.Stop
			if ev.Usage.InputTokens == 0 {
				ev.Usage.InputTokens = resp.Usage.InputTokens
			}
			resp.Usage = ev.Usage
		default:
			return bad("an event of an unknown kind")
		}
	}
	if !finished {
		return bad("no finish")
	}
	resp.Parts = parts
	return resp, nil
}

// CheckPairing is the part of chat.CheckRequest that a request decoder's
// pairing of tool calls and tool results answers for, whatever a caller
// wrote into its messages. body is a Chat Completions request: every tool message follows the assistant message that holds
// its call, every call is answered there, and no assistant message follows
// another directly.
func CheckPairing(body []byte) error {
	var req struct {
		Messages []struct {
			Role      string `json:"role"`
			CallID    string `json:"tool_call_id"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errors.New("the body is not a JSON object with messages")
	}
	var waiting map[string]bool
	last := ""
	for i, m := range req.Messages {
		switch {
		case m.Role == "tool":
			if !waiting[m.CallID] {
				return fmt.Errorf("messages[%d]: a tool message for no waiting call", i)
			}
			delete(waiting, m.CallID)
		case len(waiting) > 0:
			return fmt.Errorf("messages[%d]: %s before every call was answered", i, m.Role)
		case m.Role == "assistant":
			if last == "assistant" {
				return fmt.Errorf("messages[%d]: two assistant messages in a row", i)
			}
			waiting = map[string]bool{}
			for _, c := range m.ToolCalls {
				if waiting[c.ID] {
					return fmt.Errorf("messages[%d]: one call id twice", i)
				}
				waiting[c.ID] = true
			}
		}
		last = m.Role
	}
	if len(waiting) > 0 {
		return errors.New("calls without an answer at the end")
	}
	return nil
}
