package sse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const sample = "event: message_start\ndata: {\"a\":1}\n\n" +
	": ping\n\n" +
	"data: one\ndata: two\n\n" +
	"data:nospace\n\n" +
	"data:  two spaces\n\n" +
	"id: 7\nretry: 100\nunknown line\ndata: {\"ü\":\"日本\"}\n\n" +
	"data\n\n" +
	"event: lonely\n\n" +
	"data: last\n\n"

func sampleFrames() []Frame {
	return []Frame{
		{Event: "message_start", Data: []byte(`{"a":1}`)},
		{Data: []byte("one\ntwo")},
		{Data: []byte("nospace")},
		{Data: []byte(" two spaces")},
		{Data: []byte(`{"ü":"日本"}`)},
		{Data: []byte{}},
		{Data: []byte("last")},
	}
}

func feedAll(t *testing.T, p *Parser, chunks ...[]byte) []Frame {
	t.Helper()
	var out []Frame
	for _, c := range chunks {
		fs, err := p.Feed(c)
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		out = append(out, fs...)
	}
	return out
}

func TestParser_Whole(t *testing.T) {
	got := feedAll(t, NewParser(1024), []byte(sample))
	if !reflect.DeepEqual(got, sampleFrames()) {
		t.Fatalf("frames = %q", got)
	}
}

func TestParser_ByteByByte(t *testing.T) {
	p := NewParser(1024)
	var got []Frame
	for i := 0; i < len(sample); i++ {
		got = append(got, feedAll(t, p, []byte{sample[i]})...)
	}
	if !reflect.DeepEqual(got, sampleFrames()) {
		t.Fatalf("frames = %q", got)
	}
}

func TestParser_EverySplit(t *testing.T) {
	// Every split point: this cuts lines, field names and UTF-8 sequences.
	for i := 0; i <= len(sample); i++ {
		got := feedAll(t, NewParser(1024), []byte(sample[:i]), []byte(sample[i:]))
		if !reflect.DeepEqual(got, sampleFrames()) {
			t.Fatalf("split at %d: frames = %q", i, got)
		}
	}
}

func TestParser_LineEndings(t *testing.T) {
	for name, nl := range map[string]string{"crlf": "\r\n", "cr": "\r"} {
		in := strings.ReplaceAll(sample, "\n", nl)
		for i := 0; i <= len(in); i++ {
			got := feedAll(t, NewParser(1024), []byte(in[:i]), []byte(in[i:]))
			if !reflect.DeepEqual(got, sampleFrames()) {
				t.Fatalf("%s split at %d: frames = %q", name, i, got)
			}
		}
	}
}

func TestParser_BOM(t *testing.T) {
	in := "\xEF\xBB\xBFdata: x\n\n"
	for i := 0; i <= len(in); i++ {
		got := feedAll(t, NewParser(64), []byte(in[:i]), []byte(in[i:]))
		if !reflect.DeepEqual(got, []Frame{{Data: []byte("x")}}) {
			t.Fatalf("split at %d: frames = %q", i, got)
		}
	}
}

func TestParser_Flush(t *testing.T) {
	p := NewParser(1024)
	got := feedAll(t, p, []byte("data: a\n\nevent: e\ndata: tail"))
	if !reflect.DeepEqual(got, []Frame{{Data: []byte("a")}}) {
		t.Fatalf("frames = %q", got)
	}
	if fl := p.Flush(); !reflect.DeepEqual(fl, []Frame{{Event: "e", Data: []byte("tail")}}) {
		t.Fatalf("flush = %q", fl)
	}
	if fl := p.Flush(); len(fl) != 0 {
		t.Fatalf("second flush = %q", fl)
	}
	p = NewParser(1024)
	feedAll(t, p, []byte("data: a\n\n: comment"))
	if fl := p.Flush(); len(fl) != 0 {
		t.Fatalf("flush without a frame = %q", fl)
	}
}

func TestParser_FrameDataIsOwned(t *testing.T) {
	p := NewParser(1024)
	in := []byte("data: first\n\n")
	a := feedAll(t, p, in)
	copy(in, "XXXXXXXXXXXXX")
	feedAll(t, p, []byte("data: second-and-longer\n\n"))
	if string(a[0].Data) != "first" {
		t.Fatalf("frame data changed: %q", a[0].Data)
	}
}

