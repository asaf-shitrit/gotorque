package agents

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReasoningFromEnvironmentReadsOnlySetRoles(t *testing.T) {
	for _, key := range reasoningEnv {
		t.Setenv(key, "")
	}
	t.Setenv(EnvOptimizerReasoning, "high")
	t.Setenv(EnvReviewerReasoning, "low")

	reasoning := ReasoningFromEnvironment()
	require.Equal(t, Reasoning{RoleOptimizer: ReasoningHigh, RoleReviewer: ReasoningLow}, reasoning)
	require.NoError(t, reasoning.Validate())
}

// An unset environment must leave requests exactly as they were before the
// knob existed: no role carries an effort.
func TestReasoningFromEnvironmentDefaultsToNothing(t *testing.T) {
	for _, key := range reasoningEnv {
		t.Setenv(key, "")
	}
	require.Empty(t, ReasoningFromEnvironment())
}

func TestReasoningValidateNamesTheVariableAndValue(t *testing.T) {
	for _, role := range AllRoles {
		err := Reasoning{role: "High"}.Validate()
		require.ErrorContains(t, err, reasoningEnv[role])
		require.ErrorContains(t, err, `"High"`)
		require.ErrorContains(t, err, "low, medium, or high")
	}
	require.NoError(t, Reasoning{RoleCoordinator: ReasoningLow, RoleExplorer: ReasoningMedium, RoleAnalyst: ReasoningHigh}.Validate())
	require.NoError(t, Reasoning(nil).Validate())
}

func TestEveryRoleHasAReasoningVariable(t *testing.T) {
	require.Len(t, reasoningEnv, len(AllRoles))
	for _, role := range AllRoles {
		require.NotEmpty(t, reasoningEnv[role], "role %s", role)
	}
}

func TestReasoningDefaultNeverOverridesTheEnvironment(t *testing.T) {
	t.Setenv(EnvOptimizerReasoning, "")
	unset := ReasoningFromEnvironment()
	if !unset.Default(RoleOptimizer, ReasoningLow) || unset[RoleOptimizer] != ReasoningLow {
		t.Fatalf("an unset optimizer effort should default to low, got %+v", unset)
	}
	t.Setenv(EnvOptimizerReasoning, "high")
	explicit := ReasoningFromEnvironment()
	if explicit.Default(RoleOptimizer, ReasoningLow) || explicit[RoleOptimizer] != ReasoningHigh {
		t.Fatalf("an explicit effort must win, got %+v", explicit)
	}
}
