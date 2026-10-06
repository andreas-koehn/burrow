package view

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// writes records every Write call on its own.
type writes struct {
	mu  sync.Mutex
	all []string
	err error
}

func (w *writes) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.all = append(w.all, string(p))
	return len(p), nil
}

func (w *writes) list() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.all...)
}

func (w *writes) waitFor(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if l := w.list(); len(l) >= n {
			return l
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%d writes, want %d", len(w.list()), n)
	return nil
}

func fixedSize(cols, rows int) func() (int, int) { return func() (int, int) { return cols, rows } }

// fastScreen is a Screen that waits 10 ms between writes instead of 250.
func fastScreen(w *writes, size func() (int, int)) *Screen {
	s := NewScreen(w, size)
	s.interval = 10 * time.Millisecond
	return s
}

func TestScreen_SecondDrawRewritesInPlace(t *testing.T) {
	w := &writes{}
	s := fastScreen(w, fixedSize(80, 24))
	defer s.Close()
	s.Draw([]string{"one", "two"})
	first := w.waitFor(t, 1)[0]
	if strings.Contains(first, "A") || !strings.Contains(first, "one") || !strings.Contains(first, "two") {
		t.Fatalf("first draw: %q", first)
	}
	s.Draw([]string{"uno", "dos"})
	second := w.waitFor(t, 2)[1]
	if !strings.HasPrefix(second, "\r\x1b[2A") {
		t.Fatalf("second draw does not start by going up two lines: %q", second)
	}
	if n := strings.Count(second, "\x1b[2K"); n != 2 {
		t.Fatalf("%d lines were cleared before rewriting, want 2: %q", n, second)
	}
	if strings.Index(second, "\x1b[2K") > strings.Index(second, "uno") || !strings.Contains(second, "dos") {
		t.Fatalf("second draw: %q", second)
	}
	if !strings.HasSuffix(second, "\r\n") {
		t.Fatalf("the cursor is not left below the view: %q", second)
	}
}

func TestScreen_FewerLinesClearTheRest(t *testing.T) {
	w := &writes{}
	s := fastScreen(w, fixedSize(80, 24))
	defer s.Close()
	s.Draw([]string{"one", "two", "three"})
	w.waitFor(t, 1)
	s.Draw([]string{"only"})
	second := w.waitFor(t, 2)[1]
	up, erase, text := strings.Index(second, "\x1b[3A"), strings.Index(second, "\x1b[J"), strings.Index(second, "only")
	if up < 0 || erase < up || text < erase {
		t.Fatalf("want up three lines, erase to the end of the screen, then the line: %q", second)
	}
	// and the next draw goes up by one line only
	s.Draw([]string{"again"})
	if third := w.waitFor(t, 3)[2]; !strings.HasPrefix(third, "\r\x1b[1A") {
		t.Fatalf("third draw: %q", third)
	}
}

func TestScreen_AtMostFourDrawsPerSecond(t *testing.T) {
	w := &writes{}
	s := NewScreen(w, fixedSize(80, 24))
	defer s.Close()
	start := time.Now()
	for i := 0; i < 100; i++ {
		s.Draw([]string{fmt.Sprintf("state %d", i)})
		time.Sleep(time.Millisecond)
	}
	if d := time.Since(start); d < 240*time.Millisecond {
		if n := len(w.list()); n > 2 {
			t.Fatalf("%d writes for 100 draws in %v", n, d)
		}
	}
	time.Sleep(400 * time.Millisecond)
	all := w.list()
	if len(all) != 2 && time.Since(start) < 700*time.Millisecond {
		t.Fatalf("%d writes, want 2 (the first draw and the last state)", len(all))
	}
	if last := all[len(all)-1]; !strings.Contains(last, "state 99") {
		t.Fatalf("the last state was not drawn: %q", last)
	}
}

func TestScreen_SameLinesAreNotWrittenAgain(t *testing.T) {
	w := &writes{}
	s := fastScreen(w, fixedSize(80, 24))
	defer s.Close()
	s.Draw([]string{"one"})
	w.waitFor(t, 1)
	s.Draw([]string{"one"})
	time.Sleep(40 * time.Millisecond)
	if n := len(w.list()); n != 1 {
		t.Fatalf("%d writes for the same lines", n)
	}
}

func TestScreen_Close(t *testing.T) {
	w := &writes{}
	s := NewScreen(w, fixedSize(80, 24)) // 250 ms: the second draw is still waiting at Close
	s.Draw([]string{"one", "two"})
	s.Draw([]string{"final one", "final two"})
	s.Close()
	all := w.list()
	if len(all) != 2 {
		t.Fatalf("%d writes, want the first draw and the final state", len(all))
	}
	last := all[1]
	if !strings.Contains(last, "final one") || !strings.Contains(last, "final two") || !strings.HasSuffix(last, "final two\x1b[?7h\r\n") {
		t.Fatalf("final state: %q", last)
	}
	s.Draw([]string{"late"})
	s.Close()
	time.Sleep(300 * time.Millisecond)
	if n := len(w.list()); n != 2 {
		t.Fatalf("%d writes after Close", n-2)
	}
	// Nothing changes the terminal for longer than one write: every write
	// that turns line wrapping off turns it on again, there is no alternate
	// screen and the cursor is never hidden.
	for _, p := range all {
		if strings.Count(p, "\x1b[?7l") != strings.Count(p, "\x1b[?7h") {
			t.Fatalf("line wrapping is not restored within the write: %q", p)
		}
		for _, bad := range []string{"\x1b[?1049", "\x1b[?47", "\x1b[?25l", "\x1b7", "\x1b[s"} {
			if strings.Contains(p, bad) {
				t.Fatalf("%q in %q", bad, p)
			}
		}
	}
}

func TestScreen_NarrowerTerminalCountsTheWrappedRows(t *testing.T) {
	cols := 80
	var mu sync.Mutex
	size := func() (int, int) { mu.Lock(); defer mu.Unlock(); return cols, 0 }
	w := &writes{}
	s := fastScreen(w, size)
	defer s.Close()
	// 50 visible characters each; the colour sequences do not count
	s.Draw([]string{"\x1b[33m" + strings.Repeat("x", 50) + "\x1b[0m", strings.Repeat("y", 50), "z"})
	w.waitFor(t, 1)
	mu.Lock()
	cols = 20 // the terminal re-wraps what is on it: 3 + 3 + 1 rows
	mu.Unlock()
	s.Draw([]string{"a", "b", "c"})
	second := w.waitFor(t, 2)[1]
	if !strings.HasPrefix(second, "\r\x1b[7A\x1b[J") {
		t.Fatalf("after shrinking to 20 columns: %q", second)
	}
	// wider again: nothing was wrapped, three lines up
	mu.Lock()
	cols = 120
	mu.Unlock()
	s.Draw([]string{"d", "e", "f"})
	if third := w.waitFor(t, 3)[2]; !strings.HasPrefix(third, "\r\x1b[3A") {
		t.Fatalf("after widening: %q", third)
	}
}

func TestScreen_ResizeAloneRedraws(t *testing.T) {
	cols := 80
	var mu sync.Mutex
	size := func() (int, int) { mu.Lock(); defer mu.Unlock(); return cols, 24 }
	w := &writes{}
	s := fastScreen(w, size)
	defer s.Close()
	s.Draw([]string{"one"})
	w.waitFor(t, 1)
	mu.Lock()
	cols = 40
	mu.Unlock()
	s.Draw([]string{"one"})
	w.waitFor(t, 2)
}

func TestScreen_NeverTallerThanTheTerminal(t *testing.T) {
	w := &writes{}
	s := fastScreen(w, fixedSize(80, 4))
	defer s.Close()
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	s.Draw(lines)
	first := w.waitFor(t, 1)[0]
	if strings.Count(first, "\r\n") != 3 || !strings.Contains(first, "line 2") || strings.Contains(first, "line 3") {
		t.Fatalf("10 lines on 4 rows: %q", first)
	}
	s.Draw(lines[1:])
	if second := w.waitFor(t, 2)[1]; !strings.HasPrefix(second, "\r\x1b[3A") {
		t.Fatalf("second draw: %q", second)
	}
}

func TestScreen_LineBreaksInsideALineCannotMoveTheCursor(t *testing.T) {
	w := &writes{}
	s := fastScreen(w, fixedSize(80, 24))
	defer s.Close()
	s.Draw([]string{"a\nb\rc"})
	first := w.waitFor(t, 1)[0]
	if strings.Count(first, "\n") != 1 || strings.Count(first, "\r") != 2 {
		t.Fatalf("draw: %q", first)
	}
}

func TestScreen_AWriteErrorEndsTheDrawing(t *testing.T) {
	w := &writes{err: errors.New("terminal gone")}
	s := fastScreen(w, fixedSize(80, 24))
	s.Draw([]string{"one"})
	s.Draw([]string{"two"})
	time.Sleep(30 * time.Millisecond)
	w.mu.Lock()
	w.err = nil
	w.mu.Unlock()
	s.Draw([]string{"three"})
	s.Close()
	if n := len(w.list()); n != 0 {
		t.Fatalf("%d writes after the terminal failed", n)
	}
}

func TestScreen_ConcurrentDraws(t *testing.T) {
	w := &writes{}
	s := fastScreen(w, fixedSize(80, 24))
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Draw([]string{fmt.Sprintf("g%d i%d", g, i)})
			}
		}(g)
	}
	wg.Wait()
	s.Close()
}

