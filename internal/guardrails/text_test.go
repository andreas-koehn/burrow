package guardrails

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnswerText_PerFormat(t *testing.T) {
	cases := []struct {
		name, format, body, want string
	}{
		{"chat", FormatChat,
			`{"id":"chatcmpl-1","object":"chat.completion","model":"m-chat","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there."},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`,
			"Hello there."},
		{"chat two choices", FormatChat,
			`{"choices":[{"index":0,"message":{"content":"one"}},{"index":1,"message":{"content":"two"}}]}`,
			"one\ntwo"},
		{"chat content parts", FormatChat,
			`{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}]}`,
			"a\nb"},
		{"chat escaped", FormatChat,
			`{"choices":[{"message":{"content":"key: sk-abc \"q\" é"}}]}`,
			`key: sk-abc "q" é`},
		{"responses", FormatResponses,
			`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"reasoning","summary":[]},{"id":"msg_0","type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me look.","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":4}}`,
			"Let me look."},
		{"messages", FormatMessages,
			`{"id":"msg_01A","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"Hello there."},{"type":"tool_use","id":"t","name":"n","input":{}}],"stop_reason":"end_turn"}`,
			"Hello there."},
		// Tool calls are what the model wrote too: their arguments are read
		// (the strings of the JSON, keys included), after the text.
		{"chat tool call", FormatChat,
			`{"choices":[{"message":{"content":"Calling.","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{\"key\":\"sk-1\",\"n\":2,\"l\":[\"a\"]}"}}]}}]}`,
			"Calling.\nkey\nsk-1\nn\nl\na"},
		{"chat tool call, arguments not JSON", FormatChat,
			`{"choices":[{"message":{"content":null,"tool_calls":[{"function":{"name":"f","arguments":"not json sk-2"}}]}}]}`,
			"not json sk-2"},
		{"responses function call", FormatResponses,
			`{"output":[{"type":"function_call","call_id":"c","name":"f","arguments":"{\"q\":\"sk-3\"}"},{"type":"message","content":[{"type":"output_text","text":"Done."}]}]}`,
			"q\nsk-3\nDone."},
		{"messages tool use", FormatMessages,
			`{"content":[{"type":"text","text":"Let me."},{"type":"tool_use","id":"t","name":"n","input":{"text":"x","deep":{"k":"sk-4"}}}]}`,
			"Let me.\ntext\nx\ndeep\nk\nsk-4"},
		{"completions", FormatCompletions,
			`{"id":"cmpl-1","object":"text_completion","choices":[{"text":"Once upon","index":0},{"text":"a time","index":1}]}`,
			"Once upon\na time"},
	}
	for _, c := range cases {
		got, ok := AnswerText(c.format, []byte(c.body))
		if !ok || got != c.want {
			t.Errorf("%s: AnswerText = %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
}

func TestAnswerText_NoText(t *testing.T) {
	cases := []struct{ format, body string }{
		{FormatChat, `{"choices":[{"message":{"content":null,"tool_calls":[]}}]}`},
		{FormatChat, `not json`},
		{FormatChat, ``},
		{FormatResponses, `{"output":[]}`},
		{FormatMessages, `{"content":[{"type":"tool_use","id":"t","name":"n","input":{}}]}`},
		{FormatCompletions, `{"choices":[{"text":""}]}`},
		{"embeddings", `{"choices":[{"message":{"content":"x"}}]}`},
	}
	for _, c := range cases {
		if got, ok := AnswerText(c.format, []byte(c.body)); ok || got != "" {
			t.Errorf("AnswerText(%s, %s) = %q, %v; want \"\", false", c.format, c.body, got, ok)
		}
	}
}

func TestDeltaText_PerFormat(t *testing.T) {
	cases := []struct {
		name, format, frame, want string
	}{
		{"chat", FormatChat, `{"id":"chatcmpl-3","object":"chat.completion.chunk","model":"m-chat","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`, "Hel"},
		{"chat escaped", FormatChat, `{"choices":[{"delta":{"content":"sk-"}}]}`, "sk-"},
		{"responses", FormatResponses, `{"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_0","output_index":0,"content_index":0,"delta":"Let me look.","logprobs":[]}`, "Let me look."},
		{"messages", FormatMessages, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`, "Hello"},
		// The pieces of a tool call's arguments, as they stream (raw JSON).
		{"chat tool call", FormatChat, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"k\":\"sk-"}}]}}]}`, `{"k":"sk-`},
		{"responses arguments", FormatResponses, `{"type":"response.function_call_arguments.delta","delta":"{\"pa"}`, `{"pa`},
		{"messages input json", FormatMessages, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a"}}`, `{"a`},
		{"completions", FormatCompletions, `{"id":"cmpl-1","object":"text_completion","choices":[{"text":"Once","index":0}]}`, "Once"},
	}
	for _, c := range cases {
		got, ok := DeltaText(c.format, []byte(c.frame))
		if !ok || got != c.want {
			t.Errorf("%s: DeltaText = %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
}

func TestDeltaText_FramesWithoutText(t *testing.T) {
	cases := []struct{ format, frame string }{
		{FormatChat, `[DONE]`},
		{FormatChat, `{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`},
		{FormatChat, `{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}`},
		{FormatResponses, `{"type":"response.output_text.done","text":"Let me look."}`},
		{FormatResponses, `{"type":"response.completed","response":{"output":[{"content":[{"text":"x"}]}]}}`},
		{FormatMessages, `{"type":"ping"}`},
		{FormatMessages, `{"type":"message_stop"}`},
		{FormatMessages, `garbage`},
		{FormatChat, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t","function":{"name":"f","arguments":""}}]}}]}`},
		{FormatResponses, `{"type":"response.function_call_arguments.done","arguments":"{}"}`},
		{FormatCompletions, `[DONE]`},
	}
	for _, c := range cases {
		if got, ok := DeltaText(c.format, []byte(c.frame)); ok || got != "" {
			t.Errorf("DeltaText(%s, %s) = %q, %v; want \"\", false", c.format, c.frame, got, ok)
		}
	}
}

func TestRequestText_PerFormat(t *testing.T) {
	cases := []struct {
		name, format, body string
		want               []string
	}{
		{"chat", FormatChat,
			`{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":[{"type":"text","text":"Hi"},{"type":"image_url","image_url":{"url":"x"}}]}]}`,
			[]string{"Be brief.", "Hi"}},
		{"responses string", FormatResponses,
			`{"model":"m","instructions":"Rules.","input":"What is 2+2?"}`,
			[]string{"Rules.", "What is 2+2?"}},
		{"responses items", FormatResponses,
			`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"one"}]},{"role":"user","content":"two"}]}`,
			[]string{"one", "two"}},
		{"messages", FormatMessages,
			`{"model":"m","system":[{"type":"text","text":"House rule."}],"messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":[{"type":"text","text":"Hi!"}]}]}`,
			[]string{"House rule.", "Hello", "Hi!"}},
		{"messages string system", FormatMessages,
			`{"model":"m","system":"S","messages":[{"role":"user","content":"U"}]}`,
			[]string{"S", "U"}},
	}
	for _, c := range cases {
		got, ok := RequestText(c.format, []byte(c.body))
		if !ok || got != strings.Join(c.want, "\n") {
			t.Errorf("%s: RequestText = %q, %v; want %q", c.name, got, ok, strings.Join(c.want, "\n"))
		}
	}
	if got, ok := RequestText(FormatChat, []byte(`{"model":"m"}`)); ok || got != "" {
		t.Errorf("no messages: %q %v", got, ok)
	}
}

func TestRequestText_MediaEndpoints(t *testing.T) {
	cases := []struct {
		name, format, body string
		want               string
		ok                 bool
	}{
		{"images", FormatImages, `{"model":"m","prompt":"a red fox","n":1}`, "a red fox", true},
		{"speech", FormatSpeech, `{"model":"m","input":"Hello there","voice":"alloy"}`, "Hello there", true},
		{"rerank strings", FormatRerank, `{"model":"m","query":"q?","documents":["doc one","doc two"]}`, "q?\ndoc one\ndoc two", true},
		{"rerank objects", FormatRerank, `{"model":"m","query":"q?","documents":["doc one",{"text":"doc two"},{"title":"no text"}]}`, "q?\ndoc one\ndoc two", true},
		{"rerank no documents", FormatRerank, `{"model":"m","query":"q?"}`, "q?", true},
		{"images without prompt", FormatImages, `{"model":"m"}`, "", false},
		{"speech not json", FormatSpeech, `nope`, "", false},
		{"transcription is a form", FormatTranscription, "--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--B--\r\n", "", false},
	}
	for _, c := range cases {
		got, ok := RequestText(c.format, []byte(c.body))
		if got != c.want || ok != c.ok {
			t.Errorf("%s: RequestText = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// Streamed tool-call arguments are raw JSON: their simple escapes are
// decoded before the scan, so a key right after "\n" follows a line break,
// not the letter n. An escape split across two pieces is decoded too (the
// reader carries the backslash); text deltas are decoded by encoding/json
// already and are left alone.
func TestDeltaReader_DecodesArgumentEscapes(t *testing.T) {
	chat := func(piece string) []byte {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": piece}}}}}}})
		return b
	}
	responses := func(piece string) []byte {
		b, _ := json.Marshal(map[string]any{"type": "response.function_call_arguments.delta", "delta": piece})
		return b
	}
	messages := func(piece string) []byte {
		b, _ := json.Marshal(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "input_json_delta", "partial_json": piece}})
		return b
	}
	for name, frame := range map[string]func(string) []byte{FormatChat: chat, FormatResponses: responses, FormatMessages: messages} {
		cases := []struct {
			pieces, want []string
		}{
			{[]string{`{"c":"a\nb\tc\rd\"e\\f\/g"}`}, []string{"{\"c\":\"a\nb\tc\rd\"e\\f/g\"}"}},
			{[]string{`{"c":"x\`, `nAKIA`}, []string{`{"c":"x`, "\nAKIA"}},
			{[]string{`{"c":"x\\`, `nAKIA`}, []string{`{"c":"x\`, "nAKIA"}},
			{[]string{`{"c":"x\\\`, `t`}, []string{`{"c":"x\`, "\t"}},
			{[]string{`\`, `"q`}, []string{"", `"q`}},
			{[]string{`{"c":"A"}`}, []string{`{"c":"A"}`}},
		}
		for _, c := range cases {
			r := NewDeltaReader(name)
			for i, p := range c.pieces {
				got, _ := r.Text(frame(p))
				if got != c.want[i] {
					t.Errorf("%s %q piece %d: %q, want %q", name, c.pieces, i, got, c.want[i])
				}
			}
		}
	}
	// A text delta is not decoded a second time.
	r := NewDeltaReader(FormatChat)
	if got, _ := r.Text([]byte(`{"choices":[{"delta":{"content":"a\\nb"}}]}`)); got != `a\nb` {
		t.Errorf("text delta: %q", got)
	}
	if got, _ := DeltaText(FormatMessages, messages(`{"k":"a\nb`)); got != "{\"k\":\"a\nb" {
		t.Errorf("DeltaText: %q", got)
	}
}