func TestParser_TooLarge(t *testing.T) {
	cases := map[string]string{
		"one long line":       "data: " + strings.Repeat("x", 200),
		"line without a data": strings.Repeat("y", 200),
		"many data lines":     strings.Repeat("data: 0123456789\n", 20),
		"long event name":     "event: " + strings.Repeat("e", 200) + "\n",
		"long comment":        ":" + strings.Repeat("c", 200),
	}
	for name, in := range cases {
		p := NewParser(100)
		var err error
		for i := 0; i < len(in) && err == nil; i += 7 {
			_, err = p.Feed([]byte(in[i:min(i+7, len(in))]))
		}
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("%s: err = %v", name, err)
		}
		// It stays failed and holds nothing.
		for i := 0; i < 100; i++ {
			if _, err := p.Feed(bytes.Repeat([]byte("z"), 1000)); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("%s: later err = %v", name, err)
			}
		}
		if cap(p.line) != 0 || cap(p.data) != 0 {
			t.Fatalf("%s: buffers kept: line %d data %d", name, cap(p.line), cap(p.data))
		}
		if fl := p.Flush(); len(fl) != 0 {
			t.Fatalf("%s: flush after error = %q", name, fl)
		}
	}
}

func TestParser_FramesBeforeTheLargeOne(t *testing.T) {
	p := NewParser(50)
	fs, err := p.Feed([]byte("data: ok\n\ndata: " + strings.Repeat("x", 100) + "\n\n"))
	if !errors.Is(err, ErrTooLarge) || !reflect.DeepEqual(fs, []Frame{{Data: []byte("ok")}}) {
		t.Fatalf("frames = %q, err = %v", fs, err)
	}
}