func TestTerminal_AFileIsNotATerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Fatal("a regular file was taken for a terminal")
	}
	if got := Width(f); got != 80 {
		t.Fatalf("width of a file = %d, want the fallback 80", got)
	}
	if cols, rows := Size(f); cols != 0 || rows != 0 {
		t.Fatalf("size of a file = %d x %d, want unknown", cols, rows)
	}
	env := map[string]string{}
	if Colour(f, func(k string) string { return env[k] }) {
		t.Fatal("colour for a file")
	}
}

func TestColourAllowed(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if !colourAllowed(get) {
		t.Fatal("colour is off without NO_COLOR")
	}
	env["NO_COLOR"] = "1"
	if colourAllowed(get) {
		t.Fatal("NO_COLOR was not honoured")
	}
}

// A height that is not known: the view is cut as on 24 rows, so that a tall
// one does not scroll the terminal with every redraw.
func TestScreen_UnknownHeightIs24Rows(t *testing.T) {
	w := &writes{}
	s := fastScreen(w, fixedSize(80, 0))
	defer s.Close()
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	s.Draw(lines)
	first := w.waitFor(t, 1)[0]
	if strings.Count(first, "\r\n") != 23 || !strings.Contains(first, "line 22") || strings.Contains(first, "line 23") {
		t.Fatalf("40 lines on an unknown height: %d lines drawn", strings.Count(first, "\r\n"))
	}
	lines[0] = "changed"
	s.Draw(lines)
	if second := w.waitFor(t, 2)[1]; !strings.HasPrefix(second, "\r\x1b[23A") {
		t.Fatalf("second draw: %q", second[:20])
	}
}

// A draw that was put off is written by a timer, on a goroutine of its own. A
// fault there (the size, the writer) must not end the process: the tunnels
// are worth more than their display. The Screen stops drawing and hands the
// fault to the next caller of Draw.
func TestScreen_APanicInADeferredDrawStaysInTheScreen(t *testing.T) {
	w := &writes{}
	calls := 0
	var mu sync.Mutex
	s := fastScreen(w, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		if calls++; calls > 1 {
			panic("size failed")
		}
		return 80, 24
	})
	s.Draw([]string{"one"})
	s.Draw([]string{"two"}) // put off: the timer draws it and the size panics
	time.Sleep(100 * time.Millisecond)
	if n := len(w.list()); n != 1 {
		t.Fatalf("%d writes", n)
	}
	got := func() (r any) {
		defer func() { r = recover() }()
		s.Draw([]string{"three"})
		return nil
	}()
	if got != "size failed" {
		t.Fatalf("the next Draw panicked with %v, want the fault of the timer", got)
	}
	// Once told, the Screen is simply closed.
	s.Draw([]string{"four"})
	s.Close()
	if n := len(w.list()); n != 1 {
		t.Fatalf("%d writes after the fault", n)
	}
}
