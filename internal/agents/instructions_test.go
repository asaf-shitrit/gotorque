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
	instruction := optimizerInstruction
	require.Contains(t, instruction, `target.kind is "function_set"`)
	require.Contains(t, instruction, "target.callers")
	require.Contains(t, instruction, "function_source (function_sources for a function_set target)")
}

// TestOptimizerInstructionNamesEarlierCandidates pins the sentence that tells
// the optimizer what --history's earlier_candidates are (ADR 0031).
func TestOptimizerInstructionNamesEarlierCandidates(t *testing.T) {
	require.Contains(t, optimizerInstruction, "earlier_candidates")
}

// TestOptimizerInstructionNamesGoVersion pins the sentence that holds the
// optimizer to the target module's declared Go version.
func TestOptimizerInstructionNamesGoVersion(t *testing.T) {
	require.Contains(t, optimizerInstruction, "go_version")
}

// TestOptimizerInstructionNamesTargetContext pins the sentence that tells the
// optimizer to use target.context's real declarations instead of guessing.
func TestOptimizerInstructionNamesTargetContext(t *testing.T) {
	require.Contains(t, optimizerInstruction, "target.context")
}
