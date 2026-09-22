package agents

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// streamIdleTimeout bounds silence inside one model call.
//
// It replaces a whole-request timeout, and the difference is the point. The
// endpoint sends nothing until generation finishes when a call is made
// non-streaming, so the only bound available was on total duration — and that
// bound cut calls that were working. Observed against OpenRouter: role calls
// legitimately finishing in fifteen seconds to three minutes, while stall
// failures reported `error reading response body: context deadline exceeded`
// at exactly the four-minute client timeout, in every campaign round, two to
// eight times per round. Worst case the ladder burned four minutes per stalled
// attempt with nothing to show, and the campaign lost the candidates it could
// have evaluated in that time.
//
// With streaming, bytes flow while the model works, so idleness is
// measurable: a call that keeps producing is allowed to keep going, and one
// that goes quiet is abandoned and retried. Two minutes is chosen to clear
// provider queueing delays while still failing stalls in half the time the old
// bound took.
const streamIdleTimeout = 2 * time.Minute

// errNoResponse reports a stream that ended without ever carrying a response.
var errNoResponse = errors.New("model stream produced no response")

// streamedModel runs every call through the endpoint's streaming API
// regardless of what the caller asked for, collapses the chunks back into the
// single response a non-streaming call would have produced, and fails a call
// that stops producing for longer than idle.
//
// ADK requests non-streaming responses (it streams only in SSE mode), so
// without this decorator the transport stays silent for the whole generation
// and a stalled call is indistinguishable from a slow one until a deadline
// kills both.
type streamedModel struct {
	inner model.LLM
	idle  time.Duration
}

func newStreamedModel(inner model.LLM) streamedModel {
	return streamedModel{inner: inner, idle: streamIdleTimeout}
}

func (m streamedModel) Name() string { return m.inner.Name() }

func (m streamedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		chunks := pump(ctx, m.inner, req)
		last, err := m.drain(ctx, chunks, cancel)
		if err != nil {
			yield(nil, err)
			return
		}
		if last == nil {
			yield(nil, errNoResponse)
			return
		}
		yield(last, nil)
	}
}

// chunk is one item of the inner stream.
type chunk struct {
	resp *model.LLMResponse
	err  error
}

// pump reads the inner streaming sequence on its own goroutine so drain can
// select on it against a timer. The goroutine ends when the sequence ends or
// when ctx is cancelled, which is what drain does on a stall.
func pump(ctx context.Context, inner model.LLM, req *model.LLMRequest) <-chan chunk {
	chunks := make(chan chunk)
	go func() {
		defer close(chunks)
		for resp, err := range inner.GenerateContent(ctx, req, true) {
			select {
			case chunks <- chunk{resp: resp, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return chunks
}

// drain consumes the stream and rebuilds the response a non-streaming call
// would have produced.
//
// The text comes from the deltas, not from the last response. ADK's streaming
// path yields the answer incrementally and then, as its final item, an
// aggregated response that repeats the answer and carries any reasoning text
// alongside it: keeping that final response produced the answer twice on a
// live OpenRouter call (`{"ok":true}{"ok":true}`) and mixed reasoning into
// the payload the decoder has to parse. Usage and completion state still come
// from the last item, which is where the endpoint reports them.
func (m streamedModel) drain(ctx context.Context, chunks <-chan chunk, cancel context.CancelFunc) (*model.LLMResponse, error) {
	var (
		pending *model.LLMResponse
		text    []string
	)
	idle := time.NewTimer(m.idle)
	defer idle.Stop()
	for {
		select {
		case item, ok := <-chunks:
			if !ok {
				return complete(pending, text), nil
			}
			if item.err != nil {
				return nil, item.err
			}
			pending, text = holdResponse(pending, text, item.resp)
			resetTimer(idle, m.idle)
		case <-idle.C:
			cancel()
			return nil, fmt.Errorf("model stream stalled: no chunk for %s", m.idle)
		case <-ctx.Done():
			// context.Cause, not ctx.Err(): fence.go's per-attempt bound now
			// sets a cause naming the attempt budget, and this is the only
			// path back to the observer log and the degraded-role record.
			// Cause propagates a parent's cancellation (Ctrl-C, the
			// campaign's max_duration) unchanged, so that reason still reads
			// as itself rather than as this attempt's budget.
			return nil, context.Cause(ctx)
		}
	}
}

// complete returns the response a non-streaming call would have produced: the
// accumulated deltas, carrying the usage the endpoint reported on its last
// item. A stream whose deltas never carried plain text — a single-response
// stream, or one that only streamed reasoning — falls back to that last item,
// which then holds the answer.
func complete(last *model.LLMResponse, text []string) *model.LLMResponse {
	if len(text) == 0 {
		return last
	}
	role := "model"
	var (
		usage        *genai.GenerateContentResponseUsageMetadata
		metadata     map[string]any
		turnComplete bool
		finishReason genai.FinishReason
		errorCode    string
		errorMessage string
	)
	if last != nil {
		usage, metadata, turnComplete = last.UsageMetadata, last.CustomMetadata, last.TurnComplete
		finishReason, errorCode, errorMessage = last.FinishReason, last.ErrorCode, last.ErrorMessage
		if last.Content != nil && last.Content.Role != "" {
			role = last.Content.Role
		}
	}
	return &model.LLMResponse{
		Content:        &genai.Content{Role: role, Parts: []*genai.Part{{Text: joinText(text)}}},
		UsageMetadata:  usage,
		CustomMetadata: metadata,
		TurnComplete:   turnComplete,
		FinishReason:   finishReason,
		ErrorCode:      errorCode,
		ErrorMessage:   errorMessage,
	}
}

// holdResponse pushes one streamed item through a one-item delay line: the held
// item is known to be a delta only once another arrives, so the stream's last
// item — the aggregated response — is never accumulated. Folding it in
// duplicated the answer on a live call.
func holdResponse(pending *model.LLMResponse, text []string, resp *model.LLMResponse) (*model.LLMResponse, []string) {
	if resp == nil {
		return pending, text
	}
	if pending != nil {
		text = append(text, answerText(pending)...)
	}
	return resp, text
}

// answerText returns the plain text parts of one streamed response, in order.
// Reasoning parts are skipped: they are the model thinking out loud, and the
// decoder downstream expects the payload, not the deliberation.
func answerText(resp *model.LLMResponse) []string {
	if resp == nil || resp.Content == nil {
		return nil
	}
	parts := make([]string, 0, len(resp.Content.Parts))
	for _, part := range resp.Content.Parts {
		if part == nil || part.Text == "" || part.Thought {
			continue
		}
		parts = append(parts, part.Text)
	}
	return parts
}

func joinText(parts []string) string {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]byte, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return string(out)
}

// resetTimer restarts a timer that may already have fired, without the
// classic drain deadlock.
func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}
