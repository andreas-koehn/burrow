package aigw

import (
	"context"
	"sync"
)

// GatewayKeySubjectPrefix marks a gateway key in a per-key subject (cache
// scope, rate-limit and quota subject): "gw:<gateway key id>". A service key's
// subject is its bare id. db.GatewayKeySubjectPrefix is the same string.
const GatewayKeySubjectPrefix = "gw:"

// Route describes how the gateway routed one request: which gateway key asked,
// which model name it asked for, and which provider and model answered. The
// gateway may still change the target while the request is being served (a
// fallback), so the chain reads it only when it writes the usage row.
type Route struct {
	mu             sync.Mutex
	gatewayKeyID   string
	dialect        string
	providerSlug   string
	requestedModel string
	targetModel    string
	requestID      string
}

// RouteInfo is an immutable copy of a Route.
type RouteInfo struct {
	GatewayKeyID, Dialect, ProviderSlug, RequestedModel, TargetModel, RequestID string
}

// NewRoute starts a route for a request; the target is set once it is known.
// dialect is the API format the caller spoke ("openai" or "anthropic").
func NewRoute(gatewayKeyID, dialect, requestedModel, requestID string) *Route {
	return &Route{gatewayKeyID: gatewayKeyID, dialect: dialect, requestedModel: requestedModel, requestID: requestID}
}

// SetTarget records the provider and model that serve (or last tried to serve) the request.
func (r *Route) SetTarget(providerSlug, targetModel string) {
	r.mu.Lock()
	r.providerSlug, r.targetModel = providerSlug, targetModel
	r.mu.Unlock()
}

// Snapshot returns the route as it is now.
func (r *Route) Snapshot() RouteInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RouteInfo{
		GatewayKeyID:   r.gatewayKeyID,
		Dialect:        r.dialect,
		ProviderSlug:   r.providerSlug,
		RequestedModel: r.requestedModel,
		TargetModel:    r.targetModel,
		RequestID:      r.requestID,
	}
}

type routeKey struct{}

// WithRoute attaches r to ctx.
func WithRoute(ctx context.Context, r *Route) context.Context {
	return context.WithValue(ctx, routeKey{}, r)
}

// RouteFrom returns the route attached to ctx, if any.
func RouteFrom(ctx context.Context) (RouteInfo, bool) {
	r, ok := ctx.Value(routeKey{}).(*Route)
	if !ok || r == nil {
		return RouteInfo{}, false
	}
	return r.Snapshot(), true
}
