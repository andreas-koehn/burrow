package irtest

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// toolsEvents is the event sequence chat/testdata/stream_tools.sse decodes to.
func toolsEvents() []ir.Event {
	return []ir.Event{
		{Kind: ir.Start, ID: "chatcmpl-4", Model: "m-chat"},
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Text}},
		{Kind: ir.TextDelta, Index: 0, Text: "Let me look."},
		{Kind: ir.PartStop, Index: 0},
		{Kind: ir.PartStart, Index: 1, Part: ir.Part{Kind: ir.ToolUse, ToolID: "call_a", ToolName: "read_file"}},
		{Kind: ir.ToolArgsDelta, Index: 1, ArgsJSON: `{"pa`},
		{Kind: ir.PartStart, Index: 2, Part: ir.Part{Kind: ir.ToolUse, ToolID: "call_b", ToolName: "list_dir"}},
		{Kind: ir.ToolArgsDelta, Index: 2, ArgsJSON: `{"dir":`},
		{Kind: ir.ToolArgsDelta, Index: 1, ArgsJSON: `th":"a.txt"}`},
		{Kind: ir.ToolArgsDelta, Index: 2, ArgsJSON: `"."}`},
		{Kind: ir.PartStop, Index: 1},
		{Kind: ir.PartStop, Index: 2},
		{Kind: ir.Finish, Stop: ir.StopToolUse, Usage: ir.Usage{InputTokens: 50, OutputTokens: 21}},
	}
}

func TestCollect(t *testing.T) {
	got, err := Collect(toolsEvents())
	if err != nil {
		t.Fatal(err)
	}
	want := ir.Response{
		ID: "chatcmpl-4", Model: "m-chat",
		Parts: []ir.Part{
			{Kind: ir.Text, Text: "Let me look."},
			{Kind: ir.ToolUse, ToolID: "call_a", ToolName: "read_file", Input: json.RawMessage(`{"path":"a.txt"}`)},
			{Kind: ir.ToolUse, ToolID: "call_b", ToolName: "list_dir", Input: json.RawMessage(`{"dir":"."}`)},
		},
		Stop:  ir.StopToolUse,
		Usage: ir.Usage{InputTokens: 50, OutputTokens: 21},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Collect = %+v", got)
	}
}

func TestCollect_ThinkingUsageAndEmptyInput(t *testing.T) {
	got, err := Collect([]ir.Event{
		{Kind: ir.Start, ID: "i", Model: "m", Usage: ir.Usage{InputTokens: 7}},
		{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Thinking}},
		{Kind: ir.ThinkingDelta, Index: 0, Text: "hm"},
		{Kind: ir.ThinkingDelta, Index: 0, Text: "m"},
		{Kind: ir.PartStop, Index: 0},
		{Kind: ir.PartStart, Index: 1, Part: ir.Part{Kind: ir.ToolUse, ToolID: "c", ToolName: "now"}},
		{Kind: ir.PartStop, Index: 1},
		{Kind: ir.Finish, Stop: ir.StopToolUse, Usage: ir.Usage{OutputTokens: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := ir.Response{ID: "i", Model: "m", Stop: ir.StopToolUse, Usage: ir.Usage{InputTokens: 7, OutputTokens: 4},
		Parts: []ir.Part{{Kind: ir.Thinking, Text: "hmm"}, {Kind: ir.ToolUse, ToolID: "c", ToolName: "now", Input: json.RawMessage(`{}`)}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Collect = %+v", got)
	}
}

func TestCollect_Errors(t *testing.T) {
	start := ir.Event{Kind: ir.Start}
	text := ir.Event{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.Text}}
	tool := ir.Event{Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.ToolUse, ToolID: "c", ToolName: "f"}}
	stop0 := ir.Event{Kind: ir.PartStop, Index: 0}
	fin := ir.Event{Kind: ir.Finish, Stop: ir.StopEnd}
	cases := map[string][]ir.Event{
		"error event":             {start, text, {Kind: ir.Error, Err: "overloaded"}},
		"stop without a start":    {start, stop0, fin},
		"stop twice":              {start, text, stop0, stop0, fin},
		"arguments not JSON":      {start, tool, {Kind: ir.ToolArgsDelta, Index: 0, ArgsJSON: `{not json`}, stop0, fin},
		"arguments not an object": {start, tool, {Kind: ir.ToolArgsDelta, Index: 0, ArgsJSON: `[1]`}, stop0, fin},
		"no start":                {text, stop0, fin},
		"start twice":             {start, start, fin},
		"no finish":               {start, text, stop0},
		"empty":                   {},
		"event after finish":      {start, fin, text},
		"part left open":          {start, text, fin},
		"index skips":             {start, {Kind: ir.PartStart, Index: 5, Part: ir.Part{Kind: ir.Text}}, fin},
		"absurd index":            {start, {Kind: ir.TextDelta, Index: 1_000_000_000, Text: "x"}, fin},
		"negative index":          {start, {Kind: ir.PartStop, Index: -1}, fin},
		"delta of the wrong kind": {start, text, {Kind: ir.ToolArgsDelta, Index: 0, ArgsJSON: "{}"}, stop0, fin},
		"delta after stop":        {start, text, stop0, {Kind: ir.TextDelta, Index: 0, Text: "x"}, fin},
		"part of a request kind":  {start, {Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.ToolResult}}, stop0, fin},
		"tool without a name":     {start, {Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.ToolUse, ToolID: "c"}}, stop0, fin},
		"tool without an id":      {start, {Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.ToolUse, ToolName: "f"}}, stop0, fin},
		"unknown event kind":      {start, {Kind: "weird"}, fin},
	}
	for name, evs := range cases {
		if _, err := Collect(evs); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	_, err := Collect(cases["error event"])
	// The provider's message is in the typed error's field, for the caller; the error's text is fixed.
	var se *ir.StreamError
	if !errors.Is(err, ir.ErrStream) || !errors.As(err, &se) || se.Message != "overloaded" || strings.Contains(err.Error(), "overloaded") || err.Error() != ir.ErrStream.Error() {
		t.Fatalf("error event: %v (%+v)", err, se)
	}
}

