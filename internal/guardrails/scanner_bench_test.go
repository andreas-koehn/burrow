package guardrails

import (
	"fmt"
	"strings"
	"testing"
)

// benchCustom is a realistic set of operator patterns for the benchmark.
func benchCustom() []CustomPattern {
	out := make([]CustomPattern, 10)
	for i := range out {
		out[i] = CustomPattern{ID: fmt.Sprintf("c%d", i), Pattern: fmt.Sprintf(`(?i)\bproject[ -]codename-%d\b|TICKET-%d\d{4}`, i, i), Direction: DirectionAnswer}
	}
	return out
}

// BenchmarkStreamScannerAdd measures one Add of a token-sized delta once the
// window is full: the cost paid on every streamed event. "bundled" runs the
// bundled answer patterns only, "custom10" adds ten custom answer patterns.
func BenchmarkStreamScannerAdd(b *testing.B) {
	for _, bc := range []struct {
		name   string
		custom []CustomPattern
	}{{"bundled", nil}, {"custom10", benchCustom()}} {
		b.Run(bc.name, func(b *testing.B) {
			s := benchScanner(NewEngine(), bc.custom)
			prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 120)
			s.Add(prose) // fill the window
			deltas := strings.Fields(prose)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.Add(deltas[i%len(deltas)] + " ")
			}
		})
	}
}

// benchScanner is the scanner the answer guard builds: bundled patterns on
// every delta, custom ones (when there are any) on the whole window now and
// then.
func benchScanner(e *Engine, custom []CustomPattern) *StreamScanner {
	var full func(string) (bool, string)
	if len(custom) > 0 {
		full = func(t string) (bool, string) { return e.InspectAnswerCustom(t, custom) }
	}
	return NewStreamScanner(e.InspectAnswerBundled, full)
}