func TestParser_ManySmallFramesStayBounded(t *testing.T) {
	p := NewParser(64)
	n := 0
	for i := 0; i < 20000; i++ {
		fs, err := p.Feed([]byte(": keep-alive\ndata: 0123456789\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		n += len(fs)
	}
	if n != 20000 || cap(p.line) > 64 || cap(p.data) > 64 {
		t.Fatalf("frames %d, line cap %d, data cap %d", n, cap(p.line), cap(p.data))
	}
}

func TestParser_NonPositiveLimitUsesDefault(t *testing.T) {
	p := NewParser(0)
	if _, err := p.Feed([]byte("data: " + strings.Repeat("x", 100000) + "\n\n")); err != nil {
		t.Fatal(err)
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	frames := []Frame{
		{Event: "content_block_delta", Data: []byte(`{"x":"y"}`)},
		{Data: []byte("a\nb\n\nc")},
		{Data: []byte{}},
		{Data: []byte(" leading space")},
		{Event: "e", Data: []byte("[DONE]")},
	}
	var buf bytes.Buffer
	for _, f := range frames {
		if err := Write(&buf, f.Event, f.Data); err != nil {
			t.Fatal(err)
		}
	}
	got := feedAll(t, NewParser(1024), buf.Bytes())
	if !reflect.DeepEqual(got, frames) {
		t.Fatalf("frames = %q\nwire:\n%s", got, buf.String())
	}
}

type countingWriter struct{ calls int }

func (c *countingWriter) Write(p []byte) (int, error) { c.calls++; return len(p), nil }

func TestWrite_OneWritePerFrameAndBadInput(t *testing.T) {
	var cw countingWriter
	if err := Write(&cw, "e", []byte("a\nb")); err != nil || cw.calls != 1 {
		t.Fatalf("err %v, writes %d", err, cw.calls)
	}
	for _, bad := range []struct{ ev, data string }{{"a\nb", "x"}, {"a\rb", "x"}, {"", "a\rb"}} {
		if err := Write(io.Discard, bad.ev, []byte(bad.data)); !errors.Is(err, ErrBadFrame) {
			t.Fatalf("%q/%q: err = %v", bad.ev, bad.data, err)
		}
	}
}

func TestRead_CallbackOrderAndFinalFrame(t *testing.T) {
	var got []Frame
	err := Read(context.Background(), strings.NewReader("data: a\n\ndata: b"), 1024, func(f Frame) error {
		got = append(got, f)
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, []Frame{{Data: []byte("a")}, {Data: []byte("b")}}) {
		t.Fatalf("frames = %q, err = %v", got, err)
	}
}

func TestRead_StopsOnCallbackErrorAndTooLarge(t *testing.T) {
	stop := errors.New("stop")
	n := 0
	err := Read(context.Background(), strings.NewReader(strings.Repeat("data: a\n\n", 50)), 1024, func(Frame) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("err = %v after %d frames", err, n)
	}
	err = Read(context.Background(), strings.NewReader(strings.Repeat("x", 1<<20)), 4096, func(Frame) error { return nil })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

// endless never ends and never sends a line ending or a frame end.
type endless struct{ chunk []byte }

func (e endless) Read(p []byte) (int, error) { return copy(p, e.chunk), nil }

func TestRead_EndlessStreams(t *testing.T) {
	// A single endless line is refused at the limit instead of being buffered.
	err := Read(context.Background(), endless{[]byte("xxxxxxxx")}, 1<<16, func(Frame) error { return nil })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	// An endless run of keep-alives (a stream that never finishes) ends with the context.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = Read(ctx, endless{[]byte(": ping\n\n")}, 1<<16, func(Frame) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestRead_NoGoroutineLeftBehind(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		pr, pw := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- Read(ctx, pr, 1024, func(Frame) error { return nil })
		}()
		if _, err := pw.Write([]byte("data: a\n\ndata: half")); err != nil {
			t.Fatal(err)
		}
		switch i % 3 {
		case 0: // the upstream body is closed early
			pr.CloseWithError(errors.New("closed early"))
		case 1: // the upstream ends
			pw.Close()
		default: // the request ends; the transport then closes the body
			cancel()
			pr.CloseWithError(context.Canceled)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Read did not return")
		}
		cancel()
		pw.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

func FuzzParser(f *testing.F) {
	f.Add([]byte(sample), 3)
	f.Add([]byte(strings.ReplaceAll(sample, "\n", "\r\n")), 1)
	f.Add([]byte("data: [DONE]"), 100)
	f.Add([]byte("\xEF\xBB\xBFdata\r\r\n\n:\n"), 2)
	f.Fuzz(func(t *testing.T, in []byte, step int) {
		const limit = 256
		if step < 1 {
			step = 1
		}
		whole := NewParser(limit)
		want, wantErr := whole.Feed(in)
		if wantErr == nil {
			want = append(want, whole.Flush()...)
		}
		p := NewParser(limit)
		var got []Frame
		var err error
		for i := 0; i < len(in) && err == nil; i += step {
			var fs []Frame
			fs, err = p.Feed(in[i:min(i+step, len(in))])
			got = append(got, fs...)
			if cap(p.line) > 2*limit+step || cap(p.data) > 2*limit {
				t.Fatalf("buffers grew: line %d data %d", cap(p.line), cap(p.data))
			}
		}
		if err == nil {
			got = append(got, p.Flush()...)
		}
		if (err != nil) != (wantErr != nil) {
			t.Fatalf("whole err %v, pieces err %v", wantErr, err)
		}
		if len(got) != len(want) {
			t.Fatalf("whole %d frames, pieces %d", len(want), len(got))
		}
		for i := range got {
			if got[i].Event != want[i].Event || !bytes.Equal(got[i].Data, want[i].Data) {
				t.Fatalf("frame %d differs", i)
			}
			if len(got[i].Data)+len(got[i].Event) > limit {
				t.Fatalf("frame %d over the limit", i)
			}
			if bytes.IndexByte(got[i].Data, '\r') >= 0 || strings.ContainsAny(got[i].Event, "\r\n") {
				continue // Write refuses these
			}
			// What the parser yields can be written and read back.
			var buf bytes.Buffer
			if werr := Write(&buf, got[i].Event, got[i].Data); werr != nil {
				t.Fatal(werr)
			}
			back, berr := NewParser(4*limit + 64).Feed(buf.Bytes())
			if berr != nil || len(back) != 1 || back[0].Event != got[i].Event || !bytes.Equal(back[0].Data, got[i].Data) {
				t.Fatalf("frame %d does not round-trip: %q -> %q (%v)", i, got[i], back, berr)
			}
		}
	})
}
