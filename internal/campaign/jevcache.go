package campaign

import (
	"context"

	"github.com/asaf-shitrit/gotorque/internal/jev"
)

// Role names under which JevCacheSnapshot counters are kept, matching the
// agents.Role* strings used for token usage.
const (
	jevCacheRoleAnalyst  = "analyst"
	jevCacheRoleExplorer = "explorer"
)

// JevCacheSnapshot is persisted per-role Jev cache counts, mirroring
// RoleUsageSnapshot's shape for token usage. Hits are answers this campaign's
// own cache held, Shared are answers the cross-campaign record held (ADR
// 0042), and Misses are the requests that reached Jev.
type JevCacheSnapshot struct {
	Hits   int64 `json:"hits"`
	Shared int64 `json:"shared,omitempty"`
	Misses int64 `json:"misses"`
}

// jevAnswerSource names where an answer came from, which is what the three
// counters above record.
type jevAnswerSource int

const (
	answerFromJev jevAnswerSource = iota
	answerFromCampaignCache
	answerFromRecord
)

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
// rather than a frozen error. When the campaign carries an AnswerRecord
// (ADR 0042) the same digest is looked up there too, which is what stops a
// near-threshold flag from flipping between campaigns on the same source.
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
	// The pinned build belongs in the key. The client fills Model itself
	// before sending, so this changes nothing on the wire, but without it a
	// gotorque upgrade that moves jev.Model would replay answers measured
	// against the build it replaced (ADR 0029), here and in the answer record
	// (ADR 0042).
	if req.Model == "" {
		req.Model = jev.Model
	}
	digest, err := jev.Digest(req)
	if err != nil {
		// A request that cannot be keyed cannot be cached either way; ask Jev
		// directly rather than failing the campaign over a cache that is only
		// ever an optimization.
		return c.inner.Evaluate(ctx, req)
	}
	if resp, ok, err := c.store.JevCacheGet(digest); err == nil && ok {
		c.engine.bumpJevCache(c.role, answerFromCampaignCache)
		return withNoUsage(resp), nil
	}
	// This campaign has not asked this exact question of this exact source, but
	// an earlier one may have (ADR 0042). A recorded answer is still code's
	// input and never a verdict: it feeds the same ranking and the same gates.
	if resp, ok := c.engine.recordedAnswer(digest); ok {
		c.engine.bumpJevCache(c.role, answerFromRecord)
		// Fill this campaign's cache too, so a later cycle of the same campaign
		// reads it as its own hit instead of a record lookup.
		_ = c.store.JevCachePut(digest, resp)
		return withNoUsage(resp), nil
	}
	resp, err := c.inner.Evaluate(ctx, req)
	if err != nil {
		return jev.Response{}, err
	}
	c.engine.bumpJevCache(c.role, answerFromJev)
	// A cache write failure is not fatal: the request still answered the
	// caller, and the next identical request just costs another round trip.
	_ = c.store.JevCachePut(digest, resp)
	c.engine.recordAnswer(digest, resp)
	return resp, nil
}

// withNoUsage strips the usage an answered request carried. Replaying it would
// charge the campaign for a call it did not make: live1-fzf's report showed 48
// analyst requests for 12 paid ones.
func withNoUsage(resp jev.Response) jev.Response {
	resp.Usage = jev.Usage{}
	return resp
}

// recordedAnswer returns the answer an earlier campaign already recorded for
// digest, or ok == false when this campaign runs without a record or the
// record could not be read. A record is an optimization and a reproducibility
// aid, never a dependency: every failure here falls back to asking Jev.
func (e *Engine) recordedAnswer(digest string) (jev.Response, bool) {
	if e.answers == nil {
		return jev.Response{}, false
	}
	resp, ok, err := e.answers.Get(digest)
	if err != nil {
		return jev.Response{}, false
	}
	return resp, ok
}

// recordAnswer stores an answer Jev just gave, for the next campaign on the
// same source. Only an answered request reaches here; a failed or partial
// response is never recorded.
func (e *Engine) recordAnswer(digest string, resp jev.Response) {
	if e.answers == nil {
		return
	}
	_ = e.answers.Put(digest, resp)
}

// UseAnswerRecord makes this campaign consult and extend the cross-campaign
// record of Jev answers (ADR 0042). It must be called before the campaign
// runs; the evaluators read the record lazily, so it may follow the ADK
// attach step. A nil record leaves the campaign on its own cache, and clears
// the path a resumed campaign inherited, so the report cannot claim answers
// this run did not replay.
func (e *Engine) UseAnswerRecord(record *AnswerRecord) {
	if record == nil {
		e.answers = nil
		e.state.JevAnswersPath = ""
		return
	}
	e.answers = record
	e.state.JevAnswersPath = record.Path()
}

// bumpJevCache updates this process's in-memory cache counters for role. It
// does not itself write to bbolt; the counters ride along with e.state on the
// next saveEvent, the same way token usage does (see recordTokenUsage).
func (e *Engine) bumpJevCache(role string, from jevAnswerSource) {
	if e.state.JevCache == nil {
		e.state.JevCache = map[string]JevCacheSnapshot{}
	}
	s := e.state.JevCache[role]
	switch from {
	case answerFromCampaignCache:
		s.Hits++
	case answerFromRecord:
		s.Shared++
	case answerFromJev:
		s.Misses++
	}
	e.state.JevCache[role] = s
}
