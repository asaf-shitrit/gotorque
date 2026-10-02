package agents

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelFromEnvironmentDefaultsAndOverrides(t *testing.T) {
	t.Setenv(EnvOptimizerModel, "")
	require.Equal(t, DefaultModel, ModelFromEnvironment())
	t.Setenv(EnvOptimizerModel, "vendor/other-model")
	require.Equal(t, "vendor/other-model", ModelFromEnvironment())
}

func TestReasoningFromEnvironmentReadsTheOptimizerVariable(t *testing.T) {
	t.Setenv(EnvOptimizerReasoning, "high")
	require.Equal(t, ReasoningHigh, ReasoningFromEnvironment())
	require.NoError(t, ReasoningFromEnvironment().Validate())
}

// An unset environment must leave requests exactly as they were before the
// knob existed: no effort at all.
func TestReasoningFromEnvironmentDefaultsToNothing(t *testing.T) {
	t.Setenv(EnvOptimizerReasoning, "")
	require.Empty(t, ReasoningFromEnvironment())
	require.NoError(t, ReasoningFromEnvironment().Validate())
}

func TestReasoningValidateNamesTheVariableAndValue(t *testing.T) {
	err := ReasoningEffort("High").Validate()
	require.ErrorContains(t, err, EnvOptimizerReasoning)
	require.ErrorContains(t, err, `"High"`)
	require.ErrorContains(t, err, "low, medium, or high")
	for _, ok := range []ReasoningEffort{"", ReasoningLow, ReasoningMedium, ReasoningHigh} {
		require.NoError(t, ok.Validate())
	}
}
