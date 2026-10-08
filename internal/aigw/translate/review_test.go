package translate

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
	"github.com/ankoehn/burrow/internal/aigw/translate/responses"
)

// A call that was cut before its first argument byte must not reach a caller as a call with the
// arguments "{}": a decoder stops every open part before it reports the failure, and a caller's
// encoder that completed such a call would hand over arguments nobody sent.
// TestResponsesChat_Streamed_ACallCutBeforeItsFirstArgumentByte holds this for the Responses
// caller; these two hold it for the Messages caller and for the Chat caller.

func TestMessagesChat_Streamed_ACallCutBeforeItsFirstArgumentByte(t *testing.T) {
	nameOnly := func(index int) string {
		return chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"cut","type":"function","function":{"name":"f","arguments":""}}]}`, index))
	}
	whole := func(index int) string {
		return chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"whole","type":"function","function":{"name":"g","arguments":"{\"k\":1}"}}]}`, index))
	}
	for name, c := range map[string]struct {
		upstream []byte
		whole    bool
	}{
		"then the upstream closes":               {sseOf(nameOnly(0)), false},
		"then an error frame":                    {sseOf(nameOnly(0), `{"error":{"message":"overloaded"}}`), false},
		"then [DONE] without a finish_reason":    {sseOf(nameOnly(0), "[DONE]"), false},
		"then a frame that cannot be read":       {sseOf(nameOnly(0), `{"choices":[{"delta":`), false},
		"beside a complete call that came later": {sseOf(nameOnly(0), whole(1)), true},
		"beside a complete call that came first": {sseOf(whole(0), nameOnly(1), `{"error":{"message":"overloaded"}}`), true},
	} {
		for _, piece := range []int{1, 4096} {
			call := newCall(t, true).upstream(200, "text/event-stream", c.upstream, piece)
			raw := call.rec.Body.Bytes()
			s, err := messages.CheckStream(raw)
			if err != nil || s.Stopped || s.StopReason != "" || s.ErrType != "api_error" {
				t.Fatalf("%s: %v, %+v\n%s", name, err, s, raw)
			}
			whole := false
			for _, b := range s.Blocks {
				if b.ToolID == "cut" && (len(b.Deltas) > 0 || b.PartialJSON != "") {
					t.Errorf("%s: the cut call got arguments: %+v", name, b)
				}
				whole = whole || (b.ToolID == "whole" && b.PartialJSON == `{"k":1}`)
			}
			if whole != c.whole || bytes.Contains(raw, []byte(`"partial_json":"{}"`)) {
				t.Errorf("%s: the whole call is handed over: %v\n%s", name, whole, raw)
			}
			// The protocol wants every block closed before the error event; CheckStream held that.
			if !bytes.HasSuffix(raw, []byte(`}}`+"\n\n")) || !strings.Contains(string(raw[max(0, len(raw)-200):]), "event: error\n") {
				t.Errorf("%s: the error event is not the last thing written\n%s", name, raw)
			}
			if code, mid := call.w.Failure(); code == "" || !mid {
				t.Errorf("%s: Failure = %q, %v", name, code, mid)
			}
		}
	}
	// The same call in a stream that ends well is a call without arguments.
	call := newCall(t, true).upstream(200, "text/event-stream", sseOf(nameOnly(0), whole(1), finishChunk("tool_calls"), "[DONE]"), 7)
	s, err := messages.CheckStream(call.rec.Body.Bytes())
	if err != nil || !s.Stopped || len(s.Blocks) != 2 || s.Blocks[0].ToolID != "cut" || s.Blocks[0].Input != "{}" || s.Blocks[0].PartialJSON != "{}" || s.Blocks[1].Input != `{"k":1}` {
		t.Fatalf("%v, %+v", err, s)
	}
	if code, mid := call.w.Failure(); code != "" || mid {
		t.Fatalf("Failure = %q, %v", code, mid)
	}
}

