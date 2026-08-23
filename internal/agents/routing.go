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

type Routing map[Role]string

func DefaultRouting() Routing {
	return Routing{
		RoleCoordinator: "gpt-5.6-sol",
		RoleExplorer:    "gpt-5.6-luna",
		RoleAnalyst:     "gpt-5.6-terra",
		RoleOptimizer:   "gpt-5.6-sol",
		RoleReviewer:    "gpt-5.6-terra",
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
