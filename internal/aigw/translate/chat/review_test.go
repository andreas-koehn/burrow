package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

func TestStreamEncoder_ACallWithoutArgumentsWaitsForWhatFollows(t *testing.T) {
	// A decoder stops every open part before it reports a failure, so a call that stops without
	// a byte of arguments may be one that was cut. Its "{}" is written only when the next event
	// shows that the stream goes on; the stop of another part does not show it.
	for name, events := range map[string][]ir.Event{
		"stopped, then the Error":        {startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), {Kind: ir.Error, Err: "gone"}},
		"stopped, then Close":            {startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0)},
		"open, then Close":               {startEv(), toolStart(0, "a", "f")},
		"beside a whole call":            {startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), argsDelta(1, `{"k":1}`), ev(ir.PartStop, 0), ev(ir.PartStop, 1), {Kind: ir.Error, Err: "gone"}},
		"after a whole call":             {startEv(), toolStart(0, "b", "g"), argsDelta(0, `{"k":1}`), ev(ir.PartStop, 0), toolStart(1, "a", "f"), ev(ir.PartStop, 1), {Kind: ir.Error, Err: "gone"}},
		"two of them":                    {startEv(), toolStart(0, "a", "f"), toolStart(1, "a2", "f"), ev(ir.PartStop, 0), ev(ir.PartStop, 1), {Kind: ir.Error, Err: "gone"}},
		"a sequence the encoder refuses": {startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), ev(ir.PartStop, 0)},
	} {
		raw, _ := encodeEvents(events, false, true)
		s, err := CheckStream(raw)
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, raw)
		}
		if s.Done || s.FinishReason != "" || s.ErrMessage == "" || strings.Contains(string(raw), `"arguments":"{}"`) {
			t.Errorf("%s: arguments were made up, or the stream did not fail: %+v\n%s", name, s, raw)
		}
		for _, c := range s.Calls {
			if strings.HasPrefix(c.ID, "a") && c.Arguments != "" || c.ID == "b" && c.Arguments != `{"k":1}` {
				t.Errorf("%s: call %+v", name, c)
			}
		}
	}
	// What shows that the stream goes on: a part that starts, a delta, the Finish.
	for name, events := range map[string][]ir.Event{
		"the Finish":             {startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), finishEv(ir.StopToolUse, 1, 1)},
		"a part starts":          {startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0), textStart(1), textDelta(1, "x"), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1)},
		"the Finish, for two":    {startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), ev(ir.PartStop, 0), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1)},
		"a delta of another one": {startEv(), toolStart(0, "a", "f"), toolStart(1, "b", "g"), ev(ir.PartStop, 0), argsDelta(1, `{}`), ev(ir.PartStop, 1), finishEv(ir.StopToolUse, 1, 1)},
	} {
		s, raw := mustStream(t, events, true, true)
		if !s.Done || len(s.Calls) == 0 {
			t.Fatalf("%s: %+v\n%s", name, s, raw)
		}
		for _, c := range s.Calls {
			if c.Arguments != "{}" || len(c.Deltas) != 1 {
				t.Errorf("%s: call %+v\n%s", name, c, raw)
			}
		}
	}
	// The "{}" is written as soon as the stream goes on, not at the finish.
	var out strings.Builder
	enc := NewStreamEncoder(&out, "m", frozen(), false)
	for _, event := range []ir.Event{startEv(), toolStart(0, "a", "f"), ev(ir.PartStop, 0)} {
		if err := enc.Write(event); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(out.String(), `"arguments":"{}"`) {
		t.Fatalf("arguments were written before anything showed the stream goes on\n%s", out.String())
	}
	if err := enc.Write(textStart(1)); err != nil || !strings.Contains(out.String(), `"arguments":"{}"`) {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestStreamEncoder_AFailedWriteOfTheErrorObjectIsTheWritersError(t *testing.T) {
	// The encoder refuses an event and ends the stream itself. When the client is gone by then,
	// that is what the caller has to learn: ErrSequence or ErrLimit would say "the answer was
	// ended with the error object", and the upstream would be read to its end for nobody.
	for name, c := range map[string]struct {
		events []ir.Event
		writes int // the writes that succeed
	}{
		"a sequence error": {[]ir.Event{startEv(), textDelta(3, "x")}, 1},
		"a limit":          {[]ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, strings.Repeat("x", ir.MaxToolArgsBytes+1))}, 2},
	} {
		enc := NewStreamEncoder(&failingWriter{after: c.writes}, "m", frozen(), false)
		var err error
		for _, event := range c.events {
			err = enc.Write(event)
		}
		if err == nil || err.Error() != "client gone" || errors.Is(err, ir.ErrSequence) || errors.Is(err, ir.ErrLimit) {
			t.Errorf("%s: err = %v, want the writer's", name, err)
		}
		if cerr := enc.Close(); cerr == nil || cerr.Error() != "client gone" {
			t.Errorf("%s: Close = %v", name, cerr)
		}
	}
	// With a writer that takes the frame, the encoder's own error stands.
	enc := NewStreamEncoder(&failingWriter{after: 9}, "m", frozen(), false)
	_ = enc.Write(startEv())
	if err := enc.Write(textDelta(3, "x")); !errors.Is(err, ir.ErrSequence) {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamDecoder_AToolNameSentAgain(t *testing.T) {
	// OpenAI-compatible servers send a call's name whole in its first chunk; some send the same
	// name again with later chunks. That is one name.
	d := NewStreamDecoder()
	got, err := feedAll(d,
		chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"get_weather","arguments":"{"}}]}`),
		chunk(`{"tool_calls":[{"index":0,"function":{"name":"get_weather","arguments":"\"k\":1"}}]}`),
		chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"get_weather","arguments":"}"}}]}`),
		chunk(`{"tool_calls":[{"index":0,"function":{"name":"","arguments":" "}}]}`),
		finishChunk("tool_calls"), []byte("[DONE]"))
	if err != nil || kinds(got) != "start part_start tool_args_delta tool_args_delta tool_args_delta tool_args_delta part_stop finish" || got[1].Part.ToolName != "get_weather" {
		t.Fatalf("%v: %s, %+v", err, kinds(got), got)
	}
	// A name that goes on in a later chunk would be kept by its first piece only, and the call
	// would be handed over under a name the model did not say: the answer cannot be read.
	for name, frames := range map[string][][]byte{
		"a name in pieces": {
			chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"get_","arguments":""}}]}`),
			chunk(`{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"{}"}}]}`),
			finishChunk("tool_calls"), []byte("[DONE]")},
		"another name before the id came": {
			chunk(`{"tool_calls":[{"index":0,"function":{"name":"f","arguments":""}}]}`),
			chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"g","arguments":"{}"}}]}`),
			finishChunk("tool_calls"), []byte("[DONE]")},
		"another name beside an open call": {
			chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}},{"index":1,"id":"b","function":{"name":"g","arguments":"{}"}}]}`),
			chunk(`{"tool_calls":[{"index":1,"function":{"name":"h"}}]}`),
			finishChunk("tool_calls"), []byte("[DONE]")},
	} {
		d := NewStreamDecoder()
		got, err := feedAll(d, frames...)
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("%s: err = %v, %s", name, err, kinds(got))
		}
		// The usual failure: a stop for every open part, in part order, then exactly one Error.
		open, n := map[int]bool{}, 0
		for i, e := range got {
			switch e.Kind {
			case ir.PartStart:
				open[e.Index] = true
			case ir.PartStop:
				delete(open, e.Index)
			case ir.Error:
				if n++; i != len(got)-1 || len(open) != 0 {
					t.Errorf("%s: the Error is not last, or parts are open: %s", name, kinds(got))
				}
			case ir.Finish:
				t.Errorf("%s: a Finish: %s", name, kinds(got))
			}
		}
		if n != 1 || len(d.Close()) != 0 {
			t.Errorf("%s: %d Error events: %s", name, n, kinds(got))
		}
	}
}

func TestDecodeRequest_TheCallLimitCountsTheCallsThatAreKept(t *testing.T) {
	call := func(id string) string {
		return `{"id":"` + id + `","type":"function","function":{"name":"f","arguments":"{}"}}`
	}
	turn := func(calls []string) string {
		return chatReq(hi+`,{"role":"assistant","content":null,"tool_calls":[`+strings.Join(calls, ",")+`]}`, "")
	}
	var calls []string
	for i := 0; i < ir.MaxToolCalls; i++ {
		calls = append(calls, call(fmt.Sprint("t", i)))
	}
	// 64 calls, some of their ids once more (left out as duplicates) and calls of a tool that is
	// not a function (left out): 64 calls are kept, and that is what the limit is about.
	repeated := append(append(append([]string{}, calls...), calls[:10]...), `{"id":"c","type":"custom","custom":{"name":"x","input":"y"}}`)
	req := decodeReq(t, turn(repeated))
	kept := 0
	for _, p := range req.Messages[1].Parts {
		if p.Kind == ir.ToolUse {
			kept++
		}
	}
	if kept != ir.MaxToolCalls || !reflect.DeepEqual(sorted(req.Dropped), []string{"input:custom", "input:tool_call.duplicate", "input:tool_call.unanswered"}) {
		t.Fatalf("%d calls, dropped %v", kept, sorted(req.Dropped))
	}
	// One call more that is kept is over the limit.
	bad := refusedReq(t, turn(append(append([]string{}, calls...), call("one-more"))), "messages[1].tool_calls")
	if !bad.Limit || !errors.Is(bad, ir.ErrLimit) {
		t.Fatalf("not a limit error: %v", bad)
	}
	// The list itself stays bounded.
	bad = refusedReq(t, turn(strings.Split(strings.TrimSuffix(strings.Repeat(call("same")+"|", ir.MaxParts+1), "|"), "|")), "messages[1].tool_calls")
	if !bad.Limit {
		t.Fatalf("not a limit error: %v", bad)
	}
}

func TestEncodeRequest_HistoryToolNamesAServerWouldRefuse(t *testing.T) {
	// A call in the history under a name that is no function name for an OpenAI-compatible
	// server (^[a-zA-Z0-9_-]{1,64}$) and that is not one of the declared tools: a strict server
	// answers 400 to every replay. It is written with the characters such a server takes, cut to
	// 64, and that is reported — as the Messages target does.
	long := strings.Repeat("n", 70)
	req := ir.Request{
		Messages: []ir.Message{user(text("hi")),
			assistant(toolUse("a", "ok_name-1", `{}`), toolUse("b", "ns.tool/call me", `{}`), toolUse("c", long, `{}`), toolUse("d", "wetter-für", `{}`)),
			user(toolResult("a", "1"), toolResult("b", "2"), toolResult("c", "3"), toolResult("d", "4"))},
	}
	top, dropped := encode(t, req)
	var messages []struct {
		ToolCalls []struct {
			Function struct{ Name string }
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(top["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range messages[1].ToolCalls {
		names = append(names, c.Function.Name)
	}
	if want := []string{"ok_name-1", "ns_tool_call_me", long[:64], "wetter-f__r"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}
	if !reflect.DeepEqual(dropped, []string{"input:tool_use.name"}) {
		t.Fatalf("dropped = %v", dropped)
	}
	// Names a server takes are not reported.
	req.Messages[1] = assistant(toolUse("a", "ok_name-1", `{}`), toolUse("b", strings.Repeat("n", 64), `{}`))
	req.Messages[2] = user(toolResult("a", "1"), toolResult("b", "2"))
	if _, dropped = encode(t, req); len(dropped) != 0 {
		t.Fatalf("dropped = %v", dropped)
	}
	// A declared tool is not renamed, here or in the history: the call and its declaration stay
	// the same name, and what the server makes of that name is the server's answer.
	req.Tools = []ir.Tool{{Name: "ns.tool"}}
	req.Messages[1] = assistant(toolUse("a", "ns.tool", `{}`), toolUse("b", "ns.gone", `{}`))
	top, dropped = encode(t, req)
	if s := string(top["messages"]) + string(top["tools"]); strings.Count(s, `"name":"ns.tool"`) != 2 || !strings.Contains(s, `"name":"ns_gone"`) || !reflect.DeepEqual(dropped, []string{"input:tool_use.name"}) {
		t.Fatalf("%s, dropped %v", s, dropped)
	}
	// A declared tool without a name stays a 400 that names the field.
	req.Tools = []ir.Tool{{Name: ""}}
	if _, _, err := EncodeRequest(req, "m"); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "tools[0].name") {
		t.Fatalf("err = %v", err)
	}
}
