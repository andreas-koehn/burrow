package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/audit"
)

// fakeChain is an audit chain that exports three records and the signature.
type fakeChain struct{ err error }

func (fakeChain) Verify(context.Context, string, string) (bool, string, error) { return true, "", nil }
func (fakeChain) PublicKey() []byte                                            { return nil }
func (fakeChain) FingerprintHex() string                                       { return "" }
func (c fakeChain) ExportNDJSON(_ context.Context, w io.Writer, _ audit.ExportQuery) error {
	if c.err != nil {
		return c.err
	}
	_, _ = io.WriteString(w, "{\"id\":\"1\"}\n{\"id\":\"2\"}\n")
	_, _ = io.WriteString(w, "{\"id\":\"3\"}\n{\"_signature\":\"x\"}\n")
	return nil
}

func TestExportAnnouncingChain(t *testing.T) {
	em := &recEmitter{}
	ctx := audit.WithLogContext(context.Background(), audit.LogContext{ActorEmail: "auditor@x"})
	var out bytes.Buffer
	if err := (exportAnnouncingChain{AuditChain: fakeChain{}, events: em}).ExportNDJSON(ctx, &out, audit.ExportQuery{}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 4 {
		t.Fatalf("the export was changed on its way: %q", out.String())
	}
	got := em.named("audit.exported")
	if len(got) != 1 || got[0]["ActorEmail"] != "auditor@x" || got[0]["RecordCount"] != 3 {
		t.Fatalf("audit.exported events: %v", got)
	}
	fieldsOf(t, "audit.exported", got[0])

	// A failed export is not announced; without a publisher nothing breaks.
	failing := exportAnnouncingChain{AuditChain: fakeChain{err: errors.New("boom")}, events: em}
	if err := failing.ExportNDJSON(ctx, io.Discard, audit.ExportQuery{}); err == nil {
		t.Fatal("the export's error was swallowed")
	}
	if err := (exportAnnouncingChain{AuditChain: fakeChain{}}).ExportNDJSON(ctx, io.Discard, audit.ExportQuery{}); err != nil {
		t.Fatal(err)
	}
	if n := len(em.named("audit.exported")); n != 1 {
		t.Fatalf("%d events after a failed export, want still 1", n)
	}
}
