package agents

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	adkrunner "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// fastModel builds the decorator with the production attempt count but no
// backoff, so a test can walk the whole retry ladder in microseconds.
func fastModel(inner model.LLM, role string, usage *UsageCollector) *fenceStrippingModel {
	return &fenceStrippingModel{inner: inner, role: role, usage: usage, attempts: defaultGenerateAttempts, baseBackoff: 0}
}

func TestUnwrapJSONFence(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain json unchanged", `{"a":1}`, `{"a":1}`},
		{"fenced json", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced bare", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced array", "```json\n[1,2]\n```", `[1,2]`},
		{"prose before fence is dropped when json follows", "Here you go:\n```json\n{\"a\":1}\n```", `{"a":1}`},
		{"non-json fenced code preserved", "```go\nfmt.Println()\n```", "```go\nfmt.Println()\n```"},
		{"unclosed fence still unwrapped when json body", "```json\n{\"a\":1}", `{"a":1}`},
		{"nested braces survive", "```json\n{\"patch\":\"diff with ``` inside\"}\n```", `{"patch":"diff with ` + "```" + ` inside"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnwrapJSONFence(tc.input); got != tc.want {
				t.Fatalf("UnwrapJSONFence() = %q, want %q", got, tc.want)
			}
		})
	}
}

type fakeLLM struct {
	resp *model.LLMResponse
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(f.resp, nil)
	}
}

// stallingLLM never answers: it returns only when its context is done, which
// is what a provider that stalls past the client timeout looks like from the
// retry ladder's point of view.
type stallingLLM struct {
	calls int
}

func (s *stallingLLM) Name() string { return "stalling" }

func (s *stallingLLM) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.calls++
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

// TestRunAttemptBoundsEachLadderAttempt pins the invariant that a stalled
// provider consumes the per-attempt bound rather than the caller's whole
// budget: without it one attempt stacked the client's own retries to 12m1s
// against a 4m client timeout, ate the orchestrator's 20m node deadline, and
// aborted a campaign before it evaluated a single candidate.
func TestRunAttemptBoundsEachLadderAttempt(t *testing.T) {
	tests := []struct {
		name           string
		attemptTimeout time.Duration
		callerTimeout  time.Duration
		wantAtLeast    time.Duration
	}{
		// The bound, not the caller, must end the attempt: the caller deadline
		// here is far away, so an unbounded attempt would blow the 1s ceiling.
		{"bounded attempt ends before the caller deadline", 20 * time.Millisecond, 10 * time.Second, 0},
		// A non-positive bound is the documented escape hatch: the caller's
		// deadline is then the only thing that ends a stalled attempt.
		{"unbounded attempt falls back to the caller deadline", 0, 200 * time.Millisecond, 200 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertLadderBounded(t, tc.attemptTimeout, tc.callerTimeout, tc.wantAtLeast)
		})
	}
}

func assertLadderBounded(t *testing.T, attemptTimeout, callerTimeout, wantAtLeast time.Duration) {
	t.Helper()
	inner := &stallingLLM{}
	m := &fenceStrippingModel{
		inner: inner, role: "explorer", attempts: 3,
		baseBackoff: 0, attemptTimeout: attemptTimeout,
	}
	callerCtx, cancel := context.WithTimeout(context.Background(), callerTimeout)
	defer cancel()

	started := time.Now()
	var gotErr error
	for _, err := range m.GenerateContent(callerCtx, &model.LLMRequest{}, false) {
		gotErr = err
	}
	elapsed := time.Since(started)

	if inner.calls != 3 {
		t.Errorf("attempts run = %d, want 3: the ladder must run to exhaustion", inner.calls)
	}
	if gotErr == nil {
		t.Fatal("expected ladder exhaustion, got nil error")
	}
	if elapsed < wantAtLeast {
		t.Errorf("ladder finished in %s, want at least %s", elapsed, wantAtLeast)
	}
	if elapsed >= time.Second {
		t.Errorf("ladder took %s: an attempt is not bounded", elapsed)
	}
}

func TestFenceStrippingModelRewritesCompleteResponses(t *testing.T) {
	inner := &fakeLLM{resp: &model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "```json\n{\"objective\":\"x\"}\n```"}}}}}
	decorated := NewFenceStrippingModel(inner, "test", nil, nil)
	if decorated == nil {
		t.Fatal("NewFenceStrippingModel returned nil")
	}
	for resp, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := resp.Content.Parts[0].Text
		if got != `{"objective":"x"}` {
			t.Fatalf("text = %q, want stripped JSON", got)
		}
	}
}

