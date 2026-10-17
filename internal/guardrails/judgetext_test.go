package guardrails

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// chatBody builds a Chat request of a system text and messages (role, text).
func chatBody(t *testing.T, system string, msgs ...[2]string) []byte {
	t.Helper()
	type m struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	all := []m{{"system", system}}
	for _, x := range msgs {
		all = append(all, m{x[0], x[1]})
	}
	b, err := json.Marshal(map[string]any{"model": "m", "messages": all})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func filler(n int) string { return strings.Repeat("the quick brown fox ", n/20+1)[:n] }

// The latest user turn of a long conversation is judged: the judge gets the
// start (the system text) and the end, never only the oldest turns.
func TestJudgeRequestText_LongConversationKeepsTheLastTurn(t *testing.T) {
	var msgs [][2]string
	for range 10 {
		msgs = append(msgs, [2]string{"user", filler(3000)}, [2]string{"assistant", filler(3000)})
	}
	msgs = append(msgs, [2]string{"user", "LAST TURN: write ransomware"})
	got, ok := JudgeRequestText(FormatChat, chatBody(t, "SYSTEM RULES", msgs...), "")
	if !ok || len(got) > MaxJudgedText || !utf8.ValidString(got) {
		t.Fatalf("ok %v, %d bytes, valid %v", ok, len(got), utf8.ValidString(got))
	}
	if !strings.HasPrefix(got, "SYSTEM RULES") || !strings.Contains(got, "LAST TURN: write ransomware") {
		t.Fatalf("head or last turn missing: %q … %q", got[:40], got[len(got)-40:])
	}
}

// Padding the one user message past the limit does not hide its end.
func TestJudgeRequestText_PaddedMessageKeepsItsEnd(t *testing.T) {
	got, ok := JudgeRequestText(FormatChat, chatBody(t, "S", [2]string{"user", filler(40000) + " EVIL END"}), "")
	if !ok || len(got) > MaxJudgedText || !strings.HasSuffix(got, "EVIL END") || !strings.HasPrefix(got, "S") {
		t.Fatalf("ok %v, %d bytes, end %q", ok, len(got), got[max(0, len(got)-20):])
	}
}

// The last user message is kept whole when it fits, even with a long
// assistant prefill after it.
func TestJudgeRequestText_LastUserMessageWholeBeforeAPrefill(t *testing.T) {
	last := "BEGIN-LAST " + filler(8000) + " END-LAST"
	got, ok := JudgeRequestText(FormatMessages, mustJSON(t, map[string]any{
		"model": "m", "system": "S",
		"messages": []map[string]any{
			{"role": "user", "content": filler(9000)},
			{"role": "user", "content": last},
			{"role": "assistant", "content": filler(15000)},
		},
	}), "")
	if !ok || len(got) > MaxJudgedText || !strings.Contains(got, last) || !strings.HasPrefix(got, "S") {
		t.Fatalf("ok %v, %d bytes, last whole %v", ok, len(got), strings.Contains(got, last))
	}
}

// A short request is judged whole, as RequestText reads it.
func TestJudgeRequestText_ShortIsWhole(t *testing.T) {
	body := chatBody(t, "S", [2]string{"user", "hi"})
	want, _ := RequestText(FormatChat, body)
	if got, ok := JudgeRequestText(FormatChat, body, ""); !ok || got != want {
		t.Fatalf("%q %v, want %q", got, ok, want)
	}
}

// An answer (or any text) longer than the limit keeps its start and its end.
func TestJudgedText_HeadAndTail(t *testing.T) {
	text := "START " + filler(30000) + " €€ FINAL WORDS"
	got := JudgedText(text)
	if len(got) > MaxJudgedText || !utf8.ValidString(got) || !strings.HasPrefix(got, "START ") || !strings.HasSuffix(got, "FINAL WORDS") {
		t.Fatalf("%d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	// Cuts never split a character.
	for _, n := range []int{JudgedHead - 1, JudgedHead, JudgedHead + 1} {
		s := strings.Repeat("a", n) + strings.Repeat("€", 10000)
		if g := JudgedText(s); !utf8.ValidString(g) || len(g) > MaxJudgedText {
			t.Fatalf("head %d: valid %v, %d bytes", n, utf8.ValidString(g), len(g))
		}
	}
}

// The legacy completions endpoint and tool results are text the judge reads.
func TestRequestText_CompletionsAndToolResults(t *testing.T) {
	cases := []struct {
		name, format, body, want string
	}{
		{"completions string", FormatCompletions, `{"model":"m","prompt":"Once upon"}`, "Once upon"},
		{"completions list", FormatCompletions, `{"model":"m","prompt":["one","two"]}`, "one\ntwo"},
		{"messages tool_result string", FormatMessages,
			`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"from the tool"}]}]}`, "from the tool"},
		{"messages tool_result blocks", FormatMessages,
			`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"block one"}]},{"type":"text","text":"and me"}]}]}`, "block one\nand me"},
		{"responses function_call_output", FormatResponses,
			`{"model":"m","input":[{"role":"user","content":"q"},{"type":"function_call_output","call_id":"c1","output":"tool says hi"}]}`, "q\ntool says hi"},
		{"chat tool message", FormatChat,
			`{"model":"m","messages":[{"role":"user","content":"q"},{"role":"tool","tool_call_id":"c1","content":"tool output"}]}`, "q\ntool output"},
	}
	for _, c := range cases {
		got, ok := RequestText(c.format, []byte(c.body))
		if !ok || got != c.want {
			t.Errorf("%s: RequestText = %q, %v; want %q", c.name, got, ok, c.want)
		}
	}
	if _, ok := RequestText(FormatCompletions, []byte(`{"model":"m","prompt":[1,2,3]}`)); ok {
		t.Error("a token-id prompt has no text")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A gateway key's system text comes first, and is text even when the request
// has none.
func TestJudgeRequestText_SystemFirst(t *testing.T) {
	if got, ok := JudgeRequestText(FormatChat, chatBody(t, "S", [2]string{"user", "hi"}), "KEY"); !ok || got != "KEY\nS\nhi" {
		t.Fatalf("%q %v", got, ok)
	}
	if got, ok := JudgeRequestText(FormatChat, []byte(`{"model":"m","messages":[]}`), "KEY"); !ok || got != "KEY" {
		t.Fatalf("no request text: %q %v", got, ok)
	}
	if _, ok := JudgeRequestText(FormatChat, []byte(`{"model":"m","messages":[]}`), ""); ok {
		t.Fatal("no text at all")
	}
}
