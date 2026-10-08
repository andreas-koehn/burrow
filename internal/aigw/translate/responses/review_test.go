package responses

import (
	"errors"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

func TestStreamEncoder_AFailedWriteOfTheFailureIsTheWritersError(t *testing.T) {
	// The encoder refuses an event and ends the stream itself. When the client is gone by then,
	// that is what the caller has to learn: ErrSequence or ErrLimit would say "the answer was
	// ended with response.failed", and the upstream would be read to its end for nobody.
	for name, c := range map[string]struct {
		events []ir.Event
		writes int // the writes that succeed
	}{
		"a sequence error":                  {[]ir.Event{startEv(), textDelta(3, "x")}, 2},
		"a sequence error, no write at all": {[]ir.Event{textDelta(3, "x")}, 0},
		"a limit":                           {[]ir.Event{startEv(), toolStart(0, "a", "f"), argsDelta(0, strings.Repeat("x", ir.MaxToolArgsBytes+1))}, 3},
		"a limit on an item's text":         {[]ir.Event{startEv(), textStart(0), textDelta(0, strings.Repeat("x", MaxItemBytes)), textDelta(0, "y")}, 5},
	} {
		w := &failingWriter{left: c.writes}
		e := NewStreamEncoder(w, "m", created)
		e.now = frozen
		var err error
		for _, event := range c.events {
			err = e.Write(event)
		}
		if err == nil || err.Error() != "client gone" || errors.Is(err, ir.ErrSequence) || errors.Is(err, ir.ErrLimit) || w.n != c.writes {
			t.Errorf("%s: err = %v after %d writes, want the writer's", name, err, w.n)
		}
		if cerr := e.Close(); cerr == nil || cerr.Error() != "client gone" {
			t.Errorf("%s: Close = %v", name, cerr)
		}
	}
	// With a writer that takes the frames, the encoder's own error stands.
	e := NewStreamEncoder(&failingWriter{left: 99}, "m", created)
	e.now = frozen
	_ = e.Write(startEv())
	if err := e.Write(textDelta(3, "x")); !errors.Is(err, ir.ErrSequence) {
		t.Fatalf("err = %v", err)
	}
}
