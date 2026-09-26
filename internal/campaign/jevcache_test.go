package campaign

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"example.com/gotorque/internal/jev"
	"github.com/stretchr/testify/require"
)

// countingEvaluator answers every question with a fixed probability and
// counts how many times it was actually asked, which is what a cache hit must
// avoid incrementing.
type countingEvaluator struct {
	calls int
	err   error
}

func (c *countingEvaluator) Evaluate(_ context.Context, req jev.Request) (jev.Response, error) {
	c.calls++
	if c.err != nil {
		return jev.Response{}, c.err
	}
	answers := make(map[string]jev.Answer, len(req.Questions))
	for id := range req.Questions {
		answers[id] = jev.Answer{Type: "boolean", Probability: 0.42}
	}
	return jev.Response{Model: jev.Model, Answers: answers}, nil
}

func newCacheTestEngine(t *testing.T) *Engine {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "campaign.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return &Engine{store: store}
}

func TestCachingEvaluatorMakesNoSecondCallForTheSameRequest(t *testing.T) {
	engine := newCacheTestEngine(t)
	inner := &countingEvaluator{}
	cached := cacheEvaluator(engine, "analyst", inner)

	req := jev.Request{State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()}
	_, err := cached.Evaluate(context.Background(), req)
	require.NoError(t, err)
	_, err = cached.Evaluate(context.Background(), req)
	require.NoError(t, err)

	require.Equal(t, 1, inner.calls, "a second identical request must not reach the underlying evaluator")
	require.Equal(t, JevCacheSnapshot{Hits: 1, Misses: 1}, engine.state.JevCache["analyst"])
}

func TestCachingEvaluatorMissesOnAChangedStateOrQuestionSet(t *testing.T) {
	engine := newCacheTestEngine(t)
	inner := &countingEvaluator{}
	cached := cacheEvaluator(engine, "analyst", inner)

	base := jev.Request{State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()}
	_, err := cached.Evaluate(context.Background(), base)
	require.NoError(t, err)

	changedState := jev.Request{State: jev.SiteState("a.go", "func f() { g() }"), Questions: jev.Questions()}
	_, err = cached.Evaluate(context.Background(), changedState)
	require.NoError(t, err)
	require.Equal(t, 2, inner.calls, "a changed state must miss the cache")

	changedQuestions := jev.Request{State: jev.SiteState("a.go", "func f() {}"), Questions: jev.KindQuestions()}
	_, err = cached.Evaluate(context.Background(), changedQuestions)
	require.NoError(t, err)
	require.Equal(t, 3, inner.calls, "a changed question set must miss the cache")

	require.Equal(t, JevCacheSnapshot{Hits: 0, Misses: 3}, engine.state.JevCache["analyst"])
}

func TestCachingEvaluatorNeverCachesAFailure(t *testing.T) {
	engine := newCacheTestEngine(t)
	inner := &countingEvaluator{err: errors.New("gateway returned HTTP 429 for Jev")}
	cached := cacheEvaluator(engine, "analyst", inner)

	req := jev.Request{State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()}
	_, err := cached.Evaluate(context.Background(), req)
	require.Error(t, err)
	_, err = cached.Evaluate(context.Background(), req)
	require.Error(t, err)

	require.Equal(t, 2, inner.calls, "a failed response must never be served from the cache")
	require.Empty(t, engine.state.JevCache["analyst"], "a failure counts as neither a hit nor a miss")
}

func TestJevCacheSurvivesAStoreCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign.db")
	store, err := OpenStore(path)
	require.NoError(t, err)

	req := jev.Request{State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()}
	digest, err := jev.Digest(req)
	require.NoError(t, err)
	want := jev.Response{Model: jev.Model, Answers: map[string]jev.Answer{"alloc": {Type: "boolean", Probability: 0.7}}}
	require.NoError(t, store.JevCachePut(digest, want))
	require.NoError(t, store.Close())

	reopened, err := OpenStore(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()

	got, ok, err := reopened.JevCacheGet(digest)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)
}

func TestJevCacheGetReportsAMissWithoutError(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "campaign.db"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	_, ok, err := store.JevCacheGet("does-not-exist")
	require.NoError(t, err)
	require.False(t, ok)
}

// TestCacheEvaluatorPassesThroughWithoutAStore makes sure a nil store (an
// engine not yet backed by bbolt) degrades to the uncached evaluator instead
// of panicking.
func TestCacheEvaluatorPassesThroughWithoutAStore(t *testing.T) {
	inner := &countingEvaluator{}
	wrapped := cacheEvaluator(&Engine{}, "analyst", inner)
	require.Same(t, jev.Evaluator(inner), wrapped)
}
