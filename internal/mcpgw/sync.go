package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

type jsonrpcError = jsonrpc.Error

func asJSONRPC(err error, out **jsonrpcError) bool { return errors.As(err, out) }

// Limits of one sync. An upstream that lists more tools, or keeps handing out
// cursors, fails the sync instead of filling the table.
const (
	MaxToolsPerSource = 1000
	maxListPages      = 100
)

var errTooManyTools = fmt.Errorf("the tool source lists more than %d tools", MaxToolsPerSource)

// SyncResult tells what a sync found.
type SyncResult struct {
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
	// Tools and Pending are the source's counts after the sync: tools that
	// are not removed, and those waiting for approval.
	Tools   int `json:"tool_count"`
	Pending int `json:"pending_count"`
	// Skipped names the tools that were not stored: an invalid or repeated
	// name, or a definition over MaxDefinitionBytes. Never nil.
	Skipped []string `json:"skipped"`
}

// Sync reads the source's tools from the upstream and stores them (D9): a
// new tool, or one whose definition changed, waits for approval and is not
// served until then; a tool that vanished is marked removed. Each
// definition's title, description and schemas go through the guardrail
// request patterns, and hits are stored with the tool. A failed sync records
// its reason (Describe) on the source and leaves the tools as they were.
// ErrSourceNotFound; ErrSyncRunning while another sync of the source runs.
func (p *Pool) Sync(ctx context.Context, slug string) (SyncResult, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return SyncResult{}, ErrClosed
	}
	if p.syncing[slug] {
		p.mu.Unlock()
		return SyncResult{}, ErrSyncRunning
	}
	p.syncing[slug] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.syncing, slug)
		p.mu.Unlock()
	}()

	src, err := p.opts.Store.MCPSourceBySlug(ctx, slug)
	if errors.Is(err, store.ErrMCPSourceNotFound) {
		return SyncResult{}, ErrSourceNotFound
	}
	if err != nil {
		return SyncResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.opts.SyncTimeout)
	defer cancel()

	tools, err := p.listTools(ctx, src)
	if err != nil {
		// The record is written even when the caller has gone.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer rcancel()
		if rerr := p.opts.Store.RecordMCPSyncError(rctx, slug, time.Now().UTC(), Describe(err)); rerr != nil && !errors.Is(rerr, store.ErrMCPSourceNotFound) {
			p.log.Warn("mcpgw: record sync error failed", "source", slug, "err", rerr)
		}
		return SyncResult{}, err
	}

	seen, skipped := p.observe(ctx, tools)
	warning := ""
	if len(skipped) > 0 {
		warning = fmt.Sprintf("%d tool(s) skipped (invalid or repeated name, or definition too large): %s",
			len(skipped), clip(strings.Join(skipped, ", "), 300))
	}
	counts, err := p.opts.Store.ApplyMCPToolSync(ctx, slug, seen, time.Now().UTC(), warning)
	if errors.Is(err, store.ErrMCPSourceNotFound) {
		return SyncResult{}, ErrSourceNotFound
	}
	if err != nil {
		return SyncResult{}, err
	}
	res := SyncResult{Added: counts.Added, Changed: counts.Changed, Removed: counts.Removed, Skipped: skipped}
	if after, err := p.opts.Store.MCPSourceBySlug(ctx, slug); err == nil {
		res.Tools, res.Pending = after.ToolCount, after.PendingCount
	}
	return res, nil
}

// listTools reads every page of the upstream's tool list.
func (p *Pool) listTools(ctx context.Context, src db.MCPSource) ([]*mcp.Tool, error) {
	var out []*mcp.Tool
	err := p.withSession(ctx, src, func(cs *mcp.ClientSession) error {
		out = out[:0]
		params := &mcp.ListToolsParams{}
		for page := 0; ; page++ {
			if page == maxListPages {
				return errTooManyTools
			}
			res, err := cs.ListTools(ctx, params)
			if err != nil {
				return err
			}
			out = append(out, res.Tools...)
			if len(out) > MaxToolsPerSource {
				return errTooManyTools
			}
			if res.NextCursor == "" {
				return nil
			}
			params = &mcp.ListToolsParams{Cursor: res.NextCursor}
		}
	})
	return out, err
}

