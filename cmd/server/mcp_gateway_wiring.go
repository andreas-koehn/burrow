package main

import (
	"context"
	"log/slog"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/guardrails"
	"github.com/ankoehn/burrow/internal/mcpgw"
	"github.com/ankoehn/burrow/internal/oauth"
	"github.com/ankoehn/burrow/internal/store"
)

// newMCPToolGateway builds the client side of the MCP gateway (/mcp/<slug>
// and /mcp) from the engines model traffic uses: the key validation with its
// owner checks, the authorization server's access tokens (nil: keys only),
// the quota engine's rate limits, the redaction and guardrail engines, the
// usage sink, the inspector, the audit log and the observer.
func newMCPToolGateway(st *store.Store, v04 *v04Stack, pool *mcpgw.Pool, oauthServer *oauth.Server, log *slog.Logger) *mcpgw.Gateway {
	var tokens mcpgw.OAuthTokens
	if oauthServer != nil { // a nil *oauth.Server must not become a non-nil interface
		tokens = oauthServer
	}
	return mcpgw.NewGateway(mcpgw.GatewayOptions{
		OAuth: tokens,
		Store: st, Keys: st, Upstream: pool, Limits: v04.QuotaEngine,
		Redact: v04.RedactEngine, Guardrails: v04.GuardrailsEngine,
		Policy:    mcpToolPolicy(v04.AIChain.Loader, v04.GlobalGuardrails, log),
		Meter:     v04.MeterSink,
		Inspector: v04.InspectorMgr,
		Audit:     v04.AuditAppender,
		Observer:  v04.Observer,
		Log:       log,
	})
}

// mcpToolPolicy reads the policy of a tool source's backing service from its
// AI config, as the chain does for model traffic: redaction and the
// inspector when the service turns them on, the guardrails that are
// effective for it (the global ones when it has no section of its own). A
// config that cannot be read leaves the global guardrails only, as the chain
// does.
func mcpToolPolicy(loader aigw.ConfigLoader, global func(context.Context) guardrails.Settings, log *slog.Logger) func(context.Context, string) mcpgw.Policy {
	return func(ctx context.Context, serviceID string) mcpgw.Policy {
		var cfg aigw.ServiceAIConfig
		if loader != nil {
			svc, ok, err := loader.LoadAIConfig(ctx, serviceID)
			if err != nil {
				log.Warn("mcp gateway: load ai config failed", "service_id", serviceID, "err", err)
			}
			if ok {
				cfg = svc.AIConfig
			}
		}
		if cfg.Guardrails == nil && global != nil {
			cfg.Guardrails = guardrails.Effective(nil, global(ctx))
		}
		p := mcpgw.Policy{Guardrails: cfg.Guardrails}
		if r := cfg.Redaction; r != nil && r.Enabled {
			p.Redact, p.RedactForLogsOnly = true, r.ForLogsOnly
		}
		if i := cfg.Inspector; i != nil && i.Enabled {
			p.Inspect, p.InspectMax = true, i.MaxRequests
		}
		return p
	}
}

// mcpGatewayParts is what the management API drives: the pool's sync, and
// on a source change or delete both the pool's session and the gateway's
// limits of that source are dropped.
type mcpGatewayParts struct {
	*mcpgw.Pool
	tools *mcpgw.Gateway
}

// Invalidate drops the source's upstream session and its idle limits.
func (p mcpGatewayParts) Invalidate(slug string) {
	p.Pool.Invalidate(slug)
	p.tools.Forget(slug)
}
