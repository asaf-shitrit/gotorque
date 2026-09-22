package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// RoleUsage aggregates token usage across every model call made for one role.
type RoleUsage struct {
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// UsageCollector accumulates per-role token usage from decorated models.
// It is safe for concurrent use; the zero value is ready for Record.
type UsageCollector struct {
	mu     sync.Mutex
	ByRole map[string]*RoleUsage
}

// NewUsageCollector returns an empty collector keyed by role name.
func NewUsageCollector() *UsageCollector {
	return &UsageCollector{ByRole: map[string]*RoleUsage{}}
}

// Record folds one response's usage metadata into the running totals for
// role. Nil collectors and nil metadata are ignored so decoration stays
// transparent when an endpoint omits usage data.
func (c *UsageCollector) Record(role string, u *genai.GenerateContentResponseUsageMetadata) {
	if c == nil || u == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ByRole == nil {
		c.ByRole = map[string]*RoleUsage{}
	}
	usage := c.ByRole[role]
	if usage == nil {
		usage = &RoleUsage{}
		c.ByRole[role] = usage
	}
	usage.Requests++
	usage.PromptTokens += int64(u.PromptTokenCount)
	usage.CompletionTokens += int64(u.CandidatesTokenCount)
	usage.TotalTokens += int64(u.TotalTokenCount)
}

// Snapshot returns a copy of the per-role totals keyed by role name.
func (c *UsageCollector) Snapshot() map[string]RoleUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := make(map[string]RoleUsage, len(c.ByRole))
	for role, usage := range c.ByRole {
		if usage == nil {
			continue
		}
		snapshot[role] = *usage
	}
	return snapshot
}

// fenceStrippingModel wraps an LLM and removes Markdown code fences that
// models frequently wrap around JSON payloads. Typed workflow nodes parse
// model text as raw JSON, so an otherwise valid response like
// "```json\n{...}\n```" would fail output validation. It also records token
// usage metadata into a shared collector when one is supplied.
type fenceStrippingModel struct {
	inner    model.LLM
	role     string
	usage    *UsageCollector
	observer CallObserver

	// attempts, baseBackoff and attemptTimeout are fields rather than
	// constants so tests can drive the full retry ladder without minutes of
	// real sleeping.
	attempts       int
	baseBackoff    time.Duration
	attemptTimeout time.Duration
}

// Transparent retries: shared-pool rate limits (HTTP 429), transport
// errors, and complete-but-unusable payloads all otherwise abort a
// multi-hour campaign on a single bad roll. The ladder is 15s, 30s, 60s.
const (
	defaultGenerateAttempts = 4
	defaultGenerateBackoff  = 15 * time.Second
)

// NewFenceStrippingModel decorates an LLM so fenced JSON responses are
// normalized before any downstream validation runs. The role labels the
// collector's per-role totals; the collector may be nil to skip tracking.
// The observer, when non-nil, receives one CallInfo per attempt.
func NewFenceStrippingModel(inner model.LLM, role string, usage *UsageCollector, observer CallObserver) model.LLM {
	if inner == nil {
		return nil
	}
	return &fenceStrippingModel{inner: inner, role: role, usage: usage, observer: observer, attempts: defaultGenerateAttempts, baseBackoff: defaultGenerateBackoff, attemptTimeout: attemptTimeout}
}

func (m fenceStrippingModel) Name() string { return m.inner.Name() }

