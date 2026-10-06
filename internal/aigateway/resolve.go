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
	Targets   []Target   // at least one, all speaking Dialect, in try order
}

// SyntheticModels looks up a synthetic model by name.
type SyntheticModels interface {
	ModelByName(ctx context.Context, name string) (db.AIModel, error) // store.ErrModelNotFound
}

var errModelNotFound = errors.New("aigateway: model not found")

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
// A request never crosses dialects: every returned target's provider speaks
// dialect, checked against the provider row at request time.
//
// Errors: errModelNotFound and *formatMismatchError describe the request; any
// other error is a failed lookup.
func (g *Gateway) resolve(ctx context.Context, name, dialect string) (Resolution, error) {
	if name == "" {
		return Resolution{}, errModelNotFound
	}
	if !strings.Contains(name, "/") {
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
		return res, nil
	}
	// Nothing in this dialect. Only now look at the other one, to tell a
	// model served elsewhere from one that is not served at all.
	for _, t := range m.Targets {
		if t.Dialect == dialect {
			continue
		}
		_, ok, err := g.targetProvider(ctx, t)
		if err != nil {
			return Resolution{}, err
		}
		if ok {
			return Resolution{}, &formatMismatchError{Model: name, Dialect: dialect, ServedBy: t.Dialect}
		}
	}
	return Resolution{}, errModelNotFound
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
