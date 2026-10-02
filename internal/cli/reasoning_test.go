package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/asaf-shitrit/gotorque/internal/agents"
)

func TestTheOptimizerDefaultsToLowWhenEffortIsUnset(t *testing.T) {
	var out bytes.Buffer
	unset := agents.OpenAIProvider{}
	require.NoError(t, defaultOptimizerReasoning(&out, &unset))
	require.Equal(t, agents.ReasoningLow, unset.Reasoning)
	require.Contains(t, out.String(), "reasoning effort low")

	out.Reset()
	explicit := agents.OpenAIProvider{Reasoning: agents.ReasoningHigh}
	require.NoError(t, defaultOptimizerReasoning(&out, &explicit))
	require.Equal(t, agents.ReasoningHigh, explicit.Reasoning)
	require.Empty(t, out.String())
}
