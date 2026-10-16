package guardrails

// StreamWindow is how much of a streamed answer's text the scanner keeps
// between deltas: 4096 bytes. A secret split across deltas is found as long
// as its parts arrive within one window of each other.
const StreamWindow = 4096

// StreamScanner screens the text of a streamed answer as it arrives. Each
// delta is checked together with the window before it, so a secret split
// across two or more frames is found. Not safe for use by several
// goroutines.
type StreamScanner struct {
	check  func(text string) (bool, string)
	window []byte // the last StreamWindow bytes of the text so far
}

// NewStreamScanner returns a scanner that runs check on the window plus each
// new delta.
func NewStreamScanner(check func(text string) (bool, string)) *StreamScanner {
	return &StreamScanner{check: check}
}

// Add checks the window followed by delta, then keeps the last StreamWindow
// bytes of that as the new window. It returns what check returned.
func (s *StreamScanner) Add(delta string) (hit bool, id string) {
	if delta == "" {
		return false, ""
	}
	text := append(s.window, delta...)
	hit, id = s.check(string(text))
	if len(text) > StreamWindow {
		// Copy, so that the backing array does not keep growing with the
		// stream.
		text = append([]byte(nil), text[len(text)-StreamWindow:]...)
	}
	s.window = text
	return hit, id
}
