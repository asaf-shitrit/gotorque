package agents

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// scriptedStream yields the given responses with a delay before each one, so a
// test can drive both a healthy slow stream and a stalled one.
type scriptedStream struct {
	responses []*model.LLMResponse
	delay     time.Duration
	sawStream bool
}

func (s *scriptedStream) Name() string { return "scripted" }

func (s *scriptedStream) GenerateContent(ctx context.Context, _ *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	s.sawStream = stream
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, resp := range s.responses {
			select {
			case <-time.After(s.delay):
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			}
			if !yield(resp, nil) {
				return
			}
		}
	}
}

func textResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}}}
}

// collect drains a sequence and returns every response it yielded.
func collect(seq iter.Seq2[*model.LLMResponse, error]) ([]*model.LLMResponse, error) {
	var (
		out  []*model.LLMResponse
		last error
	)
	for resp, err := range seq {
		if err != nil {
			last = err
			continue
		}
		out = append(out, resp)
	}
	return out, last
}

// A slow call that keeps producing must survive: the old whole-request timeout
// cut calls like this one and reported them as stalls. The deltas spell the
// answer and the final item repeats it, which is the shape a live stream has.
func TestStreamedModelKeepsASlowButProducingCallAlive(t *testing.T) {
	final := &model.LLMResponse{
		Content:       &genai.Content{Role: "model", Parts: []*genai.Part{{Text: `{"objective":"x"}`}}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 42},
	}
	inner := &scriptedStream{delay: 30 * time.Millisecond, responses: []*model.LLMResponse{
		textResponse(`{"objective":`),
		textResponse(`"x"}`),
		final,
	}}
	decorated := streamedModel{inner: inner, idle: 200 * time.Millisecond}

	responses, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.NoError(t, err)
	require.True(t, inner.sawStream, "the decorator must ask the endpoint to stream")
	require.Len(t, responses, 1, "the caller asked for one response, not a stream of chunks")
	require.JSONEq(t, `{"objective":"x"}`, textOf(responses[0]))
	require.Equal(t, int32(42), responses[0].UsageMetadata.TotalTokenCount)
}

// A call that goes quiet is abandoned long before the attempt deadline, which
// is what lets the retry ladder try again inside the same node budget.
func TestStreamedModelFailsAStalledStreamAtTheIdleBound(t *testing.T) {
	inner := &scriptedStream{delay: 500 * time.Millisecond, responses: []*model.LLMResponse{textResponse(`{"objective":"x"}`)}}
	decorated := streamedModel{inner: inner, idle: 50 * time.Millisecond}

	started := time.Now()
	_, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.Error(t, err)
	require.ErrorContains(t, err, "stalled")
	require.Less(t, time.Since(started), 400*time.Millisecond, "the idle bound must fire before the scripted chunk arrives")
}

// Usage arrives on the final item, which for some providers carries nothing
// else, so the answer still has to come from the deltas.
func TestStreamedModelTakesUsageFromTheFinalChunk(t *testing.T) {
	usageOnly := &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 7}}
	inner := &scriptedStream{responses: []*model.LLMResponse{
		textResponse(`{"objective":`),
		textResponse(`"x"}`),
		usageOnly,
	}}
	decorated := streamedModel{inner: inner, idle: time.Second}

	responses, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.NoError(t, err)
	require.Len(t, responses, 1)
	require.JSONEq(t, `{"objective":"x"}`, textOf(responses[0]))
	require.Equal(t, int32(7), responses[0].UsageMetadata.TotalTokenCount)
}

// A consumer that stops early must not leave the producer goroutine blocked:
// the fence gives up on a response it cannot parse.
func TestStreamedModelStopsWhenTheConsumerStops(t *testing.T) {
	inner := &scriptedStream{responses: []*model.LLMResponse{textResponse("a"), textResponse("b"), textResponse("c")}}
	decorated := streamedModel{inner: inner, idle: time.Second}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, err := range decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
			_ = err
			return // abandon the sequence, as ADK does on a terminal error
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("returning from the consumer blocked")
	}
}

