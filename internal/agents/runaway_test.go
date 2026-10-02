package agents

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// sseReasoningRunaway is the held-out optimizer runaway as OpenRouter sends
// it: cut at max_output_tokens, every output token spent reasoning, and the
// reasoning returned as an output item with text but no message item.
const sseReasoningRunaway = "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"sequence_number\":4,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"incomplete\",\"model\":\"m\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":32768,\"total_tokens\":32771,\"output_tokens_details\":{\"reasoning_tokens\":32768}},\"output\":[{\"type\":\"reasoning\",\"content\":[{\"type\":\"reasoning_text\",\"text\":\"Hmm, decision: use race.ReadSlice. Hold on, let me reconsider\"}]}]}}\n\n"

// TestAReasoningRunawayIsRetriedWithReasoningOff runs the real ladder, role
// model and transport chain against a fake endpoint whose first answer is a
// reasoning-only cutoff: the retry must go out with reasoning disabled and
// its answer must come back.
func TestAReasoningRunawayIsRetriedWithReasoningOff(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = io.WriteString(w, sseCreated+sseReasoningRunaway)
			return
		}
		_, _ = io.WriteString(w, sseCreated+sseItemAdded+sseDeltaOne+sseDeltaTwo+sseCompleted)
	}))
	t.Cleanup(server.Close)
	p := OpenAIProvider{APIKey: "secret", BaseURL: server.URL + "/v1", Model: "test/model", Reasoning: ReasoningLow}
	inner, err := p.endpointModel(context.Background())
	require.NoError(t, err)
	m := &fenceStrippingModel{inner: newStreamedModel(inner), role: string(RoleOptimizer), attempts: 3}

	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "patch it"}}}},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: MaxOutputTokens},
	}
	var answer string
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		require.NoError(t, err)
		if resp != nil && !resp.Partial && resp.Content != nil {
			answer = strings.Join(answerText(resp), "")
		}
	}
	require.JSONEq(t, `{"ok":true}`, answer)
	require.Len(t, bodies, 2)
	require.Contains(t, bodies[0], `"effort":"low"`, "the first attempt keeps the configured effort")
	require.Contains(t, bodies[1], `"reasoning":{"enabled":false}`)
	require.NotContains(t, bodies[1], `"effort"`)
}

func TestReasoningOnlyNeedsAPureReasoningCutoff(t *testing.T) {
	resp := func(reason string, out, reasoning int, text string) incompleteResponse {
		var r incompleteResponse
		r.IncompleteDetails.Reason = reason
		r.Usage.OutputTokens = out
		r.Usage.OutputTokensDetails.ReasoningTokens = reasoning
		if text != "" {
			item := outputItem{Type: "message"}
			item.Content = append(item.Content, struct {
				Text string `json:"text"`
			}{Text: text})
			r.Output = append(r.Output, item)
		}
		reasoningItem := outputItem{Type: "reasoning"}
		reasoningItem.Content = append(reasoningItem.Content, struct {
			Text string `json:"text"`
		}{Text: "thinking about the patch"})
		r.Output = append(r.Output, reasoningItem)
		return r
	}
	require.True(t, reasoningOnly(resp("max_output_tokens", 32768, 32768, "")))
	require.False(t, reasoningOnly(resp("max_output_tokens", 32768, 30000, "")), "some tokens went to output")
	require.False(t, reasoningOnly(resp("max_output_tokens", 32768, 32768, "{\"patch\":")), "visible output was written")
	require.False(t, reasoningOnly(resp("content_filter", 100, 100, "")))
	require.False(t, reasoningOnly(resp("max_output_tokens", 0, 0, "")), "no usage reported")
}
