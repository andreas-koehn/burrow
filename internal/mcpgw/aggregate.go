package mcpgw

// aggregate.go — /mcp: the tools of every enabled source behind one endpoint
// (D11). A tool is named "<slug>__<tool>", with the rename override applied
// to the tool part, so two sources can never offer the same name. The key's
// allow-list ("<slug>__*" or "<slug>__<tool>") is matched against exactly
// that name. A tool whose aggregated name is unusable (over 64 characters,
// characters outside [A-Za-z0-9_-]) or in a name conflict is left off.
// Calls run through the pipeline of Call, on the source the name belongs to.

import (
	"context"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/db"
)

// aggregatedTool is a tool of one source with the name it has on /mcp.
type aggregatedTool struct {
	src  db.MCPSource
	tool Tool   // as served on /mcp/<slug>; what Call runs
	name string // "<slug>__<served name>"
}

// ServeAggregated serves /mcp. The order is that of ServeSource: origin
// check, the gateway key or OAuth token (401), the body, then the tools are
// read (two queries whatever the number of sources) and the SDK serves the
// request. There is no source to be unknown: a key that names nothing sees
// an empty list.
func (g *Gateway) ServeAggregated(w http.ResponseWriter, r *http.Request) {
	caller, ok := g.perimeter(w, r, "/mcp")
	if !ok {
		return
	}
	info := requestInfo(r)
	g.serveSDK(w, r, func() (*mcp.Server, error) {
		mine, all, unserved, err := g.aggregatedFor(r.Context(), caller)
		if err != nil {
			g.log.Error("mcp gateway: aggregated tools not read", "err", err)
			return nil, err
		}
		srv := NewServer()
		for _, a := range mine {
			named := a.tool
			named.Name = a.name
			g.AddTool(srv, named, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return g.Call(ctx, CallRequest{Caller: caller, Source: a.src, Tool: a.tool, Arguments: req.Params.Arguments, Info: info, AggregatedName: a.name})
			})
		}
		byName := make(map[string]aggregatedTool, len(all))
		for _, a := range all {
			byName[a.name] = a
		}
		srv.AddReceivingMiddleware(g.denied(caller, info, func(ctx context.Context, name string) (db.MCPSource, Tool, string, bool) {
			if a, found := byName[name]; found {
				return a.src, a.tool, a.name, true
			}
			// A source the key never named was not read for the list; a
			// call of one of its tools is looked up now, so that it is
			// recorded as denied like any other.
			return g.lookupAggregated(ctx, name)
		}, func(_ context.Context, name string) (db.MCPSource, unservedTool, string, bool) {
			slug, tool, ok := strings.Cut(name, "__")
			if !ok {
				return db.MCPSource{}, unservedTool{}, "", false
			}
			s, found := unserved[slug]
			if !found {
				return db.MCPSource{}, unservedTool{}, "", false
			}
			u, found := s.tools[tool]
			return s.src, u, name, found
		}))
		return srv, nil
	})
}

// unservedOf is what one source of /mcp has but does not serve.
type unservedOf struct {
	src   db.MCPSource
	tools map[string]unservedTool
}

// AggregatedFor returns the tools of every enabled source that c may see
// under their aggregated names, and every tool the aggregated endpoint
// offers to anybody (for telling a denied call from an unknown one). Both are
// ordered by source slug, then served name. A source the key's allow-list
// cannot name is not read at all.
func (g *Gateway) AggregatedFor(ctx context.Context, c Caller) (mine, all []aggregatedTool, err error) {
	mine, all, _, err = g.aggregatedFor(ctx, c)
	return mine, all, err
}

// aggregatedFor is AggregatedFor that also returns, by slug, the tools the
// named sources have but do not serve. It reads the sources and then the
// tools of every named source in one query.
func (g *Gateway) aggregatedFor(ctx context.Context, c Caller) (mine, all []aggregatedTool, unserved map[string]unservedOf, err error) {
	if len(c.AllowedTools) == 0 {
		return nil, nil, nil, nil
	}
	sources, err := g.opts.Store.ListMCPSources(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var named []db.MCPSource
	slugs := []string{}
	for _, src := range sources {
		if src.Enabled && namesSource(c.AllowedTools, src.Slug) {
			named = append(named, src)
			slugs = append(slugs, src.Slug)
		}
	}
	if len(named) == 0 {
		return nil, nil, nil, nil
	}
	rows, err := g.opts.Store.ListMCPToolsForSlugs(ctx, slugs)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, src := range named {
		st := g.toolsFrom(src, rows[src.Slug], c)
		smine, sall := st.mine, st.all
		if len(st.unserved) > 0 {
			if unserved == nil {
				unserved = map[string]unservedOf{}
			}
			unserved[src.Slug] = unservedOf{src: src, tools: st.unserved}
		}
		for _, t := range sall {
			if name, issue := AggregatedName(src.Slug, t.Name); issue == "" {
				all = append(all, aggregatedTool{src: src, tool: t, name: name})
			}
		}
		for _, t := range smine {
			if name, issue := AggregatedName(src.Slug, t.Name); issue == "" {
				mine = append(mine, aggregatedTool{src: src, tool: t, name: name})
			}
		}
	}
	return mine, all, unserved, nil
}

// lookupAggregated finds the tool an aggregated name stands for among what
// its source serves to anybody, reading that source only. Slugs have no "_",
// so the name splits at its first "__".
func (g *Gateway) lookupAggregated(ctx context.Context, name string) (db.MCPSource, Tool, string, bool) {
	slug, _, ok := strings.Cut(name, "__")
	if !ok {
		return db.MCPSource{}, Tool{}, "", false
	}
	src, err := g.source(ctx, slug)
	if err != nil {
		return db.MCPSource{}, Tool{}, "", false
	}
	_, all, err := g.ToolsFor(ctx, src, Caller{})
	if err != nil {
		g.log.Warn("mcp gateway: tools not read for a denied call", "source", slug, "err", err)
		return db.MCPSource{}, Tool{}, "", false
	}
	for _, t := range all {
		if agg, issue := AggregatedName(slug, t.Name); issue == "" && agg == name {
			return src, t, agg, true
		}
	}
	return db.MCPSource{}, Tool{}, "", false
}

// namesSource reports whether an entry of the allow-list can match a tool of
// slug at all, so that a key is not made to read sources it never named.
func namesSource(allowed []string, slug string) bool {
	prefix := slug + "__"
	for _, e := range allowed {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
