package agents

import (
	"fmt"
	"os"
)

const (
	EnvOptimizerModel     = "GOTORQUE_MODEL_OPTIMIZER"
	EnvOptimizerReasoning = "GOTORQUE_REASONING_OPTIMIZER"
)

// DefaultModel is the optimizer's model unless GOTORQUE_MODEL_OPTIMIZER
// overrides it. Reasoning model: the optimizer must allow enough completion
// tokens for reasoning output ahead of the JSON payload the workflow parses.
const DefaultModel = "deepseek/deepseek-v4.1-flash"

// ModelFromEnvironment reads the optimizer's model ID only. Endpoint and
// credential values remain owned by the OpenAI-compatible adapter and are
// never returned here or persisted in campaign state.
func ModelFromEnvironment() string {
	if value := os.Getenv(EnvOptimizerModel); value != "" {
		return value
	}
	return DefaultModel
}

// ReasoningEffort is the Responses API reasoning.effort the optimizer's
// requests carry. Empty sends no reasoning field, leaving the provider's
// default in force. Without it there is no knob at all: ADK's openaimodel
// never maps genai's ThinkingConfig onto the request.
type ReasoningEffort string

const (
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

// ReasoningFromEnvironment reads GOTORQUE_REASONING_OPTIMIZER as given. The
// value is checked by Validate, so a typo fails the preflight instead of being
// dropped.
func ReasoningFromEnvironment() ReasoningEffort {
	return ReasoningEffort(os.Getenv(EnvOptimizerReasoning))
}

// Validate rejects any effort other than low, medium, or high. An unknown
// value is not passed on for the endpoint to judge: providers disagree on
// what they accept, and one that ignores the field would run the campaign at
// its default while the operator believed otherwise.
func (e ReasoningEffort) Validate() error {
	switch e {
	case "", ReasoningLow, ReasoningMedium, ReasoningHigh:
		return nil
	}
	return fmt.Errorf("%s=%q is not a reasoning effort: use low, medium, or high, or leave it unset", EnvOptimizerReasoning, string(e))
}