func (m fenceStrippingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		run := &generateRun{}
		var backoff time.Duration
		for attempt := 0; attempt < m.attempts; attempt++ {
			if err := waitBackoff(ctx, backoff); err != nil {
				yield(nil, err)
				return
			}
			started := time.Now()
			// The start line is what separates a slow endpoint from a hung one:
			// a completion line alone arrives only once the attempt is over, so
			// four minutes of silence reads the same as four minutes of work.
			m.observer.observe(CallInfo{Role: m.role, Attempt: attempt + 1, Started: true})
			done := m.runAttempt(ctx, req, stream, yield, run)
			// Reported per attempt rather than per call: a campaign that
			// prints only the final outcome cannot distinguish a slow
			// endpoint from one silently retrying behind a long wait.
			m.observer.observe(CallInfo{
				Role:     m.role,
				Attempt:  attempt + 1,
				Duration: time.Since(started),
				Err:      run.lastErr,
				Retrying: !done && attempt+1 < m.attempts,
			})
			if done {
				return
			}
			backoff = time.Duration(1<<uint(attempt)) * m.baseBackoff
		}
		run.yieldExhausted(yield, m.role, m.attempts)
	}
}

// runAttempt runs one ladder attempt under its own deadline.
//
// The bound exists to keep the ladder inside the node deadline: the
// orchestrator allows 20m per agent node, and four attempts plus backoff have
// to fit, or a stalled provider kills the node mid-ladder and takes the
// campaign with it. Observed: a single explorer attempt ran 12m1s and
// consumed the whole per-node deadline before the campaign had evaluated a
// single candidate. Silence inside an attempt is now caught far sooner by
// streamIdleTimeout, so this bound is the backstop rather than the first line
// of defence. A non-positive attemptTimeout leaves the attempt unbounded.
func (m fenceStrippingModel) runAttempt(ctx context.Context, req *model.LLMRequest, stream bool, yield func(*model.LLMResponse, error) bool, run *generateRun) bool {
	if m.attemptTimeout <= 0 {
		return m.streamAttempt(ctx, req, stream, yield, run)
	}
	// WithTimeoutCause, not WithTimeout: a bare context.DeadlineExceeded names
	// neither which budget ran out nor its size, so the observer log and the
	// degraded-role record could not distinguish this bound from the caller's
	// own cancellation (Ctrl-C, campaign max_duration). drain in stream.go
	// reports context.Cause(ctx), which carries this cause through unless a
	// parent context was cancelled first.
	cause := fmt.Errorf("model call attempt exceeded its %s budget", m.attemptTimeout)
	attemptCtx, cancel := context.WithTimeoutCause(ctx, m.attemptTimeout, cause)
	defer cancel()
	return m.streamAttempt(attemptCtx, req, stream, yield, run)
}

// generateRun carries what one decorated call has learned across its
// attempts. last and lastErr describe the most recent attempt only — each
// attempt overwrites both — so the exhaustion path reports the failure the
// caller actually ended on rather than an older one.
type generateRun struct {
	last    *model.LLMResponse
	lastErr error
	partial bool
}

func (r *generateRun) recordErr(err error) { r.last, r.lastErr = nil, err }

func (r *generateRun) recordUnusable(resp *model.LLMResponse) { r.last, r.lastErr = resp, nil }

// yieldExhausted delivers the one response or error a spent retry ladder is
// allowed to produce.
//
// A response carrying no answer part is converted into an error instead of
// being yielded, because ADK's two consumers of such a turn disagree and the
// disagreement is fatal: the workflow agent node stamps the turn's (empty)
// text as the node's output, while the LLM flow classifies it as thinking
// rather than answering and calls the model again inside the same node
// execution. The second call's answer then becomes a second output-bearing
// event and the workflow scheduler kills the run with ErrMultipleOutputs. An
// error here costs one node, not the campaign.
//
// An answer that merely failed to parse as JSON is still yielded: downstream
// decoding names the offending text, which is far more useful than a generic
// failure from here.
func (r *generateRun) yieldExhausted(yield func(*model.LLMResponse, error) bool, role string, attempts int) {
	if hasAnswerPart(r.last) {
		yield(r.last, nil)
		return
	}
	if r.lastErr != nil {
		yield(nil, fmt.Errorf("%s model call failed after %d attempts: %w", role, attempts, r.lastErr))
		return
	}
	yield(nil, fmt.Errorf("%s model call produced no answer in %d attempts: every response was reasoning-only or empty", role, attempts))
}

