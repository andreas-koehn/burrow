package guardrails

import (
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
		{FormatMessages, `{"content":[{"type":"tool_use","id":"t","name":"n","input":{"text":"x"}}]}`},
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
		{FormatChat, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`},
		{FormatResponses, `{"type":"response.output_text.done","text":"Let me look."}`},
		{FormatResponses, `{"type":"response.function_call_arguments.delta","delta":"{\"pa"}`},
		{FormatResponses, `{"type":"response.completed","response":{"output":[{"content":[{"text":"x"}]}]}}`},
		{FormatMessages, `{"type":"ping"}`},
		{FormatMessages, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a"}}`},
		{FormatMessages, `{"type":"message_stop"}`},
		{FormatMessages, `garbage`},
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