func TestFenceStrippingModelLeavesPartialsUntouched(t *testing.T) {
	inner := &fakeLLM{resp: &model.LLMResponse{Partial: true, Content: &genai.Content{Parts: []*genai.Part{{Text: "```json\n{\"obj"}}}}}
	decorated := NewFenceStrippingModel(inner, "test", nil, nil)
	for resp, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, true) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Content.Parts[0].Text != "```json\n{\"obj" {
			t.Fatalf("partial was rewritten: %q", resp.Content.Parts[0].Text)
		}
	}
}

func TestNewFenceStrippingModelNil(t *testing.T) {
	if NewFenceStrippingModel(nil, "test", nil, nil) != nil {
		t.Fatal("expected nil for nil inner model")
	}
}

func TestUnwrapJSONFenceProseExtraction(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"prose then json", "Here is my plan:\n{\"a\":1}", `{"a":1}`},
		{"json then trailing prose", "{\"a\":1}\nHope this helps!", `{"a":1}`},
		{"braces inside strings respected", `Prefix {"a":"}{"} suffix`, `{"a":"}{"}`},
		{"nested containers", `Answer: {"o":{"x":[1,2]},"y":"}"}`, `{"o":{"x":[1,2]},"y":"}"}`},
		{"array payload", "text before [1,{\"b\":2}] text after", `[1,{"b":2}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnwrapJSONFence(tc.input); got != tc.want {
				t.Fatalf("UnwrapJSONFence() = %q, want %q", got, tc.want)
			}
		})
	}
}

type flakyLLM struct {
	calls int
	fail  bool
	resp  *model.LLMResponse
}

func (f *flakyLLM) Name() string { return "flaky" }

func (f *flakyLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.calls++
		if f.fail && f.calls == 1 {
			yield(nil, context.DeadlineExceeded)
			return
		}
		yield(f.resp, nil)
	}
}

func TestFenceStrippingModelRetriesFirstFailure(t *testing.T) {
	inner := &flakyLLM{fail: true, resp: &model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: `{"ok":1}`}}}}}
	decorated := fastModel(inner, "test", nil)
	for resp, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("expected retry to succeed, got %v", err)
		}
		if resp.Content.Parts[0].Text != `{"ok":1}` {
			t.Fatalf("unexpected text %q", resp.Content.Parts[0].Text)
		}
	}
	if inner.calls != 2 {
		t.Fatalf("calls = %d, want 2 (one failure + one retry)", inner.calls)
	}
}

func TestFenceStrippingModelRecordsUsage(t *testing.T) {
	inner := &fakeLLM{resp: &model.LLMResponse{
		Content:       &genai.Content{Parts: []*genai.Part{{Text: `{"a":1}`}}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 120, CandidatesTokenCount: 34, TotalTokenCount: 154},
	}}
	collector := NewUsageCollector()
	decorated := NewFenceStrippingModel(inner, "optimizer", collector, nil)
	for _, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	snapshot := collector.Snapshot()
	usage, ok := snapshot["optimizer"]
	if !ok {
		t.Fatalf("no usage recorded for optimizer: %v", snapshot)
	}
	if usage.Requests != 1 || usage.PromptTokens != 120 || usage.CompletionTokens != 34 || usage.TotalTokens != 154 {
		t.Fatalf("usage = %+v, want 1 request with 120/34/154 tokens", usage)
	}
}

func TestUsageCollectorIgnoresNilCollectorAndNilMetadata(t *testing.T) {
	var nilCollector *UsageCollector
	nilCollector.Record("optimizer", &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 5})
	collector := NewUsageCollector()
	collector.Record("optimizer", nil)
	if len(collector.Snapshot()) != 0 {
		t.Fatal("nil metadata must not be recorded")
	}
}

func TestUsageCollectorSnapshotIsConcurrentSafe(t *testing.T) {
	t.Parallel()
	collector := NewUsageCollector()
	const writers, iterations = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				collector.Record("optimizer", &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 2, CandidatesTokenCount: 3, TotalTokenCount: 5})
				_ = collector.Snapshot()["optimizer"]
			}
		}()
	}
	wg.Wait()
	usage, ok := collector.Snapshot()["optimizer"]
	if !ok {
		t.Fatal("no usage recorded")
	}
	if usage.Requests != writers*iterations {
		t.Fatalf("requests = %d, want %d", usage.Requests, writers*iterations)
	}
	if usage.PromptTokens != 2*writers*iterations || usage.CompletionTokens != 3*writers*iterations || usage.TotalTokens != 5*writers*iterations {
		t.Fatalf("token totals wrong: %+v", usage)
	}
}

type sequenceLLM struct {
	responses []*model.LLMResponse
	calls     int
}

func (s *sequenceLLM) Name() string { return "sequence" }

func (s *sequenceLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if s.calls < len(s.responses) {
			r := s.responses[s.calls]
			s.calls++
			yield(r, nil)
			return
		}
		s.calls++
	}
}

// reasoningLLM mimics a reasoning model that burns its whole output budget
// thinking: the first thoughtOnly calls return a turn whose every part is a
// Thought part, and the call after that returns the JSON answer.
type reasoningLLM struct {
	thoughtOnly int
	answer      string
	calls       int
}

func (r *reasoningLLM) Name() string { return "reasoning" }

func (r *reasoningLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		r.calls++
		parts := []*genai.Part{{Text: r.answer}}
		if r.calls <= r.thoughtOnly {
			parts = []*genai.Part{{Text: "weighing two candidate patch sites", Thought: true}}
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: "model", Parts: parts}}, nil)
	}
}

// runSingleNodeWorkflow drives m through the real ADK stack the campaign uses:
// an llmagent in single-turn mode wrapped in a workflow agent node. That stack
// is where the one-output-per-execution rule lives, so it is the only way to
// prove the decorator cannot break it.
func runSingleNodeWorkflow(t *testing.T, m model.LLM) error {
	t.Helper()
	a, err := llmagent.New(llmagent.Config{
		Name:        "optimizer",
		Description: "proposes a patch",
		Model:       m,
		Instruction: "Return only JSON.",
		Mode:        llmagent.ModeSingleTurn,
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	node, err := workflow.NewAgentNode(a, workflow.NodeConfig{})
	if err != nil {
		t.Fatalf("NewAgentNode: %v", err)
	}
	wf, err := workflow.New("repro", workflow.NewEdgeBuilder().Add(workflow.Start, node).Build())
	if err != nil {
		t.Fatalf("workflow.New: %v", err)
	}
	root, err := adkagent.New(adkagent.Config{
		Name:        "root",
		Description: "runs the single-node graph",
		SubAgents:   []adkagent.Agent{a},
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return wf.Run(ctx)
		},
	})
	if err != nil {
		t.Fatalf("adkagent.New: %v", err)
	}
	adk, err := adkrunner.NewInMemory("test", root)
	if err != nil {
		t.Fatalf("NewInMemory: %v", err)
	}
	msg := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "optimize"}}}
	var runErr error
	for _, err := range adk.Run(context.Background(), "test", "session", msg, adkagent.RunConfig{}) {
		if err != nil && runErr == nil {
			runErr = err
		}
	}
	return runErr
}

// A reasoning-only turn is the one response the decorator must never let out.
// ADK's two consumers of such a turn disagree: the workflow agent node stamps
// its (empty) text as the node's output, while the LLM flow classifies it as
// "thinking, not answering" and calls the model again — so the next response
// becomes a second output event and the node dies on ErrMultipleOutputs,
// taking the campaign with it. Exhausting the retry ladder must therefore end
// in an error, not in handing that turn to the flow.
func TestFenceStrippingModelNeverYieldsReasoningOnlyTurn(t *testing.T) {
	inner := &reasoningLLM{thoughtOnly: defaultGenerateAttempts, answer: `{"hypothesis":"x"}`}
	err := runSingleNodeWorkflow(t, fastModel(inner, "optimizer", NewUsageCollector()))
	if errors.Is(err, workflow.ErrMultipleOutputs) {
		t.Fatalf("decorator produced two output-bearing events for one node execution: %v", err)
	}
	if err == nil {
		t.Fatal("expected the exhausted retry ladder to surface an error")
	}
	if inner.calls != defaultGenerateAttempts {
		t.Fatalf("calls = %d, want %d (one full ladder, no second model call)", inner.calls, defaultGenerateAttempts)
	}
}

// A reasoning model that thinks its way through a few attempts and then
// answers must still produce exactly one output, via the retry ladder rather
// than via a second flow turn.
func TestFenceStrippingModelRetriesPastReasoningOnlyTurns(t *testing.T) {
	inner := &reasoningLLM{thoughtOnly: 2, answer: `{"hypothesis":"x"}`}
	if err := runSingleNodeWorkflow(t, fastModel(inner, "optimizer", NewUsageCollector())); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner.calls != 3 {
		t.Fatalf("calls = %d, want 3 (two reasoning-only turns then the answer)", inner.calls)
	}
}

// chattyLLM yields everything in one pass: the errors first, then the
// responses. It stands in for any inner model that does not stop at a single
// complete response.
type chattyLLM struct {
	errs      []error
	responses []*model.LLMResponse
	calls     int
}

func (c *chattyLLM) Name() string { return "chatty" }

func (c *chattyLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		c.calls++
		for _, err := range c.errs {
			if !yield(nil, err) {
				return
			}
		}
		for _, resp := range c.responses {
			if !yield(resp, nil) {
				return
			}
		}
	}
}

// One node execution may carry at most one output-bearing event, so the
// decorator has to cap its own output rather than trust the inner iterator to
// stop at one. Both shapes below used to leak a second value: two complete
// responses in a row, and an error surfaced with the range loop continuing
// afterwards.
func TestFenceStrippingModelYieldsAtMostOneValuePerCall(t *testing.T) {
	answer := func() *model.LLMResponse {
		return &model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: `{"a":1}`}}}}
	}
	tests := []struct {
		name  string
		inner *chattyLLM
	}{
		{"two complete responses", &chattyLLM{responses: []*model.LLMResponse{answer(), answer()}}},
		{"error then response", &chattyLLM{errs: []error{context.DeadlineExceeded}, responses: []*model.LLMResponse{answer()}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yields := 0
			for range fastModel(tc.inner, "optimizer", NewUsageCollector()).GenerateContent(context.Background(), &model.LLMRequest{}, false) {
				yields++
			}
			if yields != 1 {
				t.Fatalf("yields = %d, want exactly 1", yields)
			}
		})
	}
}

// A ladder that only ever sees transport failures must end in one error that
// still names the underlying cause, so 429s and deadlines stay diagnosable.
func TestFenceStrippingModelSurfacesLastErrorOnceWhenLadderIsSpent(t *testing.T) {
	inner := &chattyLLM{errs: []error{context.DeadlineExceeded}}
	yields := 0
	var got error
	for resp, err := range fastModel(inner, "optimizer", nil).GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		yields++
		got, _ = err, resp
	}
	if yields != 1 {
		t.Fatalf("yields = %d, want exactly 1", yields)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want it to wrap context.DeadlineExceeded", got)
	}
	if inner.calls != defaultGenerateAttempts {
		t.Fatalf("calls = %d, want %d (every transport failure retried)", inner.calls, defaultGenerateAttempts)
	}
}

// Usage is per response received, not per response delivered: a discarded
// attempt still burned tokens the campaign has to account for.
func TestFenceStrippingModelRecordsUsageForDiscardedAttempts(t *testing.T) {
	inner := &reasoningLLM{thoughtOnly: 2, answer: `{"a":1}`}
	collector := NewUsageCollector()
	for _, err := range fastModel(inner, "optimizer", collector).GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := collector.Snapshot()["optimizer"].Requests; got != 0 {
		t.Fatalf("requests = %d, want 0 (the stub reports no usage metadata)", got)
	}
	metered := &sequenceLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "not json"}}}, UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 7}},
		{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: `{"a":1}`}}}, UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 11}},
	}}
	collector = NewUsageCollector()
	for _, err := range fastModel(metered, "optimizer", collector).GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	usage := collector.Snapshot()["optimizer"]
	if usage.Requests != 2 || usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v, want 2 requests and 18 tokens", usage)
	}
}

func TestFenceStrippingModelRetriesUnparseableJSON(t *testing.T) {
	badContent := &genai.Content{Parts: []*genai.Part{{Text: "not json at all"}}}
	goodContent := &genai.Content{Parts: []*genai.Part{{Text: `{"a":1}`}}}
	seq := &sequenceLLM{responses: []*model.LLMResponse{
		{Content: badContent},
		{Content: goodContent},
	}}
	decorated := fastModel(seq, "explorer", NewUsageCollector())
	count := 0
	for resp, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		count++
		if got := resp.Content.Parts[0].Text; got != `{"a":1}` {
			t.Fatalf("yielded text = %q", got)
		}
	}
	if count != 1 || seq.calls != 2 {
		t.Fatalf("yielded=%d calls=%d, want 1/2", count, seq.calls)
	}
}

func TestFenceStrippingModelReportsEachAttemptToObserver(t *testing.T) {
	// First response is unusable so a retry happens; the second succeeds.
	inner := &sequenceLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Parts: []*genai.Part{{Text: "not json at all"}}}},
		{Content: &genai.Content{Parts: []*genai.Part{{Text: `{"objective":"x"}`}}}},
	}}
	var calls []CallInfo
	decorated := fastModel(inner, "analyst", nil)
	decorated.observer = func(info CallInfo) { calls = append(calls, info) }
	for _, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if len(calls) != 2 {
		t.Fatalf("observed %d attempts, want 2: %+v", len(calls), calls)
	}
	if calls[0].Attempt != 1 || !calls[0].Retrying {
		t.Errorf("first attempt = %+v, want attempt 1 marked retrying", calls[0])
	}
	if calls[1].Attempt != 2 || calls[1].Retrying {
		t.Errorf("second attempt = %+v, want attempt 2 not retrying", calls[1])
	}
	for _, call := range calls {
		if call.Role != "analyst" {
			t.Errorf("role = %q, want analyst", call.Role)
		}
	}
}
