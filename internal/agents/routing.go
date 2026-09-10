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
