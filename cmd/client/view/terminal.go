package view

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// drawInterval is the shortest time between two writes: four per second.
const drawInterval = 250 * time.Millisecond

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// Size returns the columns and rows of the terminal f, and 0 for what is not
// known.
func Size(f *os.File) (cols, rows int) {
	if f == nil {
		return 0, 0
	}
	c, r, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, 0
	}
	return max(c, 0), max(r, 0)
}

// Colour reports whether output to f may be coloured: f is a terminal and
// NO_COLOR is not set.
func Colour(f *os.File, getenv func(string) string) bool {
	return IsTerminal(f) && colourAllowed(getenv)
}

func colourAllowed(getenv func(string) string) bool { return getenv("NO_COLOR") == "" }

// Screen redraws a view in place on a terminal.
//
// It uses no alternate screen, hides no cursor and changes no input mode, so
// there is nothing to restore when the process ends, however it ends. One
// mode of the terminal it does set: line wrapping is turned off before each
// line and on after it, within the same write. A terminal that had wrapping
// turned off is therefore left with it on, which is what terminals start with.
//
// Draw and Close may block for as long as the terminal does not take the
// output; call them from a goroutine nothing else waits for.
type Screen struct {
	w        io.Writer
	size     func() (cols, rows int)
	interval time.Duration

	mu        sync.Mutex
	pending   []string // what to draw next; nil = nothing
	drawn     []string // what is on the terminal
	drawnCols int      // the width it was drawn at
	lastWrite time.Time
	timer     *time.Timer
	closed    bool
	failed    any // what a write of the timer panicked with, until Draw passes it on
}

// NewScreen returns a Screen that writes to w. size reports the columns and
// rows of the terminal behind w, 0 for what is not known; it is asked before
// every write, which is how a resize is noticed.
func NewScreen(w io.Writer, size func() (cols, rows int)) *Screen {
	return &Screen{w: w, size: size, interval: drawInterval}
}

// Draw replaces what was drawn before with lines. At most four draws a second
// reach the terminal; of the ones in between only the last is drawn, a little
// later. Lines that are already on the terminal are not written again.
//
// A write that was put off is made by a timer. Should it panic (the writer and
// the size function are the caller's), the Screen closes itself and the next
// Draw panics with the same value, on the goroutine of its caller, which is
// where a caller can recover.
func (s *Screen) Draw(lines []string) {
	lines = append(make([]string, 0, len(lines)), lines...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.failed; r != nil {
		s.failed = nil
		panic(r)
	}
	if s.closed {
		return
	}
	s.pending = lines
	if s.timer != nil {
		return // a write is due and will take these lines
	}
	if wait := s.interval - time.Since(s.lastWrite); !s.lastWrite.IsZero() && wait > 0 {
		s.timer = time.AfterFunc(wait, s.tick)
		return
	}
	s.flush(false)
}

func (s *Screen) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// This is a goroutine of the timer: a panic here would end the process,
	// and with it the tunnels.
	defer func() {
		if r := recover(); r != nil {
			s.closed, s.failed = true, r
		}
	}()
	s.timer = nil
	if !s.closed {
		s.flush(false)
	}
}

// Close draws the last state once more, which also removes what a key press
// has left below the view, and leaves the cursor on the line below it.
// Nothing is drawn after Close.
func (s *Screen) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.pending == nil {
		s.pending = s.drawn
	}
	s.flush(true)
	s.closed = true
}

// flush writes the pending lines in one Write. s.mu is held.
func (s *Screen) flush(force bool) {
	lines := s.pending
	s.pending = nil
	if lines == nil {
		return
	}
	cols, rows := s.size()
	if rows <= 0 {
		rows = defaultRows // not known: see defaultRows
	}
	// The view must leave the last row to the cursor: going up beyond the top
	// of the screen is not possible, and the view would scroll with each draw.
	if rows > 0 && len(lines) > max(rows-1, 1) {
		lines = lines[:max(rows-1, 1)]
	}
	resized := cols != s.drawnCols
	if !force && !resized && s.drawn != nil && slices.Equal(lines, s.drawn) {
		return
	}

	up := s.rowsOnScreen(cols)
	if rows > 0 && up > rows-1 {
		up = max(rows-1, 0)
	}
	var b strings.Builder
	b.WriteString("\r")
	if up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	if force || resized || up != len(lines) {
		b.WriteString("\x1b[J") // nothing of the old view stays below the new one
	}
	for _, l := range lines {
		b.WriteString("\x1b[2K\x1b[?7l")
		b.WriteString(oneLine(l))
		b.WriteString("\x1b[?7h\r\n")
	}
	if _, err := io.WriteString(s.w, b.String()); err != nil {
		// The terminal is gone; writing more would only fail again.
		s.closed = true
		return
	}
	s.drawn, s.drawnCols, s.lastWrite = lines, cols, time.Now()
}

// rowsOnScreen is how many rows the lines drawn last take up now. That is one
// each, unless the terminal has become narrower since: most terminals then
// wrap the lines that no longer fit.
func (s *Screen) rowsOnScreen(cols int) int {
	if cols <= 0 || s.drawnCols <= 0 || cols >= s.drawnCols {
		return len(s.drawn)
	}
	n := 0
	for _, l := range s.drawn {
		visible := min(length(stripSGR(l)), s.drawnCols) // what did not fit was not drawn
		n += max((visible+cols-1)/cols, 1)
	}
	return n
}

// oneLine keeps a line from moving the cursor to another row.
func oneLine(l string) string {
	if !strings.ContainsAny(l, "\r\n") {
		return l
	}
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(l)
}
