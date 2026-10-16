package aigw

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/guardrails"
)

// A fake OpenAI project key, built at run time so that no scanner takes this
// file for a leak. Its first 15 characters after "sk-" are not yet a match.
var guardKey = "sk-" + "proj-" + strings.Repeat("aB3dE5fG7h", 4)

var guardEngine = guardrails.NewEngine()

func guardInspect(text string) (bool, string) { return guardEngine.InspectAnswerText(text, nil) }

func guardReq() *http.Request {
	return httptest.NewRequest("POST", "https://x.example.com/v1/chat/completions", nil)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The three text pieces of a streamed answer: the third completes the key.
var guardPieces = []string{"Your key is " + guardKey[:8], guardKey[8:18], guardKey[18:] + " - keep it safe."}

func chatFrames(pieces []string) []string {
	out := []string{"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"}
	for _, p := range pieces {
		out = append(out, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":"+jsonString(p)+"}}]}\n\n")
	}
	return append(out,
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n")
}

func responsesFrames(pieces []string) []string {
	out := []string{"event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r\",\"output\":[]}}\n\n"}
	for i, p := range pieces {
		out = append(out, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":"+strconv.Itoa(i+1)+",\"item_id\":\"msg_0\",\"output_index\":0,\"content_index\":0,\"delta\":"+jsonString(p)+"}\n\n")
	}
	return append(out, "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":9,\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n")
}

func messagesFrames(pieces []string) []string {
	out := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"content\":[]}}\n\n",
	}
	for _, p := range pieces {
		out = append(out, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":"+jsonString(p)+"}}\n\n")
	}
	return append(out, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

// serveGuard writes an upstream response through a guard: the header, then
// the chunks, then the end. It returns what the app received.
func serveGuard(t *testing.T, format, action, contentType string, status int, chunks []string) (*httptest.ResponseRecorder, *answerGuard) {
	t.Helper()
	rec := httptest.NewRecorder()
	g := newAnswerGuard(rec, guardReq(), format, action, guardInspect)
	g.Header().Set("Content-Type", contentType)
	g.WriteHeader(status)
	for _, c := range chunks {
		if _, err := g.Write([]byte(c)); err != nil {
			t.Fatalf("write: %v", err)
		}
		g.Flush()
	}
	g.finish(false)
	return rec, g
}

func TestAnswerGuard_StreamBlock(t *testing.T) {
	cases := []struct {
		format string
		frames []string
		errEv  string // what the one error event starts with
		after  string // what follows it
	}{
		{guardrails.FormatChat, chatFrames(guardPieces), "data: {\"error\":", "data: [DONE]\n\n"},
		{guardrails.FormatResponses, responsesFrames(guardPieces), "event: error\ndata: {\"type\":\"error\"", ""},
		{guardrails.FormatMessages, messagesFrames(guardPieces), "event: error\ndata: {\"type\":\"error\"", ""},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			rec, g := serveGuard(t, c.format, guardrails.AnswerActionBlock, "text/event-stream", 200, c.frames)
			got := rec.Body.String()
			// Frames 0..2 hold the start and the first two pieces; frame 3
			// completes the key.
			before := strings.Join(c.frames[:3], "")
			if !strings.HasPrefix(got, before) {
				t.Fatalf("the frames before the hit were not delivered as sent:\n%s", got)
			}
			rest := got[len(before):]
			if !strings.HasPrefix(rest, c.errEv) {
				t.Fatalf("no error event after the clean frames:\n%s", rest)
			}
			if strings.Count(rest, "\n\n") != 1+strings.Count(c.after, "\n\n") {
				t.Fatalf("want exactly one error event:\n%s", rest)
			}
			if !strings.HasSuffix(rest, c.after) {
				t.Fatalf("error event not followed by %q:\n%s", c.after, rest)
			}
			if !strings.Contains(rest, "guardrail_blocked_answer") || !strings.Contains(rest, "the answer was withheld by a guardrail") {
				t.Fatalf("error event without code and message: %s", rest)
			}
			if strings.Contains(got, guardPieces[2]) || strings.Contains(got, "keep it safe") {
				t.Fatalf("frame three reached the app:\n%s", got)
			}
			hit, id, blocked, mid := g.Result()
			if !hit || id != "openai_key" || !blocked || !mid {
				t.Fatalf("Result = %v %q %v %v", hit, id, blocked, mid)
			}
		})
	}
}

func TestAnswerGuard_StreamFlag(t *testing.T) {
	for format, frames := range map[string][]string{
		guardrails.FormatChat:      chatFrames(guardPieces),
		guardrails.FormatResponses: responsesFrames(guardPieces),
		guardrails.FormatMessages:  messagesFrames(guardPieces),
	} {
		t.Run(format, func(t *testing.T) {
			rec, g := serveGuard(t, format, guardrails.AnswerActionFlag, "text/event-stream", 200, frames)
			if rec.Body.String() != strings.Join(frames, "") {
				t.Fatalf("flag changed the stream:\n%s", rec.Body.String())
			}
			// Nothing was stopped: the hit is not "mid-answer".
			hit, id, blocked, mid := g.Result()
			if !hit || id != "openai_key" || blocked || mid {
				t.Fatalf("Result = %v %q %v %v", hit, id, blocked, mid)
			}
		})
	}
}

// Frames that arrive cut into small pieces, split anywhere, are put back
// together before they are checked: the outcome is the same.
func TestAnswerGuard_StreamInSmallPieces(t *testing.T) {
	frames := chatFrames(guardPieces)
	whole := strings.Join(frames, "")
	var chunks []string
	for i := 0; i < len(whole); i += 7 {
		chunks = append(chunks, whole[i:min(i+7, len(whole))])
	}
	rec, g := serveGuard(t, guardrails.FormatChat, guardrails.AnswerActionBlock, "text/event-stream", 200, chunks)
	if !strings.HasPrefix(rec.Body.String(), strings.Join(frames[:3], "")) || strings.Contains(rec.Body.String(), "keep it safe") {
		t.Fatalf("app received:\n%s", rec.Body.String())
	}
	if _, _, blocked, _ := g.Result(); !blocked {
		t.Fatal("not blocked")
	}
	// CRLF line endings, too.
	crlf := strings.ReplaceAll(whole, "\n", "\r\n")
	rec, g = serveGuard(t, guardrails.FormatChat, guardrails.AnswerActionBlock, "text/event-stream", 200, []string{crlf})
	if _, _, blocked, _ := g.Result(); !blocked || strings.Contains(rec.Body.String(), "keep it safe") {
		t.Fatalf("CRLF stream not blocked:\n%s", rec.Body.String())
	}
}

// A stream that ends without the blank line after its last frame is still
// checked at the end.
func TestAnswerGuard_StreamLastFrameUnterminated(t *testing.T) {
	frames := chatFrames([]string{"clean: ", guardKey})
	body := strings.Join(frames[:3], "")
	body = strings.TrimSuffix(body, "\n\n")
	rec, g := serveGuard(t, guardrails.FormatChat, guardrails.AnswerActionBlock, "text/event-stream", 200, []string{body})
	if _, _, blocked, _ := g.Result(); !blocked || strings.Contains(rec.Body.String(), guardKey) {
		t.Fatalf("unterminated last frame: %s", rec.Body.String())
	}
}

func chatAnswer(text string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":` + jsonString(text) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`
}

func TestAnswerGuard_WholeAnswer(t *testing.T) {
	leak := chatAnswer("Here is the key: " + guardKey)
	t.Run("block", func(t *testing.T) {
		rec := httptest.NewRecorder()
		g := newAnswerGuard(rec, guardReq(), guardrails.FormatChat, guardrails.AnswerActionBlock, guardInspect)
		g.Header().Set("Content-Type", "application/json")
		g.Header().Set("Content-Length", strconv.Itoa(len(leak)))
		g.WriteHeader(200)
		_, _ = g.Write([]byte(leak[:40]))
		g.Flush() // a flush while the answer is held sends nothing
		if rec.Code != 200 || rec.Body.Len() != 0 || rec.Flushed {
			t.Fatalf("bytes went out before the answer was complete: %q flushed=%v", rec.Body.String(), rec.Flushed)
		}
		_, _ = g.Write([]byte(leak[40:]))
		g.finish(false)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, guardKey) || strings.Contains(body, "Here is the key") {
			t.Fatalf("answer bytes reached the app: %s", body)
		}
		if !strings.Contains(body, `"code":"guardrail_blocked_answer"`) || !strings.Contains(body, "the answer was withheld by a guardrail") {
			t.Fatalf("body: %s", body)
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(body)) {
			t.Fatalf("Content-Length %q for a body of %d", cl, len(body))
		}
		hit, id, blocked, mid := g.Result()
		if !hit || id != "openai_key" || !blocked || mid {
			t.Fatalf("Result = %v %q %v %v", hit, id, blocked, mid)
		}
	})
	t.Run("block messages", func(t *testing.T) {
		ans := `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":` + jsonString(guardKey) + `}],"stop_reason":"end_turn"}`
		rec, _ := serveGuard(t, guardrails.FormatMessages, guardrails.AnswerActionBlock, "application/json", 200, []string{ans})
		if rec.Code != 502 || !strings.Contains(rec.Body.String(), `"type":"error"`) || !strings.Contains(rec.Body.String(), "guardrail_blocked_answer") {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("flag", func(t *testing.T) {
		rec, g := serveGuard(t, guardrails.FormatChat, guardrails.AnswerActionFlag, "application/json", 200, []string{leak[:30], leak[30:]})
		if rec.Code != 200 || rec.Body.String() != leak {
			t.Fatalf("flag changed the answer: %d %s", rec.Code, rec.Body.String())
		}
		hit, id, blocked, mid := g.Result()
		if !hit || id != "openai_key" || blocked || mid {
			t.Fatalf("Result = %v %q %v %v", hit, id, blocked, mid)
		}
	})
	t.Run("escaped", func(t *testing.T) {
		esc := strings.Replace(leak, "sk-", `sk\u002d`, 1)
		rec, g := serveGuard(t, guardrails.FormatChat, guardrails.AnswerActionBlock, "application/json; charset=utf-8", 200, []string{esc})
		if hit, _, _, _ := g.Result(); !hit || rec.Code != 502 {
			t.Fatalf("escaped key not caught: %d", rec.Code)
		}
	})
}

func TestAnswerGuard_CleanAnswersUnchanged(t *testing.T) {
	clean := chatAnswer("Hello there, nothing secret here.")
	stream := strings.Join(chatFrames([]string{"Hello ", "there."}), "")
	for _, action := range []string{guardrails.AnswerActionFlag, guardrails.AnswerActionBlock} {
		rec, g := serveGuard(t, guardrails.FormatChat, action, "application/json", 200, []string{clean[:10], clean[10:]})
		if rec.Code != 200 || rec.Body.String() != clean {
			t.Fatalf("%s: whole answer changed: %d %s", action, rec.Code, rec.Body.String())
		}
		if hit, _, _, _ := g.Result(); hit {
			t.Fatalf("%s: clean answer hit", action)
		}
		rec, g = serveGuard(t, guardrails.FormatChat, action, "text/event-stream", 200, []string{stream[:50], stream[50:]})
		if rec.Body.String() != stream {
			t.Fatalf("%s: stream changed:\n%s", action, rec.Body.String())
		}
		if hit, _, _, _ := g.Result(); hit {
			t.Fatalf("%s: clean stream hit", action)
		}
	}
}

func TestAnswerGuard_NotInspected(t *testing.T) {
	leak := "binary " + guardKey
	cases := []struct {
		name, ct string
		status   int
		enc      string
	}{
		{"audio", "audio/mpeg", 200, ""},
		{"image", "image/png", 200, ""},
		{"error status", "application/json", 500, ""},
		{"compressed", "application/json", 200, "gzip"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		g := newAnswerGuard(rec, guardReq(), guardrails.FormatChat, guardrails.AnswerActionBlock, guardInspect)
		g.Header().Set("Content-Type", c.ct)
		if c.enc != "" {
			g.Header().Set("Content-Encoding", c.enc)
		}
		g.WriteHeader(c.status)
		_, _ = g.Write([]byte(leak))
		g.finish(false)
		if rec.Code != c.status || rec.Body.String() != leak {
			t.Errorf("%s: %d %q", c.name, rec.Code, rec.Body.String())
		}
		if hit, _, _, _ := g.Result(); hit {
			t.Errorf("%s: inspected", c.name)
		}
	}
}

// An answer larger than what the guard may hold is delivered uninspected.
func TestAnswerGuard_OverTheCapIsDelivered(t *testing.T) {
	leak := chatAnswer(guardKey + strings.Repeat(" padding", 50))
	rec := httptest.NewRecorder()
	g := newAnswerGuard(rec, guardReq(), guardrails.FormatChat, guardrails.AnswerActionBlock, guardInspect)
	g.max = 100
	g.Header().Set("Content-Type", "application/json")
	g.WriteHeader(200)
	for i := 0; i < len(leak); i += 64 {
		_, _ = g.Write([]byte(leak[i:min(i+64, len(leak))]))
	}
	g.finish(false)
	if rec.Code != 200 || rec.Body.String() != leak {
		t.Fatalf("over the cap: %d %q", rec.Code, rec.Body.String())
	}
	if hit, _, _, _ := g.Result(); hit {
		t.Fatal("an answer over the cap was inspected")
	}
}

// A held answer whose upstream broke off is not delivered in part.
func TestAnswerGuard_AbortedHeldAnswerIsDropped(t *testing.T) {
	rec := httptest.NewRecorder()
	g := newAnswerGuard(rec, guardReq(), guardrails.FormatChat, guardrails.AnswerActionBlock, guardInspect)
	g.Header().Set("Content-Type", "application/json")
	g.WriteHeader(200)
	_, _ = g.Write([]byte(`{"choices":[{"message":{"content":"` + guardKey[:10]))
	g.finish(true)
	if rec.Body.Len() != 0 {
		t.Fatalf("a cut answer went out: %q", rec.Body.String())
	}
}

func TestAnswerGuard_ImplicitHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	g := newAnswerGuard(rec, guardReq(), guardrails.FormatChat, guardrails.AnswerActionBlock, guardInspect)
	g.Header().Set("Content-Type", "application/json")
	_, _ = g.Write([]byte(chatAnswer(guardKey)))
	g.finish(false)
	if rec.Code != 502 || bytes.Contains(rec.Body.Bytes(), []byte(guardKey)) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// Custom patterns are checked on the whole window at events without text
// (the end of the answer), so a short answer that never reaches the custom
// interval is still blocked before its end is forwarded.
func TestAnswerGuard_CustomPatternCheckedBeforeTheEnd(t *testing.T) {
	custom := []guardrails.CustomPattern{{ID: "codename", Pattern: `project falcon`, Direction: guardrails.DirectionAnswer}}
	rec := httptest.NewRecorder()
	g := newAnswerGuard(rec, guardReq(), guardrails.FormatChat, guardrails.AnswerActionBlock, guardInspect)
	g.quick = guardEngine.InspectAnswerBundled
	g.full = func(text string) (bool, string) { return guardEngine.InspectAnswerCustom(text, custom) }
	stopped := 0
	g.onStop = func() { stopped++ }
	frames := chatFrames([]string{"It is project ", "falcon."})
	g.Header().Set("Content-Type", "text/event-stream")
	g.WriteHeader(200)
	for _, f := range frames {
		_, _ = g.Write([]byte(f))
	}
	g.finish(false)
	body := rec.Body.String()
	if !strings.HasPrefix(body, strings.Join(frames[:3], "")) || strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "guardrail_blocked_answer") {
		t.Fatalf("body:\n%s", body)
	}
	if hit, id, blocked, _ := g.Result(); !hit || id != "codename" || !blocked || stopped != 1 {
		t.Fatalf("Result %v %q %v, onStop called %d times", hit, id, blocked, stopped)
	}
}

// A custom pattern in the last text of a stream that has no event after it
// is found at the end of the stream.
func TestAnswerGuard_CustomPatternAtTheVeryEnd(t *testing.T) {
	custom := []guardrails.CustomPattern{{ID: "codename", Pattern: `project falcon`, Direction: guardrails.DirectionAnswer}}
	rec := httptest.NewRecorder()
	g := newAnswerGuard(rec, guardReq(), guardrails.FormatChat, guardrails.AnswerActionFlag, guardInspect)
	g.quick = guardEngine.InspectAnswerBundled
	g.full = func(text string) (bool, string) { return guardEngine.InspectAnswerCustom(text, custom) }
	g.Header().Set("Content-Type", "text/event-stream")
	g.WriteHeader(200)
	_, _ = g.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"project falcon\"}}]}\n\n"))
	g.finish(false)
	if hit, id, _, _ := g.Result(); !hit || id != "codename" {
		t.Fatalf("Result %v %q", hit, id)
	}
}
