package aigateway

import (
	"context"
	"errors"
	"strings"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// Target is one place a request can be sent.
type Target struct {
	Provider db.AIProvider
	Model    string // native model id at that provider
}

// Resolution is the routing decision for a requested model name in one dialect.
type Resolution struct {
	Requested string     // the name the client asked for; allow-lists apply to this
	Dialect   string     // "openai" or "anthropic"
	Synthetic bool       // true when Requested named a synthetic model
	Model     db.AIModel // the synthetic model; zero value for a direct address
	Targets   []Target   // all speaking Dialect, in try order
	// Other holds a synthetic model's targets of the other dialect, in
	// their order, each on a provider that speaks that dialect. resolve
	// fills it only when Targets is empty and the model has translation
	// turned on (db.AIModel.Translate): then it is all the model has. With
	// targets of the request's own dialect the other one is not looked up
	// (see otherTargets). Always empty for a direct address. A resolution
	// has at least one target in Targets or in Other.
	Other []Target
}

// SyntheticModels looks up a synthetic model by name.
type SyntheticModels interface {
	ModelByName(ctx context.Context, name string) (db.AIModel, error) // store.ErrModelNotFound
}

var errModelNotFound = errors.New("aigateway: model not found")

// Bounds on a requested name, in bytes. The name comes from the client, is
// used in lookups and is echoed in errors; a longer one cannot exist.
const (
	maxSyntheticNameLen = 63  // the synthetic-name rule: ^[a-z0-9][a-z0-9._-]{1,62}$
	maxDirectAddressLen = 256 // "<provider-slug>/<native model id>"
)

// formatMismatchError reports a model that exists but is not served in the
// request's dialect. ServedBy names the dialect that does serve it.
type formatMismatchError struct {
	Model, Dialect, ServedBy string
}

func (e *formatMismatchError) Error() string {
	return "aigateway: model " + e.Model + " is not served in the " + e.Dialect + " format"
}

// resolve turns a requested model name into the targets to try for dialect.
// A name without "/" is a synthetic model; synthetic names never contain "/",
// so they cannot shadow a "<provider>/<model>" address or be shadowed by one.
// In a direct address the provider slug is everything before the first "/";
// the rest is the native model id, further slashes included.
//
// A request is never sent across dialects as it is: every target in Targets
// speaks dialect, checked against the provider row at request time. The
// targets in Other speak the other one and can only be reached through a
// translating pair (see candidatesForRequest).
//
// Errors: errModelNotFound and *formatMismatchError describe the request; any
// other error is a failed lookup.
func (g *Gateway) resolve(ctx context.Context, name, dialect string) (Resolution, error) {
	if name == "" || len(name) > maxDirectAddressLen {
		return Resolution{}, errModelNotFound
	}
	if !strings.Contains(name, "/") {
		if len(name) > maxSyntheticNameLen {
			return Resolution{}, errModelNotFound
		}
		return g.resolveSynthetic(ctx, name, dialect)
	}
	slug, native, _ := strings.Cut(name, "/")
	if slug == "" || native == "" {
		return Resolution{}, errModelNotFound
	}
	p, err := g.Providers.ProviderBySlug(ctx, slug)
	if errors.Is(err, db.ErrNotFound) {
		return Resolution{}, errModelNotFound
	}
	if err != nil {
		return Resolution{}, err
	}
	if p.APIFormat != dialect {
		return Resolution{}, &formatMismatchError{Model: name, Dialect: dialect, ServedBy: p.APIFormat}
	}
	return Resolution{Requested: name, Dialect: dialect, Targets: []Target{{Provider: p, Model: native}}}, nil
}

func (g *Gateway) resolveSynthetic(ctx context.Context, name, dialect string) (Resolution, error) {
	if g.Synthetic == nil {
		return Resolution{}, errModelNotFound
	}
	m, err := g.Synthetic.ModelByName(ctx, name)
	if errors.Is(err, store.ErrModelNotFound) {
		return Resolution{}, errModelNotFound
	}
	if err != nil {
		return Resolution{}, err
	}
	if !m.Enabled {
		return Resolution{}, errModelNotFound
	}
	res := Resolution{Requested: name, Dialect: dialect, Synthetic: true, Model: m}
	for _, t := range m.Targets {
		if t.Dialect != dialect {
			continue // never looked up: this request cannot use it
		}
		p, ok, err := g.targetProvider(ctx, t)
		if err != nil {
			return Resolution{}, err
		}
		if ok {
			res.Targets = append(res.Targets, Target{Provider: p, Model: t.TargetModel})
		}
	}
	if len(res.Targets) > 0 {
		// The other dialect is not looked at: a request that has a target
		// of its own neither needs it nor may fail because of it. The one
		// request that does need it (an endpoint none of these targets
		// offers) asks for it then, see otherTargets.
		return res, nil
	}
	// Nothing in this dialect. Only now look at the other one, to tell a
	// model served elsewhere from one that is not served at all. A model
	// that opted into translation is resolved with those targets; any other
	// is a mismatch.
	for _, t := range m.Targets {
		if t.Dialect == dialect {
			continue
		}
		p, ok, err := g.targetProvider(ctx, t)
		if err != nil {
			return Resolution{}, err
		}
		if !ok {
			continue
		}
		if !m.Translate {
			return Resolution{}, &formatMismatchError{Model: name, Dialect: dialect, ServedBy: t.Dialect}
		}
		res.Other = append(res.Other, Target{Provider: p, Model: t.TargetModel})
	}
	if len(res.Other) > 0 {
		return res, nil
	}
	return Resolution{}, errModelNotFound
}

// otherTargets returns the targets of the other dialect for a resolution
// that has targets of its own, and so came without them: a synthetic model's,
// in their order, each on a provider that speaks its dialect. It is asked
// only when none of the request's own targets offers the endpoint and the
// model has translation turned on. A provider that cannot be looked up is
// left out: the request still has the candidates of its own dialect.
func (g *Gateway) otherTargets(ctx context.Context, res Resolution) []Target {
	var other []Target
	for _, t := range res.Model.Targets {
		if t.Dialect == res.Dialect {
			continue
		}
		if p, ok, err := g.targetProvider(ctx, t); err == nil && ok {
			other = append(other, Target{Provider: p, Model: t.TargetModel})
		}
	}
	return other
}

// targetProvider returns the provider of a target row. ok is false when the
// target cannot be used: its provider is gone (the store refuses to delete a
// provider in use; a stale row is tolerated anyway), or the provider does not
// speak the row's dialect. The store's format lock is check-then-write, so a
// row can disagree with its provider; the provider row decides and a request
// is never sent across formats.
func (g *Gateway) targetProvider(ctx context.Context, t db.AIModelTarget) (db.AIProvider, bool, error) {
	p, err := g.Providers.ProviderBySlug(ctx, t.ProviderSlug)
	if errors.Is(err, db.ErrNotFound) {
		return db.AIProvider{}, false, nil
	}
	if err != nil {
		return db.AIProvider{}, false, err
	}
	return p, p.APIFormat == t.Dialect, nil
}
