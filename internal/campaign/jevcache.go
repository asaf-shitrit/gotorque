package campaign

import (
	"context"

	"example.com/gotorque/internal/jev"
)

// Role names under which JevCacheSnapshot counters are kept, matching the
// agents.Role* strings used for token usage.
const (
	jevCacheRoleAnalyst  = "analyst"
	jevCacheRoleExplorer = "explorer"
)

// JevCacheSnapshot is persisted per-role Jev cache hit/miss counts, mirroring
// RoleUsageSnapshot's shape for token usage.
type JevCacheSnapshot struct {
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`
}

// cachingEvaluator memoizes Jev answers for the life of a campaign, keyed by
// jev.Digest(req): the exact state, question set, and model a request carries.
// TypeSafe's docs and this repository's own replay (jev-batch-replay.md) agree
// that an answer does not depend on the other questions asked alongside it,
// so a request that repeats an earlier one's state and question set, whatever
// role asks it, can safely reuse that earlier answer.
//
// The cache lives in the campaign's bbolt store (Store.JevCacheGet/Put), not
// in engine memory, so --resume reuses it: CLAUDE.md's rule is that anything
// that must survive resume goes in bbolt, because in-graph CampaignState is
// rebuilt on every entry. A failed or partial response is never cached: it
// goes straight back to the caller so the next attempt gets a real retry
// rather than a frozen error.
type cachingEvaluator struct {
	inner jev.Evaluator
	store *Store
	// role names the hit/miss counters this evaluator's calls update
	// (e.g. "analyst", "explorer"), read back by the report.
	role   string
	engine *Engine
}

// cacheEvaluator wraps inner in a cachingEvaluator when both a store and an
// engine to record hits and misses against are available, and passes inner
// through unchanged otherwise (nil stays nil, and a role with no evaluator to
// cache needs no wrapping).
func cacheEvaluator(engine *Engine, role string, inner jev.Evaluator) jev.Evaluator {
	if inner == nil || engine == nil || engine.store == nil {
		return inner
	}
	return &cachingEvaluator{inner: inner, store: engine.store, role: role, engine: engine}
}

func (c *cachingEvaluator) Evaluate(ctx context.Context, req jev.Request) (jev.Response, error) {
	digest, err := jev.Digest(req)
	if err != nil {
		// A request that cannot be keyed cannot be cached either way; ask Jev
		// directly rather than failing the campaign over a cache that is only
		// ever an optimization.
		return c.inner.Evaluate(ctx, req)
	}
	if resp, ok, err := c.store.JevCacheGet(digest); err == nil && ok {
		c.engine.bumpJevCache(c.role, true)
		return resp, nil
	}
	resp, err := c.inner.Evaluate(ctx, req)
	if err != nil {
		return jev.Response{}, err
	}
	c.engine.bumpJevCache(c.role, false)
	// A cache write failure is not fatal: the request still answered the
	// caller, and the next identical request just costs another round trip.
	_ = c.store.JevCachePut(digest, resp)
	return resp, nil
}

// bumpJevCache updates this process's in-memory cache counters for role. It
// does not itself write to bbolt; the counters ride along with e.state on the
// next saveEvent, the same way token usage does (see recordTokenUsage).
func (e *Engine) bumpJevCache(role string, hit bool) {
	if e.state.JevCache == nil {
		e.state.JevCache = map[string]JevCacheSnapshot{}
	}
	s := e.state.JevCache[role]
	if hit {
		s.Hits++
	} else {
		s.Misses++
	}
	e.state.JevCache[role] = s
}
