package agents

import (
	"fmt"
	"os"
)

const (
	EnvCoordinatorModel = "GOTORQUE_MODEL_COORDINATOR"
	EnvExplorerModel    = "GOTORQUE_MODEL_EXPLORER"
	EnvAnalystModel     = "GOTORQUE_MODEL_ANALYST"
	EnvOptimizerModel   = "GOTORQUE_MODEL_OPTIMIZER"
	EnvReviewerModel    = "GOTORQUE_MODEL_REVIEWER"
)

// DefaultModel is the model every role uses unless its GOTORQUE_MODEL_* env
// var overrides it. Reasoning model: roles must allow enough completion tokens
// for reasoning output ahead of the JSON payload the workflow nodes parse.
const DefaultModel = "deepseek/deepseek-v4.1-flash"

type Routing map[Role]string

func DefaultRouting() Routing {
	return Routing{
		RoleCoordinator: DefaultModel,
		RoleExplorer:    DefaultModel,
		RoleAnalyst:     DefaultModel,
		RoleOptimizer:   DefaultModel,
		RoleReviewer:    DefaultModel,
	}
}

// RoutingFromEnvironment reads model IDs only. Endpoint and credential values
// remain owned by the OpenAI-compatible adapter and are never returned here or
// persisted in campaign state.
func RoutingFromEnvironment() Routing {
	routing := DefaultRouting()
	for role, key := range map[Role]string{RoleCoordinator: EnvCoordinatorModel, RoleExplorer: EnvExplorerModel, RoleAnalyst: EnvAnalystModel, RoleOptimizer: EnvOptimizerModel, RoleReviewer: EnvReviewerModel} {
		if value := os.Getenv(key); value != "" {
			routing[role] = value
		}
	}
	return routing
}

func (r Routing) Validate() error {
	for _, role := range AllRoles {
		if r[role] == "" {
			return fmt.Errorf("model ID for %s is required", role)
		}
	}
	return nil
}

const (
	EnvCoordinatorReasoning = "GOTORQUE_REASONING_COORDINATOR"
	EnvExplorerReasoning    = "GOTORQUE_REASONING_EXPLORER"
	EnvAnalystReasoning     = "GOTORQUE_REASONING_ANALYST"
	EnvOptimizerReasoning   = "GOTORQUE_REASONING_OPTIMIZER"
	EnvReviewerReasoning    = "GOTORQUE_REASONING_REVIEWER"
)

// ReasoningEffort is the Responses API reasoning.effort one role's requests
// carry.
type ReasoningEffort string

const (
	ReasoningLow    ReasoningEffort = "low"
	ReasoningMedium ReasoningEffort = "medium"
	ReasoningHigh   ReasoningEffort = "high"
)

// Reasoning maps roles to the effort their requests ask for. A role absent
// from the map sends no reasoning field, leaving the provider's default in
// force. Without this there is no knob at all: ADK's openaimodel never maps
// genai's ThinkingConfig onto the request, so a campaign ran at whatever
// effort the provider picked.
//
// Like Routing, it is configuration read from the environment and holds no
// credential.
type Reasoning map[Role]ReasoningEffort

var reasoningEnv = map[Role]string{
	RoleCoordinator: EnvCoordinatorReasoning,
	RoleExplorer:    EnvExplorerReasoning,
	RoleAnalyst:     EnvAnalystReasoning,
	RoleOptimizer:   EnvOptimizerReasoning,
	RoleReviewer:    EnvReviewerReasoning,
}

// ReasoningFromEnvironment reads GOTORQUE_REASONING_* as given. Values are
// checked by Validate, so a typo fails the preflight instead of being dropped.
func ReasoningFromEnvironment() Reasoning {
	reasoning := Reasoning{}
	for role, key := range reasoningEnv {
		if value := os.Getenv(key); value != "" {
			reasoning[role] = ReasoningEffort(value)
		}
	}
	return reasoning
}

// Validate rejects any effort other than low, medium, or high. An unknown
// value is not passed on for the endpoint to judge: providers disagree on
// what they accept, and one that ignores the field would run the campaign at
// its default while the operator believed otherwise.
func (r Reasoning) Validate() error {
	for _, role := range AllRoles {
		switch effort := r[role]; effort {
		case "", ReasoningLow, ReasoningMedium, ReasoningHigh:
		default:
			return fmt.Errorf("%s=%q is not a reasoning effort: use low, medium, or high, or leave it unset", reasoningEnv[role], string(effort))
		}
	}
	return nil
}
