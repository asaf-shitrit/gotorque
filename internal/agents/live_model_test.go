package agents

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// envLiveModel opts a test into a live endpoint call. This is the only place
// the package touches the network, because the property worth proving here
// cannot be proven against a double: that a named OpenRouter model answers a
// prompt in a shape the campaign's decode layer accepts, through the same
// streaming, retry, fencing and usage-accounting path a campaign uses.
//
// Any OpenRouter model id works, free ones included:
//
//	GOTORQUE_LIVE_MODEL=deepseek/deepseek-v4.1-flash go test ./internal/agents -run TestLiveModelAnswersAnOptimizerPrompt -v
//
// Without the variable, or without a credential, the test skips, so CI stays
// offline and needs no secret.
const envLiveModel = "GOTORQUE_LIVE_MODEL"

func TestLiveModelAnswersAnOptimizerPrompt(t *testing.T) {
	modelID := strings.TrimSpace(os.Getenv(envLiveModel))
	if modelID == "" {
		t.Skipf("set %s to an OpenRouter model id to run this test", envLiveModel)
	}
	provider := NewOpenAIProviderFromEnvironment()
	if provider.APIKey == "" {
		t.Skipf("%s is not set, so %s cannot be reached", EnvAPIKey, modelID)
	}
	// ValidateConnectivity checks the model against the endpoint's catalogue,
	// and for a new or free slug that check is half the point of the test.
	provider.Model = modelID

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// A wrong slug, a dead endpoint, or a bad credential surfaces here, in the
	// tool's own words, rather than as a confusing decode failure later.
	require.NoError(t, provider.ValidateConnectivity(ctx), "connectivity to %s", modelID)

	llm, err := provider.OptimizerModel(ctx)
	require.NoError(t, err)

	// The prompt is the tool's own contract in miniature: one JSON object, the
	// way every role is asked to answer.
	instruction := `Return one JSON object with exactly two string keys, "objective" and "next_experiment", describing how to find the hottest function in a Go CLI. No prose and no code fence.`
	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: instruction}}}}}
	started := time.Now()
	parts := make([]string, 0, 8)
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		require.NoError(t, err)
		if resp == nil || resp.Content == nil {
			continue
		}
		for _, part := range resp.Content.Parts {
			if part != nil && part.Text != "" {
				parts = append(parts, part.Text)
			}
		}
	}
	raw := strings.Join(parts, "")
	require.NotEmpty(t, raw, "the model returned no text")

	decoded, err := DecodeResult[planResult](raw)
	require.NoError(t, err, "raw model output: %q", raw)
	require.NotEmpty(t, decoded.Objective, "decoded: %+v", decoded)

	// Usage accounting is what a campaign reports as its cost, so a model whose
	// usage never reaches the collector is not usable for a campaign.
	optimizer, ok := provider.UsageReporter().Snapshot()[string(RoleOptimizer)]
	require.True(t, ok, "per-role accounting must record %s", modelID)
	require.Positive(t, optimizer.TotalTokens, "usage: %+v", optimizer)

	t.Logf("model=%s elapsed=%s objective=%q tokens=%d",
		modelID, time.Since(started).Round(time.Millisecond), decoded.Objective, optimizer.TotalTokens)
}
