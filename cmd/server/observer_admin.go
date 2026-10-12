// observer_admin.go — the webhook event of an audit export. It wraps what
// main already wires.
package main

import (
	"context"
	"io"

	"github.com/ankoehn/burrow/internal/api"
	"github.com/ankoehn/burrow/internal/audit"
)

// exportAnnouncingChain is the audit chain of the API with one addition: a
// finished export is announced as "audit.exported", with who asked and how
// many records went out.
type exportAnnouncingChain struct {
	api.AuditChain
	events eventPublisher
}

func (c exportAnnouncingChain) ExportNDJSON(ctx context.Context, w io.Writer, q audit.ExportQuery) error {
	lines := &lineCounter{w: w}
	if err := c.AuditChain.ExportNDJSON(ctx, lines, q); err != nil {
		return err
	}
	if c.events != nil {
		records := lines.n - 1 // the last line is the signature
		if records < 0 {
			records = 0
		}
		c.events.Publish(context.Background(), "audit.exported", map[string]any{
			"ActorEmail": audit.LogContextFrom(ctx).ActorEmail, "RecordCount": records,
		})
	}
	return nil
}

// lineCounter counts the newlines written through it.
type lineCounter struct {
	w io.Writer
	n int
}

func (l *lineCounter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	for _, b := range p[:n] {
		if b == '\n' {
			l.n++
		}
	}
	return n, err
}
