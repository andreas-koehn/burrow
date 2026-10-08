package messages

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

func TestStreamEncoder_ACallWithoutArgumentsWaitsForWhatFollows(t *testing.T) {
	// A decoder stops every open part before it reports a failure, so a call that stops without
	// a byte of arguments may be one that was cut. Its "{}" is written only when the next event
	// shows that the stream goes on; the stop of another part does not show it.
	failed := "message_start content_block_start content_block_stop error"
	for name, c := range map[string]struct {
		events []ir.Event
		names  string
		whole  bool // the call "b" is handed over with its arguments
	}{
		"stopped, then the Error": {[]ir.Event{startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), {Kind: ir.Error, Err: "gone"}}, failed, false},
		"stopped, then Close":     {[]ir.Event{startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0)}, failed, false},
		"open, then the Error":    {[]ir.Event{startEv(), toolStart(0, "a", "f"), {Kind: ir.Error, Err: "gone"}}, failed, false},
		"open, then Close":        {[]ir.Event{startEv(), toolStart(0, "a", "f")}, failed, false},
		"beside a whole call that is held": {[]ir.Event{startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), argsDelta(1, `{"k":1}`),
			ev(ir.PartStop, 0), ev(ir.PartStop, 1), {Kind: ir.Error, Err: "gone"}},
			"message_start content_block_start content_block_stop content_block_start content_block_delta content_block_stop error", true},
		"after a whole call": {[]ir.Event{startEv(), toolStart(0, "b", "g"), argsDelta(0, `{"k":1}`), toolStart(1, "a", "f"),
			ev(ir.PartStop, 0), ev(ir.PartStop, 1), {Kind: ir.Error, Err: "gone"}},
			"message_start content_block_start content_block_delta content_block_stop content_block_start content_block_stop error", true},
		"held back: its block is written and closed like the open one's": {[]ir.Event{startEv(), textStart(0), textDelta(0, "x"), toolStart(1, "a", "f"), {Kind: ir.Error, Err: "gone"}},
			"message_start content_block_start content_block_delta content_block_stop content_block_start content_block_stop error", false},
		"a sequence the encoder refuses": {[]ir.Event{startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), ev(ir.PartStop, 0)}, failed, false},
	} {
		raw, _ := encodeEvents(c.events, true)
		s, err := CheckStream(raw)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, raw)
		}
		if got := eventNames(raw); got != c.names {
			t.Errorf("%s: events = %s\nwant     %s", name, got, c.names)
		}
		if s.Stopped || s.ErrType != "api_error" || strings.Contains(string(raw), `"partial_json":"{}"`) {
			t.Errorf("%s: arguments were made up, or the stream did not fail: %+v\n%s", name, s, raw)
		}
		whole := false
		for _, b := range s.Blocks {
			if b.ToolID == "a" && len(b.Deltas) > 0 {
				t.Errorf("%s: block %+v", name, b)
			}
			whole = whole || b.ToolID == "b" && b.PartialJSON == `{"k":1}`
		}
		if whole != c.whole {
			t.Errorf("%s: the whole call is handed over: %v\n%s", name, whole, raw)
		}
	}
	// What shows that the stream goes on: a part that starts, a delta, the Finish.
	for name, events := range map[string][]ir.Event{
		"the Finish":                         {startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), finishEv(ir.StopToolUse, 1, 1)},
		"a part starts":                      {startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), textStart(1), textDelta(1, "x"), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1)},
		"the Finish, a held one stopped too": {startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), ev(ir.PartStop, 0), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1)},
		"a delta of a held one": {startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), ev(ir.PartStop, 0), argsDelta(1, `{"k":1}`), ev(ir.PartStop, 1),
			finishEv(ir.StopToolUse, 1, 1)},
	} {
		s, raw := mustStream(t, events, true)
		if !s.Stopped || len(s.Blocks) == 0 || !reflect.DeepEqual(s.Blocks[0].Deltas, []string{"{}"}) || s.Blocks[0].Input != "{}" {
			t.Errorf("%s: %+v\n%s", name, s, raw)
		}
		for _, b := range s.Blocks[1:] {
			if b.Type == "tool_use" && b.Input != "{}" && b.Input != `{"k":1}` {
				t.Errorf("%s: block %+v", name, b)
			}
		}
	}
	// The "{}" is written as soon as the stream goes on, not at the finish.
	var out strings.Builder
	e := NewStreamEncoder(&out, "m")
	e.now = frozen
	for _, event := range []ir.Event{startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0)} {
		if err := e.Write(event); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(out.String(), "content_block_stop") || strings.Contains(out.String(), "partial_json") {
		t.Fatalf("the call was closed before anything showed the stream goes on\n%s", out.String())
	}
	if err := e.Write(textStart(1)); err != nil || !strings.Contains(out.String(), `"partial_json":"{}"`) || !strings.Contains(out.String(), "content_block_stop") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestStreamEncoder_AFailedWriteOfTheErrorEventIsTheWritersError(t *testing.T) {
	// The encoder refuses an event and ends the stream itself. When the client is gone by then,
	// that is what the caller has to learn: ErrSequence or ErrLimit would say "the answer was
	// ended with an error event", and the upstream would be read to its end for nobody.
	for name, c := range map[string]struct {
		events []ir.Event
		writes int // the writes that succeed
	}{
		"a sequence error":                {[]ir.Event{startEv(), textDelta(3, "x")}, 1},
		"a limit":                         {[]ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, strings.Repeat("x", ir.MaxToolArgsBytes+1))}, 2},
		"a limit, the block's stop fails": {[]ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, "{"), argsDelta(0, strings.Repeat("x", ir.MaxToolArgsBytes))}, 3},
	} {
		e := NewStreamEncoder(&failingWriter{after: c.writes}, "m")
		e.now = frozen
		var err error
		for _, event := range c.events {
			err = e.Write(event)
		}
		if err == nil || err.Error() != "client went away" || errors.Is(err, ir.ErrSequence) || errors.Is(err, ir.ErrLimit) {
			t.Errorf("%s: err = %v, want the writer's", name, err)
		}
		if cerr := e.Close(); cerr == nil || cerr.Error() != "client went away" {
			t.Errorf("%s: Close = %v", name, cerr)
		}
	}
	// With a writer that takes the frame, the encoder's own error stands.
	e := NewStreamEncoder(&failingWriter{after: 9}, "m")
	_ = e.Write(startEv())
	if err := e.Write(textDelta(3, "x")); !errors.Is(err, ir.ErrSequence) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRequest_AHistoryInputThatIsNoObjectIsReadAsEmpty(t *testing.T) {
	// The history is the client's record of an answer. An input that is not a JSON object would
	// answer 400 on every replay and end the session: the call stays, without arguments, and that
	// is reported — as the Chat and the Responses decoders do.
	for _, input := range []string{`[]`, `"text"`, `5`, `true`, `[{"a":1}]`} {
		req := decode(t, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":`+input+`},{"type":"tool_use","id":"u","name":"g","input":{"k":1}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"r"},{"type":"tool_result","tool_use_id":"u","content":"s"}]}]}`)
		want := []ir.Part{
			{Kind: ir.ToolUse, ToolID: "t", ToolName: "f", Input: json.RawMessage(`{}`)},
			{Kind: ir.ToolUse, ToolID: "u", ToolName: "g", Input: json.RawMessage(`{"k":1}`)},
		}
		if !reflect.DeepEqual(req.Messages[1].Parts, want) || !reflect.DeepEqual(ir.Dropped(req.Dropped), []string{"input:tool_use.arguments"}) {
			t.Errorf("input %s: parts %+v, dropped %v", input, req.Messages[1].Parts, req.Dropped)
		}
	}
	// An input that is an object, or absent, is not reported.
	req := decode(t, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"r"}]}]}`)
	if len(req.Dropped) != 0 {
		t.Fatalf("dropped %v", req.Dropped)
	}
	// Over a limit it stays a 400: such an input is not a record of anything this gateway sent.
	bad := refused(t, withContent("assistant", `{"type":"tool_use","id":"t","name":"f","input":["`+strings.Repeat("x", ir.MaxToolArgsBytes)+`"]}`), "messages[0].content[0].input")
	if !bad.Limit || !errors.Is(bad, ir.ErrLimit) {
		t.Fatalf("not a limit error: %v", bad)
	}
}

func TestDecodeRequest_TheCallLimitCountsTheCallsThatAreKept(t *testing.T) {
	use := func(id string) string { return `{"type":"tool_use","id":"` + id + `","name":"f","input":{}}` }
	turn := func(blocks []string) string {
		return `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]}]}`
	}
	var blocks []string
	for i := 0; i < ir.MaxToolCalls; i++ {
		blocks = append(blocks, use(fmt.Sprint("t", i)))
	}
	// 64 calls, the same ids once more (left out as duplicates) and blocks of a tool that is not
	// emulated (left out): 64 calls are kept, and that is what the limit is about.
	repeated := append(append(append([]string{}, blocks...), blocks[:10]...), `{"type":"server_tool_use","id":"s","name":"web_search","input":{}}`)
	req := decode(t, turn(repeated))
	calls := 0
	for _, p := range req.Messages[1].Parts {
		if p.Kind == ir.ToolUse {
			calls++
		}
	}
	if calls != ir.MaxToolCalls || !reflect.DeepEqual(ir.Dropped(req.Dropped), []string{"input:server_tool_use", "input:tool_use.duplicate", "input:tool_use.unanswered"}) {
		t.Fatalf("%d calls, dropped %v", calls, ir.Dropped(req.Dropped))
	}
	// One call more that is kept is over the limit.
	bad := refused(t, turn(append(append([]string{}, blocks...), use("one-more"))), "messages[1].content")
	if !bad.Limit || !errors.Is(bad, ir.ErrLimit) {
		t.Fatalf("not a limit error: %v", bad)
	}
	// What the decoder takes, the Chat target takes: a turn never holds more calls than that.
	if _, _, err := chat.EncodeRequest(req, "m"); err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
}
