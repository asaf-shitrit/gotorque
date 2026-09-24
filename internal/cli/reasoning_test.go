package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"example.com/gotorque/internal/agents"
)

func TestTheOptimizerDefaultsToLowOnlyWhenCodeChoosesTheTarget(t *testing.T) {
	var out bytes.Buffer
	chosen := agents.Reasoning{}
	require.NoError(t, defaultOptimizerReasoning(&out, chosen, true))
	require.Equal(t, agents.ReasoningLow, chosen[agents.RoleOptimizer])
	require.Contains(t, out.String(), "reasoning effort low")

	out.Reset()
	modelAnalyst := agents.Reasoning{}
	require.NoError(t, defaultOptimizerReasoning(&out, modelAnalyst, false))
	require.Empty(t, modelAnalyst)
	require.Empty(t, out.String())

	explicit := agents.Reasoning{agents.RoleOptimizer: agents.ReasoningHigh}
	require.NoError(t, defaultOptimizerReasoning(&out, explicit, true))
	require.Equal(t, agents.ReasoningHigh, explicit[agents.RoleOptimizer])
	require.Empty(t, out.String())
}
