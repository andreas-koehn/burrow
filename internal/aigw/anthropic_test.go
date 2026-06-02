package aigw_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/aigw"
)

// ----- helpers -------------------------------------------------------------

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// compactJSON normalises a JSON byte slice for byte-for-byte comparison.
// It removes whitespace and orders nothing — the test fixtures are written
// with the same key order the implementation emits.
func compactJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact json: %v\n--- input ---\n%s", err, raw)
	}
	return buf.Bytes()
}

// ----- request rewrite tests ----------------------------------------------

// TestAnthropicRewriteRequest — table-driven over the 5 fixtures.
func TestAnthropicRewriteRequest(t *testing.T) {
	cases := []struct {
		name      string
		fixture   string
		wantLossy bool
		wantDrops []string // dropped field names, in observed order
	}{
		{"plain_user", "req_plain_user", false, nil},
		{"with_system", "req_with_system", true, []string{"top_k"}},
		{"multi_turn", "req_multi_turn", false, nil},
		{"tool_use", "req_tool_use", false, nil},
		{"image_input", "req_image_input", true, []string{"image_input"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := mustReadFixture(t, tc.fixture+".json")
			wantRaw := mustReadFixture(t, tc.fixture+".internal.json")
			want := compactJSON(t, wantRaw)

			got, lossy, dropped, err := aigw.RewriteRequest(in)
			if err != nil {
				t.Fatalf("RewriteRequest: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("body mismatch\n got: %s\nwant: %s", got, want)
			}
			if lossy != tc.wantLossy {
				t.Fatalf("lossy: got %v want %v", lossy, tc.wantLossy)
			}
			if !equalStringSlice(dropped, tc.wantDrops) {
				t.Fatalf("dropped: got %v want %v", dropped, tc.wantDrops)
			}
		})
	}
}

// TestAnthropicLossyImageInput — explicit assertion that image content
// flags lossy=true with dropped=["image_input"].
func TestAnthropicLossyImageInput(t *testing.T) {
	in := []byte(`{"model":"c","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"x"}}]}]}`)
	_, lossy, dropped, err := aigw.RewriteRequest(in)
	if err != nil {
		t.Fatalf("RewriteRequest: %v", err)
	}
	if !lossy {
		t.Fatal("expected lossy=true for image content")
	}
	found := false
	for _, d := range dropped {
		if d == "image_input" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'image_input' in dropped, got %v", dropped)
	}
}

func TestAnthropicRewriteRequest_TopKDropped(t *testing.T) {
	in := []byte(`{"model":"c","max_tokens":1,"top_k":40,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, lossy, dropped, err := aigw.RewriteRequest(in)
	if err != nil {
		t.Fatalf("RewriteRequest: %v", err)
	}
	if !lossy {
		t.Fatal("expected lossy=true when top_k is present")
	}
	want := []string{"top_k"}
	if !equalStringSlice(dropped, want) {
		t.Fatalf("dropped: got %v want %v", dropped, want)
	}
}

func TestAnthropicRewriteRequest_InvalidJSON(t *testing.T) {
	_, _, _, err := aigw.RewriteRequest([]byte("{not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ----- response stream tests ----------------------------------------------

// TestAnthropicStreamRoundTripText — feed the SSE fixture in one shot and
// assert the captured bytes equal the internal fixture.
func TestAnthropicStreamRoundTripText(t *testing.T) {
	in := mustReadFixture(t, "anthropic_stream.sse")
	want := mustReadFixture(t, "internal_stream.sse")

	var buf bytes.Buffer
	w := aigw.WrapResponseStream(&buf)
	if _, err := w.Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("stream mismatch\n got:\n%s\n---\nwant:\n%s", buf.Bytes(), want)
	}
}

// recordingFlusher captures every Write timestamp + payload and offers a
// Flush counter, mirroring the aimeter pattern.
type recordingFlusher struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	writes  []time.Time
	flushes int
}

func (r *recordingFlusher) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, time.Now())
	return r.buf.Write(p)
}

func (r *recordingFlusher) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
}

func (r *recordingFlusher) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf.Bytes()...)
}

func (r *recordingFlusher) Writes() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.writes...)
}

// Ensure recordingFlusher satisfies http.Flusher.
var _ http.Flusher = (*recordingFlusher)(nil)

// TestAnthropicStreamFlushesEachFrame — drips frames through io.Pipe with a
// sleep between each upstream frame; asserts >=10ms gaps between successive
// translated frames on the visitor side (mirrors aimeter's NonBuffered check).
func TestAnthropicStreamFlushesEachFrame(t *testing.T) {
	raw := mustReadFixture(t, "anthropic_stream.sse")
	frames := splitAnthropicFrames(t, raw)
	if len(frames) < 5 {
		t.Fatalf("fixture should have >=5 anthropic events, got %d", len(frames))
	}

	visitor := &recordingFlusher{}
	w := aigw.WrapResponseStream(visitor)

	pr, pw := io.Pipe()
	gap := 15 * time.Millisecond
	go func() {
		defer pw.Close()
		for i, f := range frames {
			if i > 0 {
				time.Sleep(gap)
			}
			if _, err := pw.Write(f); err != nil {
				return
			}
		}
	}()

	if _, err := io.Copy(w, pr); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	writes := visitor.Writes()
	if len(writes) < 2 {
		t.Fatalf("too few visitor writes: %d", len(writes))
	}
	// Count wide inter-write gaps. We don't translate every upstream event
	// (message_start, content_block_start, content_block_stop are
	// suppressed/no-op for plain text path), so we just require at least 3
	// wide gaps — proving the parser forwarded frames as they arrived.
	wide := 0
	for i := 1; i < len(writes); i++ {
		if writes[i].Sub(writes[i-1]) >= 10*time.Millisecond {
			wide++
		}
	}
	if wide < 3 {
		t.Fatalf("buffered translation — expected >=3 wide gaps, got %d (writes=%d)", wide, len(writes))
	}
	if visitor.flushes < len(writes) {
		t.Fatalf("expected >=1 Flush per Write: flushes=%d writes=%d", visitor.flushes, len(writes))
	}
}

// splitAnthropicFrames splits an SSE byte slice into individual frame chunks
// (each terminated by a blank line "\n\n").
func splitAnthropicFrames(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for {
		i := bytes.Index(raw, []byte("\n\n"))
		if i < 0 {
			if len(raw) > 0 {
				out = append(out, raw)
			}
			return out
		}
		end := i + 2
		out = append(out, append([]byte(nil), raw[:end]...))
		raw = raw[end:]
	}
}

// TestAnthropicStopReasonMapping — exercises the four stop_reason values
// and asserts the translated finish_reason in the emitted chunk.
func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := []struct {
		stopReason string
		finish     string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"stop_sequence", "stop"},
		{"tool_use", "tool_calls"},
	}
	for _, tc := range cases {
		t.Run(tc.stopReason, func(t *testing.T) {
			input := []byte(
				"event: message_delta\n" +
					`data: {"type":"message_delta","delta":{"stop_reason":"` + tc.stopReason + `"},"usage":{"input_tokens":1,"output_tokens":1}}` + "\n\n" +
					"event: message_stop\n" +
					`data: {"type":"message_stop"}` + "\n\n",
			)
			var buf bytes.Buffer
			w := aigw.WrapResponseStream(&buf)
			if _, err := w.Write(input); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			want := `"finish_reason":"` + tc.finish + `"`
			if !bytes.Contains(buf.Bytes(), []byte(want)) {
				t.Fatalf("missing %s in output:\n%s", want, buf.Bytes())
			}
			if !bytes.Contains(buf.Bytes(), []byte("data: [DONE]")) {
				t.Fatalf("missing data: [DONE] in output:\n%s", buf.Bytes())
			}
		})
	}
}

// equalStringSlice does a deep equality check; nil and empty are treated as
// equal so we don't have to write `[]string{}` everywhere in the test table.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