// nonRetryableStatus is the set of HTTP statuses the ladder must not retry:
// each one describes the request, the credential or the account, not a
// transient endpoint condition, so every attempt would fail identically. 408,
// 409, 429 and 5xx are deliberately absent — those are the transient cases the
// ladder exists for.
//
// 402 joined the set after a live call against OpenRouter: an account whose
// balance could not cover the requested max_output_tokens answered 402
// Payment Required on every attempt, and the ladder spent 105 seconds proving
// it four times. Nothing changes until someone adds credits.
var nonRetryableStatus = map[int]bool{
	http.StatusBadRequest:          true, // 400: malformed request
	http.StatusUnauthorized:        true, // 401: bad or revoked credential
	http.StatusPaymentRequired:     true, // 402: account cannot pay for the request
	http.StatusForbidden:           true, // 403: credential lacks access
	http.StatusNotFound:            true, // 404: no such model or endpoint
	http.StatusUnprocessableEntity: true, // 422: request rejected by validation
}

// permanentStatusError reports whether err carries an openai-go API error
// (openai.Error, populated from the HTTP response by the SDK's transport
// layer) with a status the ladder cannot fix by retrying, and if so returns
// it wrapped with the role and a note that it was not retried. ADK's
// non-streaming path wraps this in "openai: call failed: %w" and its
// streaming path yields stream.Err() raw, so errors.As is used rather than
// assuming either shape.
func permanentStatusError(role string, err error) (bool, error) {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || !nonRetryableStatus[apiErr.StatusCode] {
		return false, nil
	}
	return true, fmt.Errorf("%s model call failed with HTTP %d, not retried: %w", role, apiErr.StatusCode, err)
}

func waitBackoff(ctx context.Context, backoff time.Duration) error {
	if backoff <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(backoff):
		return nil
	}
}

// handleAttemptErr responds to one error from the inner model, reporting done
// exactly as streamAttempt does. Split out of streamAttempt to keep that
// loop's cognitive complexity within the repo's gate.
func (m fenceStrippingModel) handleAttemptErr(resp *model.LLMResponse, err error, yield func(*model.LLMResponse, error) bool, run *generateRun) bool {
	if ok, permanent := permanentStatusError(m.role, err); ok {
		// A 401 on attempt one is a 401 on attempt four: retrying burns the
		// whole 15s/30s/60s ladder for a key or request that will never
		// succeed, and with a revoked key every role then pays that toll
		// before degrading anyway.
		run.recordErr(permanent)
		yield(nil, permanent)
		return true
	}
	run.recordErr(err)
	if !run.partial {
		return false
	}
	yield(resp, err)
	return true
}

// usableAnswer reports whether a complete response is one the campaign can
// act on: it must carry an answer part, and its visible text must parse as
// the JSON every role is prompted to return.
func usableAnswer(resp *model.LLMResponse) bool {
	return hasAnswerPart(resp) && responseTextIsJSON(resp)
}

// hasAnswerPart reports whether resp carries at least one part ADK's flow
// counts as an answer, mirroring llminternal.isThoughtOnlyTurn: any part not
// marked Thought. A reasoning model that spends its whole output budget
// thinking returns a turn of Thought parts only — see yieldExhausted for why
// such a turn must never reach the flow.
func hasAnswerPart(resp *model.LLMResponse) bool {
	if resp == nil || resp.Content == nil {
		return false
	}
	for _, part := range resp.Content.Parts {
		if part != nil && !part.Thought {
			return true
		}
	}
	return false
}

