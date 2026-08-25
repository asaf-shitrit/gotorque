package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	jsonschema "github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/genai"
)

type roleSpec struct {
	role        Role
	name        string
	description string
	instruction string
}

var roleSpecs = []roleSpec{
	{
		role:        RoleCoordinator,
		name:        "coordinator",
		description: "Chooses the next bounded performance experiment from campaign evidence.",
		instruction: `You coordinate a Go CLI optimization campaign. Choose exactly one bounded next experiment. Respect the supplied campaign limits and optimization policy. Do not claim measurements that are absent. The state includes prior_candidates: every already-evaluated proposal, its verdict, and the policy's reasons. Never repeat a rejected or inconclusive hypothesis; target a different hot path or mechanism each cycle. By your second cycle you must request a source-change experiment (kind "patch") rather than another measurement pass. Return only JSON with objective, next_experiment, and rationale fields. STRICT JSON RULES: Output raw JSON only: no Markdown fences, no commentary. Escape every double quote and backslash inside string values (\\\" and \\\\). Keep stdin and fixture content under 500 characters. Include exactly the listed fields and no others.`,
	},
	{
		role:        RoleExplorer,
		name:        "explorer",
		description: "Proposes CLI entry points and representative or diagnostic workload strategies.",
		instruction: `You explore a source-available Go CLI for runtime paths not limited to existing tests. Propose declarative replayable workloads grounded in repository evidence. Each proposal contains name, arguments, stdin, fixtures, tier, provenance, scaling_dimensions, and expected_valid. Never emit a shell command. Preserve workload evidence tiers and sandbox constraints. Return only JSON with entry_points, proposals, and rationale fields. Produce at most 4 proposals. STRICT JSON RULES: Output raw JSON only: no Markdown fences, no commentary. Escape every double quote and backslash inside string values (\\\" and \\\\). Keep stdin and fixture content under 500 characters. Include exactly the listed fields and no others.`,
	},
	{
		role:        RoleAnalyst,
		name:        "analyst",
		description: "Interprets normalized coverage, pprof, trace, and scaling evidence.",
		instruction: `You analyze deterministic Go coverage, pprof, trace, and scaling summaries. Distinguish measured hot paths from static suspicions and explain likely causes without inventing data. The state may include discovery.hot_functions: measured top CPU functions from a profile of the target; when present, hot_paths should cite those locations in path.go:line form where derivable. Return only JSON with hot_paths, likely_causes, candidate_hypotheses, and additional_checks fields. STRICT JSON RULES: Output raw JSON only: no Markdown fences, no commentary. Escape every double quote and backslash inside string values (\\\" and \\\\). Keep stdin and fixture content under 500 characters. Include exactly the listed fields and no others.`,
	},
	{
		role:        RoleOptimizer,
		name:        "optimizer",
		description: "Proposes one focused behavior-preserving source optimization.",
		instruction: `You optimize a Go CLI without changing behavior, public APIs, formats, outputs, or exit codes. Produce one small reversible patch compatible with the selected Go optimization policy. Never add or upgrade production dependencies. Return only JSON with hypothesis, patch, expected_effect, risks, and validation_plan fields. The state may include source_excerpts showing real code around hot paths; base your patch context lines EXACTLY on that text so git apply succeeds. STRICT JSON RULES: Output raw JSON only: no Markdown fences, no commentary. Escape every double quote and backslash inside string values (\\\" and \\\\). Keep stdin and fixture content under 500 characters. Include exactly the listed fields and no others.`,
	},
	{
		role:        RoleReviewer,
		name:        "reviewer",
		description: "Challenges a candidate's behavior preservation, evidence, and risks.",
		instruction: `You critically review a proposed Go optimization and its measured evidence. Look for behavior changes, benchmark overfitting, unsafe assumptions, portability regressions, and missing validation. Your proceed field is advisory; deterministic policy makes the decision. Return only JSON with proceed, behavior_argument, concerns, and required_checks fields. STRICT JSON RULES: Output raw JSON only: no Markdown fences, no commentary. Escape every double quote and backslash inside string values (\\\" and \\\\). Keep stdin and fixture content under 500 characters. Include exactly the listed fields and no others.`,
	},
}

// NewSet constructs all judgment-heavy ADK single-turn agents using injected
// models. It performs no environment lookup and requires no API key itself.
func NewSet(ctx context.Context, provider ModelProvider) (Set, error) {
	if provider == nil {
		return Set{}, fmt.Errorf("model provider is required")
	}

	created := make(map[Role]adkagent.Agent, len(roleSpecs))
	for _, spec := range roleSpecs {
		m, err := provider.ModelFor(ctx, spec.role)
		if err != nil {
			return Set{}, fmt.Errorf("model for %s: %w", spec.role, err)
		}
		if m == nil {
			return Set{}, fmt.Errorf("model for %s is nil", spec.role)
		}

		a, err := llmagent.New(llmagent.Config{
			Name:                     spec.name,
			Description:              spec.description,
			Model:                    m,
			Instruction:              spec.instruction,
			Mode:                     llmagent.ModeSingleTurn,
			DisallowTransferToParent: true,
			DisallowTransferToPeers:  true,
			// Structured JSON recommendations are long; a small default
			// output cap truncates the payload mid-object and makes it
			// impossible to recover the agent's recommendation.
			GenerateContentConfig: &genai.GenerateContentConfig{MaxOutputTokens: 32768},
		})
		if err != nil {
			return Set{}, fmt.Errorf("create %s agent: %w", spec.role, err)
		}
		created[spec.role] = a
	}

	return Set{
		Coordinator: created[RoleCoordinator],
		Explorer:    created[RoleExplorer],
		Analyst:     created[RoleAnalyst],
		Optimizer:   created[RoleOptimizer],
		Reviewer:    created[RoleReviewer],
	}, nil
}

// roleResponseSchema derives the OpenAI-compatible structured-output JSON
// schema from the Go result type each role must produce. The endpoint then
// enforces valid, schema-shaped JSON instead of relying on prompt discipline.
func roleResponseSchema(role Role) (map[string]any, error) {
	var resultType reflect.Type
	switch role {
	case RoleCoordinator:
		resultType = reflect.TypeOf(CoordinatorResult{})
	case RoleExplorer:
		resultType = reflect.TypeOf(ExplorerResult{})
	case RoleAnalyst:
		resultType = reflect.TypeOf(AnalystResult{})
	case RoleOptimizer:
		resultType = reflect.TypeOf(OptimizerResult{})
	case RoleReviewer:
		resultType = reflect.TypeOf(ReviewerResult{})
	default:
		return nil, fmt.Errorf("no result type registered for role %q", role)
	}
	schema, err := jsonschema.ForType(resultType, nil)
	if err != nil {
		return nil, err
	}
	data, err := schema.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	return document, nil
}