func TestChatMessages_Streamed_ACallCutBeforeItsFirstArgumentByte(t *testing.T) {
	wholeCall := func(i int) []string { return []string{aTool(i, "whole", "g"), aArgs(i, []byte(`{"k":1}`)), aStop(i)} }
	frames := func(parts ...any) []byte {
		list := []string{aStart}
		for _, p := range parts {
			switch v := p.(type) {
			case string:
				list = append(list, v)
			case []string:
				list = append(list, v...)
			}
		}
		return anthropicSSE(list...)
	}
	for name, c := range map[string]struct {
		upstream []byte
		cut      string // the arguments the caller holds for the call "cut"
		whole    bool
	}{
		"then the upstream closes":                       {frames(aTool(0, "cut", "f")), "", false},
		"then an error event":                            {frames(aTool(0, "cut", "f"), aError), "", false},
		"its block is stopped, then the upstream closes": {frames(aTool(0, "cut", "f"), aStop(0)), "", false},
		"its block is stopped, then an error event":      {frames(aTool(0, "cut", "f"), aStop(0), aError), "", false},
		"then a frame that cannot be read":               {append(frames(aTool(0, "cut", "f")), "event: content_block_delta\ndata: {\"type\":\n\n"...), "", false},
		"beside a complete call that came first":         {frames(wholeCall(0), aTool(1, "cut", "f"), aError), "", true},
		// Not cut: the upstream closed the block and went on, so it is a call without arguments.
		"closed by the upstream, a later call is cut": {frames(aTool(0, "cut", "f"), aStop(0), aTool(1, "late", "g"), aArgs(1, []byte(`{"k":`))), "{}", false},
	} {
		for _, piece := range []int{1, 4096} {
			call := newTargetCall(t, Chat, true, askUsage).upstream(200, "text/event-stream", c.upstream, piece)
			raw := call.rec.Body.Bytes()
			s, err := chat.CheckStream(raw)
			if err != nil || s.Done || s.FinishReason != "" || s.ErrMessage == "" || bytes.Contains(raw, []byte("[DONE]")) {
				t.Fatalf("%s: %v, %+v\n%s", name, err, s, raw)
			}
			whole := false
			for _, tc := range s.Calls {
				if tc.ID == "cut" && tc.Arguments != c.cut {
					t.Errorf("%s: the cut call has the arguments %q: %+v", name, tc.Arguments, tc)
				}
				whole = whole || (tc.ID == "whole" && tc.Arguments == `{"k":1}`)
			}
			if whole != c.whole {
				t.Errorf("%s: the whole call is handed over: %v\n%s", name, whole, raw)
			}
			if code, mid := call.w.Failure(); code == "" || !mid {
				t.Errorf("%s: Failure = %q, %v", name, code, mid)
			}
		}
	}
	// The same call in a stream that ends well is a call without arguments.
	call := newTargetCall(t, Chat, true, askUsage).upstream(200, "text/event-stream", frames(aTool(0, "cut", "f"), aStop(0), wholeCall(1), aToolEnd, aDone), 7)
	s, err := chat.CheckStream(call.rec.Body.Bytes())
	if err != nil || !s.Done || s.FinishReason != "tool_calls" || len(s.Calls) != 2 || s.Calls[0].ID != "cut" || s.Calls[0].Arguments != "{}" || s.Calls[1].Arguments != `{"k":1}` {
		t.Fatalf("%v, %+v", err, s)
	}
	if code, mid := call.w.Failure(); code != "" || mid {
		t.Fatalf("Failure = %q, %v", code, mid)
	}
}

// goneAt is a client that goes away with its n-th write.
type goneAt struct {
	http.ResponseWriter
	n, writes int
}

func (g *goneAt) Write(p []byte) (int, error) {
	if g.writes++; g.writes >= g.n {
		return 0, errors.New("broken pipe")
	}
	return g.ResponseWriter.Write(p)
}

func TestResponse_Streamed_ClientGoneWhileTheEncoderEndsTheStream(t *testing.T) {
	// An answer the caller's encoder refuses itself (an item over its limit: no decoder checks a
	// text's size) is ended by the encoder with a failure event. When that very write fails, the
	// client is gone: the upstream must not be read to its end, and nothing is reported as an
	// answer that was ended in the caller's shape.
	text := func(n int) string { return chunk(`{"content":"` + strings.Repeat("x", n) + `"}`) }
	upstream := [][]byte{sseOf(chunk(`{"role":"assistant","content":"a"}`))}
	for sent := 0; sent <= responses.MaxItemBytes; sent += 1 << 20 {
		upstream = append(upstream, sseOf(text(1<<20)))
	}
	run := func(n int) (w ResponseWriter, client *goneAt, failed error) {
		client = &goneAt{ResponseWriter: httptest.NewRecorder(), n: n}
		w = lookup(t, Responses, Chat).Response(client, ResponseOptions{Stream: true})
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, p := range upstream {
			if _, failed = w.Write(p); failed != nil {
				break
			}
		}
		return w, client, failed
	}
	// With a client that stays, the encoder ends the stream and the writer goes on taking bytes.
	w, client, failed := run(1 << 30)
	if code, mid := w.Failure(); failed != nil || code != CodeUpstreamInvalid || !mid || client.writes < 8 {
		t.Fatalf("a client that stays: %v, Failure = %q, %v, %d writes", failed, code, mid, client.writes)
	}
	w.Finish()
	// Whichever write is the one that fails — among them every frame the encoder ends the stream
	// with — the writer says so, and reports no failure of the answer.
	for n := client.writes; n > client.writes-4; n-- {
		w, _, failed = run(n)
		if failed == nil {
			t.Fatalf("write %d of %d failed, and Write kept succeeding: the upstream would be read to its end", n, client.writes)
		}
		if code, _ := w.Failure(); code != "" {
			t.Fatalf("write %d of %d failed: Failure = %q for a client that went away", n, client.writes, code)
		}
		w.Finish()
	}
}
