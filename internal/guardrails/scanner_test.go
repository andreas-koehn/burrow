package guardrails

import (
	"strings"
	"testing"
)

func bundledScanner(e *Engine) *StreamScanner {
	return NewStreamScanner(e.InspectAnswerBundled, nil)
}

func TestStreamScanner_KeySplitAcrossThreeDeltas(t *testing.T) {
	e := NewEngine()
	s := bundledScanner(e)
	// A full window of text before the key: the bundled check sees only the
	// tail, which still holds every part of it.
	s.Add(strings.Repeat("Some text. ", 500))
	key := fakeOpenAI
	parts := []string{"Here it is: " + key[:5], key[5:20], key[20:] + " done."}
	for i, p := range parts[:2] {
		if hit, id := s.Add(p); hit {
			t.Fatalf("delta %d hit early: %q", i, id)
		}
	}
	if hit, id := s.Add(parts[2]); !hit || id != "openai_key" {
		t.Fatalf("third delta: %v %q", hit, id)
	}
}

func TestStreamScanner_KeyInSingleCharacterDeltas(t *testing.T) {
	s := bundledScanner(NewEngine())
	text := "key: " + fakeAnthropic + " end"
	for i := 0; i < len(text); i++ {
		if hit, id := s.Add(text[i : i+1]); hit {
			if id != "anthropic_key" {
				t.Fatalf("hit %q", id)
			}
			return
		}
	}
	t.Fatal("a key sent one character at a time was not found")
}

func TestStreamScanner_Windows(t *testing.T) {
	var quick, full []string
	s := NewStreamScanner(
		func(text string) (bool, string) { quick = append(quick, text); return false, "" },
		func(text string) (bool, string) { full = append(full, text); return false, "" },
	)
	x := strings.Repeat("x", StreamWindow+1000)
	s.Add(x) // no window yet: both checks see the delta
	s.Add("abc")
	if len(quick) != 2 || quick[0] != x || quick[1] != strings.Repeat("x", ScanTailBundled)+"abc" {
		t.Fatalf("quick checks: %d, second of %d bytes; want the last %d bytes of the window plus the delta", len(quick), len(quick[1]), ScanTailBundled)
	}
	// The first delta reached ScanEveryCustom; the second (3 bytes) did not.
	if len(full) != 1 || full[0] != x {
		t.Fatalf("full checks: %d", len(full))
	}
	y := strings.Repeat("y", ScanEveryCustom-3)
	s.Add(y)
	if want := strings.Repeat("x", StreamWindow-3) + "abc" + y; len(full) != 2 || full[1] != want {
		t.Fatalf("full checks: %d; want one on the whole window plus the delta after %d bytes", len(full), ScanEveryCustom)
	}
	// Flush runs the full check on text that has not had one, once.
	s.Add("z")
	if hit, _ := s.Flush(); hit || len(full) != 3 || !strings.HasSuffix(full[2], "z") || len(full[2]) != StreamWindow {
		t.Fatalf("Flush: %d full checks", len(full))
	}
	if s.Flush(); len(full) != 3 {
		t.Fatal("a second Flush checked again")
	}
	if StreamWindow != 4096 || ScanTailBundled != 512 || ScanEveryCustom != 256 {
		t.Fatalf("limits %d %d %d", StreamWindow, ScanTailBundled, ScanEveryCustom)
	}
}

// A custom pattern is found within ScanEveryCustom bytes of text after it
// completed, or at Flush.
func TestStreamScanner_CustomPatterns(t *testing.T) {
	e := NewEngine()
	custom := []CustomPattern{{ID: "codename", Pattern: `project falcon`, Direction: DirectionAnswer}}
	full := func(text string) (bool, string) { return e.InspectAnswerCustom(text, custom) }
	s := NewStreamScanner(e.InspectAnswerBundled, full)
	s.Add(strings.Repeat("a ", 300)) // a full check ran; the counter is back at 0
	if hit, _ := s.Add("this is project "); hit {
		t.Fatal("hit before the pattern completed")
	}
	if hit, _ := s.Add("falcon."); hit {
		t.Fatal("custom pattern checked before the interval (it should wait)")
	}
	found := false
	for i := 0; i < ScanEveryCustom && !found; i += 8 {
		found, _ = s.Add("filler. ")
	}
	if !found {
		t.Fatalf("custom pattern not found within %d bytes", ScanEveryCustom)
	}
	s2 := NewStreamScanner(e.InspectAnswerBundled, full)
	s2.Add("project falcon")
	if hit, id := s2.Flush(); !hit || id != "codename" {
		t.Fatalf("Flush: %v %q", hit, id)
	}
}

func TestStreamScanner_CleanText(t *testing.T) {
	e := NewEngine()
	s := NewStreamScanner(e.InspectAnswerBundled, func(text string) (bool, string) { return e.InspectAnswerCustom(text, nil) })
	for _, d := range strings.Fields("The quick brown fox jumps over the lazy dog, and an API key starts with sk- usually.") {
		if hit, id := s.Add(d + " "); hit {
			t.Fatalf("clean text hit %q", id)
		}
	}
	if hit, id := s.Flush(); hit {
		t.Fatalf("clean text hit %q at Flush", id)
	}
}

func TestStreamScanner_SecretBeyondTheWindowIsForgotten(t *testing.T) {
	e := NewEngine()
	s := NewStreamScanner(e.InspectAnswerBundled, func(text string) (bool, string) { return e.InspectAnswerText(text, nil) })
	s.Add(fakeAWS[:10])
	s.Add(strings.Repeat(" ", StreamWindow))
	if hit, id := s.Add(fakeAWS[10:]); hit {
		t.Fatalf("hit %q across more than a window", id)
	}
}
