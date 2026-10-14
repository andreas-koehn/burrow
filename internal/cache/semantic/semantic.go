// Package semantic layers a vector-similarity tier on top of the v0.4.0
// exact cache. The Cache interface is defined unconditionally; the
// chromem-go-backed impl is build-tag-gated. The default build links the
// NoopCache so calls compile.
package semantic

import (
	"context"
)

// Settings holds the operator-supplied vector-cache tuning knobs.
type Settings struct {
	Enabled         bool
	MinSimilarity   float64 // 0.0..1.0 inclusive
	EmbeddingMode   string  // "local"|"none"
	EmbeddingURL    string
	EmbeddingModel  string
	FallbackPolicy  string // "treat_as_miss"|"return_cached_marked"
	PromoteOnMiss   bool
	MaxIndexEntries int
}

// Candidate is the result of a successful semantic Lookup — the
// exact-cache row whose embedding was most similar to the query prompt.
type Candidate struct {
	ExactKeyHash      string
	PromptFingerprint string
	Similarity        float64
}

// Stats is the health/occupancy snapshot returned by Cache.Stats.
type Stats struct {
	Entries            int
	OnDiskBytes        int64
	HitRate24h         float64
	SimilarReturned24h int
	Promotions24h      int
}

// Cache is the semantic cache interface. The concrete implementation is
// build-tag-gated; in the default build NoopCache is returned by New.
type Cache interface {
	// Lookup searches for a semantically-similar cached prompt.
	// Returns (Candidate, true, nil) on a hit; (zero, false, nil) on miss;
	// non-nil error only on real I/O failure (embedding HTTP error degrades
	// silently to miss — see spec A.1.6).
	Lookup(ctx context.Context, serviceID string, prompt []byte, s Settings) (Candidate, bool, error)

	// Promote indexes a prompt/response pair so future similar prompts can
	// hit it via Lookup. exactKeyHash is the exact-cache key the row joins
	// back to. Called from the exact.Cache OnMiss hook (Task 16).
	Promote(ctx context.Context, serviceID, exactKeyHash string, prompt []byte, s Settings) error

	// ClearService removes all semantic index entries for the given service.
	ClearService(ctx context.Context, serviceID string) error

	// Stats returns the occupancy/hit-rate snapshot for a service.
	Stats(ctx context.Context, serviceID string) (Stats, error)
}

// Available reports whether c is a semantic tier that does something: false
// for nil and for the NoopCache of a build without the semantic_cache tag.
func Available(c Cache) bool {
	if c == nil {
		return false
	}
	_, noop := c.(NoopCache)
	return !noop
}

// NoopCache is the zero-allocation implementation used when the semantic
// tier is disabled or not compiled in. All methods are safe to call; none
// ever return an error.
type NoopCache struct{}

func (NoopCache) Lookup(_ context.Context, _ string, _ []byte, _ Settings) (Candidate, bool, error) {
	return Candidate{}, false, nil
}

func (NoopCache) Promote(_ context.Context, _, _ string, _ []byte, _ Settings) error {
	return nil
}

func (NoopCache) ClearService(_ context.Context, _ string) error {
	return nil
}

func (NoopCache) Stats(_ context.Context, _ string) (Stats, error) {
	return Stats{}, nil
}
