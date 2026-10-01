package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// reasoningBudget is what one decorated model call shares between its retry
// ladder (fence.go), the stream reader that sees how an attempt ended
// (sse.go), and the transport that shapes the next request.
//
// An optimizer attempt can spend every output token reasoning and never write
// an answer. #49's diagnostics recorded it on held-out golines: 32768 output
// tokens, all 32768 of them reasoning, three times in a row, so the identical
// retries were the same runaway again and the attempt was lost. After a cutoff
// like that the remaining attempts go out with reasoning disabled, which the
// optimizer's model honors: on a prompt that drew 5826 reasoning tokens at
// high effort, "enabled": false drew none and still answered in full.
type reasoningBudget struct {
	// exhausted is set by the stream reader when an attempt is cut at
	// max_output_tokens having written nothing but reasoning.
	exhausted atomic.Bool
	// off is set by the ladder: later attempts go out with reasoning disabled.
	off atomic.Bool
}

type reasoningBudgetKey struct{}

func withReasoningBudget(ctx context.Context, b *reasoningBudget) context.Context {
	return context.WithValue(ctx, reasoningBudgetKey{}, b)
}

func reasoningBudgetFrom(ctx context.Context) *reasoningBudget {
	b, _ := ctx.Value(reasoningBudgetKey{}).(*reasoningBudget)
	return b
}

// reasoningOnly reports whether a cut-off response spent its whole output
// budget on reasoning and wrote no visible text.
func reasoningOnly(resp incompleteResponse) bool {
	u := resp.Usage
	return resp.IncompleteDetails.Reason == "max_output_tokens" && u.OutputTokens > 0 &&
		u.OutputTokensDetails.ReasoningTokens >= u.OutputTokens && strings.TrimSpace(visibleText(resp)) == ""
}

// reasoningOffTransport disables reasoning on a request whose call has
// switched it off. reasoningTransport wraps it, so it runs after the effort is
// set and replaces it.
type reasoningOffTransport struct {
	base http.RoundTripper
}

func (t reasoningOffTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b := reasoningBudgetFrom(req.Context())
	if b == nil || !b.off.Load() || req.Method != http.MethodPost || req.Body == nil || req.Body == http.NoBody || !strings.HasSuffix(req.URL.Path, "/responses") {
		return t.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read request body to disable reasoning: %w", err)
	}
	body, err = withReasoningDisabled(body)
	if err != nil {
		return nil, fmt.Errorf("disable reasoning: %w", err)
	}
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return t.base.RoundTrip(out)
}

// withReasoningDisabled replaces the request's reasoning settings with
// {"enabled": false}, keeping every other field's bytes.
func withReasoningDisabled(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["reasoning"] = json.RawMessage(`{"enabled":false}`)
	return json.Marshal(fields)
}