func TestStreamedModelReportsAnEmptyStream(t *testing.T) {
	decorated := streamedModel{inner: &scriptedStream{}, idle: time.Second}
	_, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.ErrorIs(t, err, errNoResponse)
}

func TestStreamedModelKeepsTheInnerError(t *testing.T) {
	decorated := streamedModel{inner: failingStream{}, idle: time.Second}
	_, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.ErrorContains(t, err, "endpoint exploded")
}

type failingStream struct{}

func (failingStream) Name() string { return "failing" }

func (failingStream) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New("endpoint exploded"))
	}
}

func textOf(resp *model.LLMResponse) string {
	if resp == nil || resp.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range resp.Content.Parts {
		if part != nil {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// A live OpenRouter stream ends by repeating the answer and carrying the
// model's reasoning beside it: keeping that final item verbatim produced
// "{\"ok\":true}{\"ok\":true}" and mixed deliberation into the payload the
// decoder parses.
func TestStreamedModelIgnoresRepeatedAndReasoningText(t *testing.T) {
	final := &model.LLMResponse{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{
			{Text: "We need answer. Need comply.", Thought: true},
			{Text: `{"ok":true}`},
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 149},
	}
	inner := &scriptedStream{responses: []*model.LLMResponse{
		textResponse(`{"ok":`),
		textResponse(`true}`),
		final,
	}}
	decorated := streamedModel{inner: inner, idle: time.Second}

	responses, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.NoError(t, err)
	require.Len(t, responses, 1)
	require.JSONEq(t, `{"ok":true}`, textOf(responses[0]))
	require.Equal(t, int32(149), responses[0].UsageMetadata.TotalTokenCount)
}

// A stream whose deltas are all reasoning still has to yield the answer, which
// arrives on the aggregated final item.
func TestStreamedModelFallsBackToTheFinalItemWhenDeltasAreReasoning(t *testing.T) {
	inner := &scriptedStream{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "thinking...", Thought: true}}}},
		textResponse(`{"objective":"x"}`),
	}}
	decorated := streamedModel{inner: inner, idle: time.Second}

	responses, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.NoError(t, err)
	require.Len(t, responses, 1)
	require.JSONEq(t, `{"objective":"x"}`, textOf(responses[0]))
}

// complete() used to keep only UsageMetadata, CustomMetadata and TurnComplete
// from the stream's last item, silently dropping FinishReason, ErrorCode and
// ErrorMessage — the fields that say a turn ended for safety or content
// filtering rather than completing normally.
func TestStreamedModelPreservesFinishReasonAndErrorFields(t *testing.T) {
	final := &model.LLMResponse{
		Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: `{"ok":true}`}}},
		FinishReason: genai.FinishReasonSafety,
		ErrorCode:    "safety",
		ErrorMessage: "blocked by safety filter",
	}
	inner := &scriptedStream{responses: []*model.LLMResponse{
		textResponse(`{"ok":`),
		textResponse(`true}`),
		final,
	}}
	decorated := streamedModel{inner: inner, idle: time.Second}

	responses, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.NoError(t, err)
	require.Len(t, responses, 1)
	require.Equal(t, genai.FinishReasonSafety, responses[0].FinishReason)
	require.Equal(t, "safety", responses[0].ErrorCode)
	require.Equal(t, "blocked by safety filter", responses[0].ErrorMessage)
}

// A provider that answers in one shot must survive the collapse unchanged.
func TestStreamedModelPassesThroughASingleResponseStream(t *testing.T) {
	inner := &scriptedStream{responses: []*model.LLMResponse{textResponse(`{"objective":"x"}`)}}
	decorated := streamedModel{inner: inner, idle: time.Second}

	responses, err := collect(decorated.GenerateContent(context.Background(), &model.LLMRequest{}, false))
	require.NoError(t, err)
	require.Len(t, responses, 1)
	require.JSONEq(t, `{"objective":"x"}`, textOf(responses[0]))
}