// observe turns the upstream's tools into rows to store: canonical
// definition, hash and guardrail hits. Tools with an invalid or repeated
// name, or a definition that is too large, are skipped and named.
func (p *Pool) observe(ctx context.Context, tools []*mcp.Tool) ([]db.MCPToolSeen, []string) {
	seen := make([]db.MCPToolSeen, 0, len(tools))
	skipped := []string{}
	names := map[string]bool{}
	for _, t := range tools {
		if t == nil {
			continue
		}
		if !ValidToolName(t.Name) || names[t.Name] {
			skipped = append(skipped, clip(t.Name, 64))
			continue
		}
		def, hash, err := Canonical(t)
		if err != nil {
			skipped = append(skipped, t.Name)
			continue
		}
		names[t.Name] = true
		seen = append(seen, db.MCPToolSeen{Name: t.Name, Hash: hash, Definition: def, GuardrailHits: p.inspect(ctx, def)})
	}
	return seen, skipped
}

// inspect runs the text fields of a definition through the guardrail request
// patterns and returns the hits as stored JSON.
func (p *Pool) inspect(ctx context.Context, def string) string {
	hits := []GuardrailHit{}
	if p.opts.InspectText != nil {
		if d, err := ParseDefinition(def); err == nil {
			// Schemas and annotations are inspected as the text a model
			// reads: their keys and string values, unescaped, one per line.
			for _, f := range []struct {
				field string
				text  []byte
			}{
				{"title", []byte(d.Title)},
				{"description", []byte(d.Description)},
				{"input_schema", jsonText(d.InputSchema)},
				{"output_schema", jsonText(d.OutputSchema)},
				{"annotations", jsonText(d.Annotations)},
			} {
				if len(f.text) == 0 {
					continue
				}
				if hit, id := p.opts.InspectText(ctx, f.text); hit {
					hits = append(hits, GuardrailHit{Field: f.field, Pattern: id})
				}
			}
		}
	}
	b, _ := json.Marshal(hits)
	return string(b)
}

// jsonText returns the object keys and string values of a JSON document,
// decoded (no \u003c escapes), one per line.
func jsonText(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			b.WriteString(x)
			b.WriteByte('\n')
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				b.WriteString(k)
				b.WriteByte('\n')
				walk(e)
			}
		}
	}
	walk(v)
	return []byte(b.String())
}

// Start runs the background sync until ctx ends or the pool is closed: every
// SyncInterval (with jitter) each enabled source is synced, a few at a time,
// off any request path. The first round starts within a minute.
func (p *Pool) Start(ctx context.Context) {
	p.mu.Lock()
	if p.closed || p.stopLoop != nil {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	p.stopLoop = cancel
	p.bg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.bg.Done()
		first := time.Duration(rand.Int63n(int64(min(p.opts.SyncInterval, time.Minute)) + 1))
		t := time.NewTimer(first)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			p.syncAll(ctx)
			// ±10 % jitter so that relays and sources do not fall into step.
			jitter := time.Duration(rand.Int63n(int64(p.opts.SyncInterval)/5+1)) - p.opts.SyncInterval/10
			t.Reset(p.opts.SyncInterval + jitter)
		}
	}()
}

// backgroundWorkers is how many sources the background sync handles at once.
const backgroundWorkers = 4

func (p *Pool) syncAll(ctx context.Context) {
	srcs, err := p.opts.Store.ListMCPSources(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("mcpgw: background sync: list sources failed", "err", err)
		}
		return
	}
	sem := make(chan struct{}, backgroundWorkers)
	done := make(chan struct{})
	n := 0
	for _, s := range srcs {
		if !s.Enabled {
			continue
		}
		n++
		slug := s.Slug
		go func() {
			defer func() { done <- struct{}{} }()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			res, err := p.Sync(ctx, slug)
			switch {
			case errors.Is(err, ErrSyncRunning), errors.Is(err, ErrSourceNotFound), errors.Is(err, ErrClosed), ctx.Err() != nil:
			case err != nil:
				p.log.Info("mcpgw: background sync failed", "source", slug, "reason", Describe(err))
			case res.Added+res.Changed+res.Removed > 0 && p.opts.OnBackgroundSync != nil:
				p.opts.OnBackgroundSync(slug, res)
			}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
}
