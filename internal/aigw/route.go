package aigw

import (
	"context"
	"sync"

	"github.com/ankoehn/burrow/internal/aimeter"
)

// GatewayKeySubjectPrefix marks a gateway key in a per-key subject (cache
// scope, rate-limit and quota subject): "gw:<gateway key id>". A service key's
// subject is its bare id. db.GatewayKeySubjectPrefix is the same string.
const GatewayKeySubjectPrefix = "gw:"

// Route describes how the gateway routed one request: which gateway key asked,
// which model name it asked for (on a provider's own path: "<provider>/<id>",
// the name that model has on the gateway endpoints), and which provider and
// model answered. The
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
	fallback       bool
	translated     string
	dropped        string
	upstreamUsage  bool
	tokensIn       int
	tokensOut      int
}

// RouteInfo is an immutable copy of a Route.
type RouteInfo struct {
	GatewayKeyID, Dialect, ProviderSlug, RequestedModel, TargetModel, RequestID string
	// Fallback: the answer came from another target than the first one, the
	// one whose service the chain (and its cache) ran under.
	Fallback bool
	// Translated is the id of the pair that translated the request, "" when
	// it is served in its own format. Dropped names what the translation
	// left out: sorted, comma-separated, cleaned and bounded names.
	Translated, Dropped string
	// UpstreamUsage: the answer the caller got is not what the upstream
	// sent (it was translated), and TokensIn and TokensOut are what the
	// gateway read from the upstream's own bytes (see UpstreamUsage, the
	// type). The chain then records these and not what it reads itself.
	UpstreamUsage       bool
	TokensIn, TokensOut int
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

// MarkFallback records that the target answering is not the first target. The
// chain keeps such an answer out of the cache: the cache key is the first
// target's service and model, and the answer is neither's.
func (r *Route) MarkFallback() {
	r.mu.Lock()
	r.fallback = true
	r.mu.Unlock()
}

// SetTranslation records that the attempt being served is translated by pair
// (for example "messages-chat") and what the translation left out. dropped
// holds names only, never values; they come from the request, so they are
// stored cleaned and bounded (aimeter.JoinDropped). An attempt in the
// caller's own format calls SetTranslation("", nil), which clears both: the
// usage row describes the attempt that answered.
func (r *Route) SetTranslation(pair string, dropped []string) {
	pair = aimeter.CleanPair(pair)
	list := ""
	if pair != "" {
		list = aimeter.JoinDropped(dropped)
	}
	r.mu.Lock()
	r.translated, r.dropped = pair, list
	r.mu.Unlock()
}

// SetUpstreamUsage records the token counts of the answer as the upstream
// sent it, for an attempt whose answer reaches the caller in another format.
// The chain meters the caller's side of a response; with these set it writes
// them into the usage row instead of what it read there.
func (r *Route) SetUpstreamUsage(tokensIn, tokensOut int) {
	r.mu.Lock()
	r.upstreamUsage, r.tokensIn, r.tokensOut = true, tokensIn, tokensOut
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
		Fallback:       r.fallback,
		Translated:     r.translated,
		Dropped:        r.dropped,
		UpstreamUsage:  r.upstreamUsage,
		TokensIn:       r.tokensIn,
		TokensOut:      r.tokensOut,
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