// streamAttempt runs one pass over the inner model. It reports done when the
// decorated call is finished — a response reached the caller, the caller hung
// up, or partials already escaped — and false when the attempt produced
// nothing deliverable and the ladder should try again.
//
// At most one complete response ever escapes a call, because the loop returns
// the moment it yields one instead of ranging on. A node execution that emits
// two output-bearing events is fatal to the workflow, so the decorator must
// make that structurally impossible rather than trust the inner iterator to
// stop at one.
func (m fenceStrippingModel) streamAttempt(ctx context.Context, req *model.LLMRequest, stream bool, yield func(*model.LLMResponse, error) bool, run *generateRun) bool {
	for resp, err := range m.inner.GenerateContent(ctx, req, stream) {
		if err != nil {
			return m.handleAttemptErr(resp, err, yield, run)
		}
		rewriteResponse(resp)
		m.usage.Record(m.role, resp.UsageMetadata)
		if resp.Partial {
			// Streaming partials must reach the caller as they arrive.
			// Once one has, replaying the turn is no longer transparent,
			// so this attempt becomes the last one.
			run.partial = true
			if !yield(resp, nil) {
				return true
			}
			continue
		}
		if !run.partial && !usableAnswer(resp) {
			run.recordUnusable(resp)
			return false
		}
		yield(resp, nil)
		return true
	}
	// An inner iterator that produced nothing at all is a failed attempt,
	// not a silent success — unless partials already went out.
	return run.partial
}

// responseTextIsJSON reports whether the complete response's visible text
// parses as JSON after fence/prose unwrapping.
func responseTextIsJSON(resp *model.LLMResponse) bool {
	if resp == nil || resp.Content == nil {
		return false
	}
	var b strings.Builder
	for _, part := range resp.Content.Parts {
		if part != nil && !part.Thought {
			b.WriteString(part.Text)
		}
	}
	var payload any
	return json.Unmarshal([]byte(UnwrapJSONFence(b.String())), &payload) == nil
}

func rewriteResponse(resp *model.LLMResponse) {
	// Only rewrite complete responses; streaming partials may split a
	// fence across chunks and must pass through untouched.
	if resp.Content == nil || resp.Partial {
		return
	}
	for _, part := range resp.Content.Parts {
		if part != nil && part.Text != "" {
			part.Text = UnwrapJSONFence(part.Text)
		}
	}
}

// UnwrapJSONFence strips a single Markdown code fence (with optional
// language tag such as "json") surrounding a JSON object or array, and
// extracts a balanced JSON payload from surrounding prose. Text without
// any extractable JSON payload is returned unchanged.
func UnwrapJSONFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if start := strings.Index(trimmed, "```"); start >= 0 {
		body := trimmed[start+3:]
		newline := strings.IndexByte(body, '\n')
		if newline < 0 {
			return text
		}
		body = body[newline+1:]
		if end := strings.LastIndex(body, "```"); end >= 0 {
			body = body[:end]
		}
		trimmed = strings.TrimSpace(body)
	}
	if payload, ok := balancedJSON(trimmed); ok {
		return payload
	}
	return text
}

// balancedJSON returns the first balanced JSON object or array in s,
// ignoring braces inside JSON strings. It returns false when s contains
// no complete JSON container.
func balancedJSON(s string) (string, bool) {
	start := -1
	var open byte
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			inString = scanJSONString(c, &escaped)
			continue
		}
		switch c {
		case '"':
			if depth > 0 {
				inString = true
			}
		case '{', '[':
			if depth == 0 {
				start = i
				open = c
			}
			depth++
		case '}', ']':
			if payload, ok, cont := closeBalanced(s, start, i, open, c, &depth); !cont {
				return payload, ok
			}
		}
	}
	return "", false
}

func closeBalanced(s string, start, i int, open, c byte, depth *int) (string, bool, bool) {
	if *depth == 0 {
		return "", false, true
	}
	*depth--
	if *depth != 0 {
		return "", false, true
	}
	want := byte('}')
	if open == '[' {
		want = ']'
	}
	if c != want {
		return "", false, false
	}
	return s[start : i+1], true, false
}
