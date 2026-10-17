package guardrails

// StreamWindow is how much of a streamed answer's text the scanner keeps
// between deltas: 4096 bytes. A secret split across deltas is found as long
// as its parts arrive within one window of each other.
const StreamWindow = 4096

// ScanTailBundled is how much of the window the bundled answer patterns are
// checked on, together with each new delta: 512 bytes. It is longer than the
// shortest match of every bundled pattern (the longest of those, a
// fine-grained GitHub token, is 31 bytes; a PEM header line is under 50), so a
// secret that a delta completes is still found: its
// shortest match ends in the delta and starts within the tail. Checking the
// tail instead of the whole window keeps the cost per delta small.
const ScanTailBundled = 512

// ScanEveryCustom is how many bytes of new text the scanner waits for before
// it checks the custom patterns again, on the whole window: 256. Their
// matches have no bound the scanner knows, so they need the whole window,
// and checking it on every token would cost too much. A custom pattern is
// therefore found up to 256 bytes of text after the text that completes it,
// or at Flush.
const ScanEveryCustom = 256

// StreamScanner screens the text of a streamed answer as it arrives, with
// two checks: a quick one (the bundled patterns) on the tail of the window
// plus every delta, and a full one (the custom patterns) on the whole window
// plus the delta, every ScanEveryCustom bytes and at Flush. A secret split
// across two or more frames is found by both. Not safe for use by several
// goroutines.
type StreamScanner struct {
	quick, full func(text string) (bool, string)
	window      []byte // the last StreamWindow bytes of the text so far
	unchecked   int    // bytes of text added since the last full check
}

// NewStreamScanner returns a scanner that runs quick on the tail of the
// window plus each delta, and full on the whole window plus the delta every
// ScanEveryCustom bytes of text and at Flush. Either may be nil.
func NewStreamScanner(quick, full func(text string) (bool, string)) *StreamScanner {
	return &StreamScanner{quick: quick, full: full}
}

// Add checks a new delta (see StreamScanner), then keeps the last
// StreamWindow bytes of the text as the new window. It returns the first
// check that matched.
func (s *StreamScanner) Add(delta string) (hit bool, id string) {
	if delta == "" {
		return false, ""
	}
	text := append(s.window, delta...)
	if s.quick != nil {
		tail := text[max(0, len(s.window)-ScanTailBundled):]
		hit, id = s.quick(string(tail))
	}
	s.unchecked += len(delta)
	if !hit && s.full != nil && s.unchecked >= ScanEveryCustom {
		hit, id = s.full(string(text))
		s.unchecked = 0
	}
	if len(text) > StreamWindow {
		// Copy, so that the backing array does not keep growing with the
		// stream.
		text = append([]byte(nil), text[len(text)-StreamWindow:]...)
	}
	s.window = text
	return hit, id
}

// Flush runs the full check on the window when text arrived since the last
// one. The caller calls it where the answer may end: an event without text,
// the end of the stream.
func (s *StreamScanner) Flush() (hit bool, id string) {
	if s.full == nil || s.unchecked == 0 {
		return false, ""
	}
	s.unchecked = 0
	return s.full(string(s.window))
}
