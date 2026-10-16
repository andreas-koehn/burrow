package guardrails

import (
	"strings"
	"testing"
)

func TestStreamScanner_KeySplitAcrossThreeDeltas(t *testing.T) {
	e := NewEngine()
	s := NewStreamScanner(func(text string) (bool, string) { return e.InspectAnswerText(text, nil) })
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

func TestStreamScanner_KeepsOnlyTheTail(t *testing.T) {
	var seen []string
	s := NewStreamScanner(func(text string) (bool, string) {
		seen = append(seen, text)
		return false, ""
	})
	long := strings.Repeat("x", StreamWindow+1000)
	s.Add(long)
	s.Add("abc")
	if len(seen) != 2 {
		t.Fatalf("%d checks", len(seen))
	}
	if seen[0] != long {
		t.Fatal("the first check did not see the whole first delta")
	}
	want := strings.Repeat("x", StreamWindow) + "abc"
	if seen[1] != want {
		t.Fatalf("second check saw %d bytes, want the last %d of the first delta plus the new one", len(seen[1]), StreamWindow)
	}
	if StreamWindow != 4096 {
		t.Fatalf("window %d, want 4096", StreamWindow)
	}
}

func TestStreamScanner_CleanText(t *testing.T) {
	e := NewEngine()
	s := NewStreamScanner(func(text string) (bool, string) { return e.InspectAnswerText(text, nil) })
	for _, d := range strings.Fields("The quick brown fox jumps over the lazy dog, and an API key starts with sk- usually.") {
		if hit, id := s.Add(d + " "); hit {
			t.Fatalf("clean text hit %q", id)
		}
	}
}

func TestStreamScanner_SecretBeyondTheWindowIsForgotten(t *testing.T) {
	e := NewEngine()
	s := NewStreamScanner(func(text string) (bool, string) { return e.InspectAnswerText(text, nil) })
	// The first half of a key, then more than a window of other text, then
	// the second half: the halves are never in one window, so no hit.
	s.Add(fakeAWS[:10])
	s.Add(strings.Repeat(" ", StreamWindow))
	if hit, id := s.Add(fakeAWS[10:]); hit {
		t.Fatalf("hit %q across more than a window", id)
	}
}
