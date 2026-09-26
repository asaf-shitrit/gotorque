package agents

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOptimizerInstructionDescribesFunctionSets pins the fix for the
// optimizer never being told about function_sources: its instruction said
// "do not patch any other function" and listed only function_source among
// the fields to return, so on a function_set target (ADR 0027) it rewrote
// the callee alone and the switched-caller rule rejected the attempt
// (multifn-dasel-3, attempts 1 and 2).
func TestOptimizerInstructionDescribesFunctionSets(t *testing.T) {
	var instruction string
	for _, spec := range roleSpecs {
		if spec.role == RoleOptimizer {
			instruction = spec.instruction
		}
	}
	require.NotEmpty(t, instruction)
	require.Contains(t, instruction, `target.kind is "function_set"`)
	require.Contains(t, instruction, "target.callers")
	require.Contains(t, instruction, "function_source (function_sources for a function_set target)")
}
