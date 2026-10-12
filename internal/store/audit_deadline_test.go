package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
)

// stuckSink is an audit log that does not answer until its context ends. It
// records the deadline of every write.
type stuckSink struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func (s *stuckSink) Append(ctx context.Context, _ any) error {
	dl, ok := ctx.Deadline()
	s.mu.Lock()
	if ok {
		s.deadlines = append(s.deadlines, dl)
	}
	s.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// One mutation has one deadline for all its audit rows: a SaveSettings with two
// audited keys and an audit log that never answers returns after about five
// seconds, not ten; every write sees at most five seconds; and the dropped
// writes are logged by action, without values.
func TestSaveSettings_OneAuditDeadlinePerMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the audit deadline")
	}
	s := newStore(t)
	sink := &stuckSink{}
	s.SetAuditLogger(sink)
	var mu sync.Mutex
	var logged []string
	old := auditWarn
	auditWarn = func(msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		line := msg
		for _, a := range args {
			line += " " + toString(a)
		}
		logged = append(logged, line)
	}
	t.Cleanup(func() { auditWarn = old })

	start := time.Now()
	err := s.SaveSettings(context.Background(), map[string]string{
		"guardrails.global": `{"secret":"VALUE-ONE"}`,
		"cache.settings":    `{"secret":"VALUE-TWO"}`,
	})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if took > 7*time.Second {
		t.Fatalf("SaveSettings took %v with a stuck audit log, want about 5s (one deadline)", took)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.deadlines) != 2 {
		t.Fatalf("%d audit writes, want 2", len(sink.deadlines))
	}
	for _, dl := range sink.deadlines {
		if dl.After(start.Add(auditWriteTimeout + time.Second)) {
			t.Errorf("a write had a deadline %v after the start, want at most %v", dl.Sub(start), auditWriteTimeout)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 2 {
		t.Fatalf("%d log lines, want one per dropped write: %v", len(logged), logged)
	}
	for _, l := range logged {
		if strings.Contains(l, "VALUE") || (!strings.Contains(l, audit.ActionGuardrailSettingsUpdate) && !strings.Contains(l, audit.ActionCacheSettingsUpdate)) {
			t.Errorf("log line %q: want the action name and no values", l)
		}
	}
}

func toString(a any) string {
	if s, ok := a.(string); ok {
		return s
	}
	return "?"
}
