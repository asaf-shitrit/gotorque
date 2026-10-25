package campaign

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/jev"
)

func openTestAnswerRecord(t *testing.T) *AnswerRecord {
	t.Helper()
	record, err := OpenAnswerRecord(filepath.Join(t.TempDir(), "jev-answers.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = record.Close() })
	return record
}

func TestAnswerRecordRoundTripsAnAnswer(t *testing.T) {
	record := openTestAnswerRecord(t)
	require.Equal(t, record.Path(), filepath.Join(filepath.Dir(record.Path()), "jev-answers.db"))
	resp := jev.Response{Model: jev.Model, Answers: map[string]jev.Answer{"alloc": {Type: "noul", Probability: 0.51}}}

	_, ok, err := record.Get("digest-1")
	require.NoError(t, err)
	require.False(t, ok, "an unrecorded digest is a miss, not an error")

	require.NoError(t, record.Put("digest-1", resp))
	got, ok, err := record.Get("digest-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, resp, got)
}

// TestCachingEvaluatorAnswersFromAnEarlierCampaignsRecord is the whole point of
// the record (ADR 0042): a second campaign on the same source must not spend
// another request whose answer the first one already knows, and it must be
// able to say in its report where the answer came from.
func TestCachingEvaluatorAnswersFromAnEarlierCampaignsRecord(t *testing.T) {
	record := openTestAnswerRecord(t)
	inner := &countingEvaluator{}
	req := jev.Request{State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()}

	first := newCacheTestEngine(t)
	first.UseAnswerRecord(record)
	_, err := cacheEvaluator(first, "analyst", inner).Evaluate(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, 1, inner.calls)

	second := newCacheTestEngine(t)
	second.UseAnswerRecord(record)
	cached := cacheEvaluator(second, "analyst", inner)
	_, err = cached.Evaluate(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, 1, inner.calls, "a campaign on the same source must reuse the recorded answer")
	require.Equal(t, JevCacheSnapshot{Shared: 1}, second.state.JevCache["analyst"])

	// The shared answer fills the campaign's own cache, so the next cycle is a
	// local hit rather than another read of the record.
	_, err = cached.Evaluate(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, 1, inner.calls)
	require.Equal(t, JevCacheSnapshot{Hits: 1, Shared: 1}, second.state.JevCache["analyst"])
}

// TestCachingEvaluatorRecordsOnlyAnswersJevGave: an error, or a response with
// no answers, must never be recorded. A recorded failure would deny every
// later campaign the retry that might have succeeded.
func TestCachingEvaluatorRecordsOnlyAnswersJevGave(t *testing.T) {
	record := openTestAnswerRecord(t)
	engine := newCacheTestEngine(t)
	engine.UseAnswerRecord(record)
	req := jev.Request{State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()}

	_, err := cacheEvaluator(engine, "analyst", &countingEvaluator{err: errors.New("HTTP 429")}).Evaluate(context.Background(), req)
	require.Error(t, err)
	digest, err := jev.Digest(jev.Request{Model: jev.Model, State: req.State, Questions: req.Questions})
	require.NoError(t, err)
	_, ok, err := record.Get(digest)
	require.NoError(t, err)
	require.False(t, ok, "a failed request must not be recorded")

	_, err = cacheEvaluator(engine, "analyst", &countingEvaluator{}).Evaluate(context.Background(), req)
	require.NoError(t, err)
	_, ok, err = record.Get(digest)
	require.NoError(t, err)
	require.True(t, ok, "an answered request is recorded under the build's own key")
}

// TestTheRecordKeyNamesThePinnedBuild: the record outlives a build, so its key
// must carry jev.Model. Without it, an upgrade that moves the pinned build
// would replay answers measured against the build it replaced (ADR 0029).
func TestTheRecordKeyNamesThePinnedBuild(t *testing.T) {
	record := openTestAnswerRecord(t)
	for _, model := range []string{"", jev.Model} {
		bare, err := jev.Digest(jev.Request{Model: model, State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()})
		require.NoError(t, err)
		require.Len(t, bare, 64)
	}
	engine := newCacheTestEngine(t)
	engine.UseAnswerRecord(record)
	_, err := cacheEvaluator(engine, "analyst", &countingEvaluator{}).Evaluate(context.Background(), jev.Request{State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()})
	require.NoError(t, err)
	// The request carried no model; the evaluator must have keyed it by the
	// pinned build anyway, which is what a later campaign looks up.
	digest, err := jev.Digest(jev.Request{Model: jev.Model, State: jev.SiteState("a.go", "func f() {}"), Questions: jev.Questions()})
	require.NoError(t, err)
	_, ok, err := record.Get(digest)
	require.NoError(t, err)
	require.True(t, ok, "the recorded key must name the pinned build")
}

// driftingEvaluator answers like scriptedEvaluator, but moves one cause's
// probability on every call, the way Jev did on the record: on the recorded
// development campaigns the same question about the same function was
// answered as far as 0.13 apart, and 21 of the 35 flag flips in that record
// crossed the 0.5 probability floor.
type driftingEvaluator struct {
	scriptedEvaluator
	values []float64
	moved  int
}

func (d *driftingEvaluator) Evaluate(ctx context.Context, req jev.Request) (jev.Response, error) {
	resp, err := d.scriptedEvaluator.Evaluate(ctx, req)
	if err != nil {
		return resp, err
	}
	p := d.values[min(d.moved, len(d.values)-1)]
	resp.Answers[string(jev.CauseRedundant)] = jev.Answer{Type: "noul", Probability: p}
	d.moved++
	return resp, nil
}

// TestAnalystFlagsStayStableAcrossCampaignsOnTheSameSource is the regression
// test for issue #73: the same function, the same questions, two campaigns,
// and an answer that drifts across the 0.5 floor. Without the record the
// second campaign stops flagging the cause and never tries the target; with
// it, both campaigns see the answer the first one recorded.
func TestAnalystFlagsStayStableAcrossCampaignsOnTheSameSource(t *testing.T) {
	repo := writeCauseFixture(t)
	record := openTestAnswerRecord(t)
	// 0.51 clears the floor and the redundant z threshold; 0.49 is under the
	// floor, which is where the recorded flips actually happened.
	evaluator := &driftingEvaluator{values: []float64{0.51, 0.49}}
	req := causeRequest(repo, "fixture.go:19")

	analyze := func() agents.AnalystResult {
		engine := newCacheTestEngine(t)
		engine.UseAnswerRecord(record)
		analyst := causeAnalyst{evaluator: cacheEvaluator(engine, "analyst", evaluator), usage: agents.NewUsageCollector()}
		result, err := analyst.AnalyzeCauses(context.Background(), req)
		require.NoError(t, err)
		return result
	}

	first := analyze()
	require.Len(t, first.Targets, 1)
	require.Equal(t, "redundant", first.Targets[0].Cause)
	require.Equal(t, 1, evaluator.moved)

	second := analyze()
	require.Equal(t, 1, evaluator.moved, "the second campaign must reuse the recorded answer instead of asking again")
	require.Equal(t, first.Targets, second.Targets, "the same source must flag the same target in both campaigns")
}

// TestAFlagOutsideTheRecordStillDrifts pins what the record does not do: two
// campaigns that each ask a fresh question, which is what --jev-answers off
// does, still disagree. It keeps the regression above honest -- it fails if a
// later change makes the flags stable for some other reason.
func TestAFlagOutsideTheRecordStillDrifts(t *testing.T) {
	repo := writeCauseFixture(t)
	evaluator := &driftingEvaluator{values: []float64{0.51, 0.49}}
	req := causeRequest(repo, "fixture.go:19")
	analyze := func() agents.AnalystResult {
		analyst := causeAnalyst{evaluator: evaluator, usage: agents.NewUsageCollector()}
		result, err := analyst.AnalyzeCauses(context.Background(), req)
		require.NoError(t, err)
		return result
	}

	require.Len(t, analyze().Targets, 1, "0.51 clears both gates")
	require.Empty(t, analyze().Targets, "0.49 leaves the probability floor, which is the drift the record removes")
}

func TestReportNamesTheAnswerRecordAndItsReuse(t *testing.T) {
	out := RenderMarkdown(State{
		ID: "c", SchemaVersion: ReportSchemaVersion, Analyst: AnalystJev,
		JevAnswersPath: "/home/u/.cache/gotorque/jev-answers.db",
		JevCache:       map[string]JevCacheSnapshot{"analyst": {Hits: 1, Shared: 2, Misses: 3}},
	})
	require.Contains(t, out, "- Jev answers: reused across campaigns from the record at `/home/u/.cache/gotorque/jev-answers.db` (ADR 0042)")
	require.Contains(t, out, "| Role | Hits | Shared | Misses |")
	require.Contains(t, out, "| `analyst` | 1 | 2 | 3 |")
}

func TestReportOmitsTheAnswerRecordWhenThereIsNone(t *testing.T) {
	out := RenderMarkdown(State{ID: "c", SchemaVersion: ReportSchemaVersion, Analyst: AnalystJev})
	require.NotContains(t, out, "Jev answers:")
	require.NotContains(t, out, "Jev cache")
}