func TestCollect_Limits(t *testing.T) {
	evs := []ir.Event{{Kind: ir.Start}}
	for i := 0; i <= ir.MaxToolCalls; i++ {
		evs = append(evs,
			ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.ToolUse, ToolID: "c" + string(rune('0'+i%10)) + string(rune('a'+i/10)), ToolName: "f"}},
			ir.Event{Kind: ir.PartStop, Index: i})
	}
	evs = append(evs, ir.Event{Kind: ir.Finish})
	if _, err := Collect(evs); !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("%d tool calls: %v", ir.MaxToolCalls+1, err)
	}

	evs = []ir.Event{{Kind: ir.Start}, {Kind: ir.PartStart, Index: 0, Part: ir.Part{Kind: ir.ToolUse, ToolID: "c", ToolName: "f"}}}
	frag := strings.Repeat(" ", 4096)
	for n := 0; n <= ir.MaxToolArgsBytes; n += len(frag) {
		evs = append(evs, ir.Event{Kind: ir.ToolArgsDelta, Index: 0, ArgsJSON: frag})
	}
	evs = append(evs, ir.Event{Kind: ir.PartStop, Index: 0}, ir.Event{Kind: ir.Finish})
	if _, err := Collect(evs); !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("large arguments: %v", err)
	}

	evs = []ir.Event{{Kind: ir.Start}}
	for i := 0; i <= ir.MaxParts; i++ {
		evs = append(evs, ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.Text}}, ir.Event{Kind: ir.PartStop, Index: i})
	}
	evs = append(evs, ir.Event{Kind: ir.Finish})
	if _, err := Collect(evs); !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("%d parts: %v", ir.MaxParts+1, err)
	}
}

func TestCollect_TotalArgumentsLimit(t *testing.T) {
	evs := []ir.Event{{Kind: ir.Start}}
	piece := `{"a":"` + strings.Repeat("x", ir.MaxToolArgsBytes-16) + `"}`
	for i := 0; i*len(piece) <= ir.MaxTotalToolArgsBytes; i++ {
		evs = append(evs,
			ir.Event{Kind: ir.PartStart, Index: i, Part: ir.Part{Kind: ir.ToolUse, ToolID: string(rune('a' + i)), ToolName: "f"}},
			ir.Event{Kind: ir.ToolArgsDelta, Index: i, ArgsJSON: piece},
			ir.Event{Kind: ir.PartStop, Index: i})
	}
	evs = append(evs, ir.Event{Kind: ir.Finish, Stop: ir.StopToolUse})
	if _, err := Collect(evs); !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Collect(append(evs[:10:10], ir.Event{Kind: ir.Finish, Stop: ir.StopToolUse})); err != nil {
		t.Fatalf("three calls: %v", err)
	}
}
