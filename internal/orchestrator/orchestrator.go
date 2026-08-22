package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"time"

	"example.com/go-agent-optimizer/internal/agents"
	"example.com/go-agent-optimizer/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
)

const (
	stateKey      = "optimizer:campaign_state"
	routeContinue = "continue"
	routeFinish   = "finish"
)

// Dependencies are intentionally narrow so deterministic execution, policy,
// and job persistence can be local, remote, or fake implementations.
type Dependencies struct {
	Runner RunnerService
	Policy PolicyService
	Jobs   JobService
	Agents agents.Set
}

// Orchestrator exposes both the ADK workflow and an Agent wrapper suitable for
// runner.Runner. Model construction remains outside this package.
type Orchestrator struct {
	Workflow *workflow.Workflow
	Agent    adkagent.Agent
	Config   Config
}

// New builds the bounded campaign graph.
func New(deps Dependencies, cfg Config) (*Orchestrator, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	if err := validateDependencies(deps); err != nil {
		return nil, err
	}

	deterministicCfg := workflow.NodeConfig{Timeout: cfg.DeterministicTimeout}
	agentCfg := workflow.NodeConfig{Timeout: cfg.AgentTimeout}

	initialize := workflow.NewFunctionNode("initialize_campaign", func(ctx adkagent.Context, input any) (*session.Event, error) {
		req, err := decodeRequest(input)
		if err != nil {
			return nil, err
		}
		req, err = normalizeRequest(req)
		if err != nil {
			return nil, err
		}
		job, err := deps.Jobs.StartCampaign(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("start campaign job: %w", err)
		}
		if job.ID == "" {
			return nil, fmt.Errorf("start campaign job: empty job ID")
		}
		state := CampaignState{Request: req, Job: job, StartedAt: time.Now()}
		return stateEvent(ctx, state), nil
	}, deterministicCfg)

	inspect := workflow.NewFunctionNode("inspect_repository", func(ctx adkagent.Context, state CampaignState) (*session.Event, error) {
		inventory, err := deps.Runner.Inspect(ctx, state.Request)
		if err != nil {
			return nil, fmt.Errorf("inspect repository: %w", err)
		}
		state.Inspection = inventory
		return stateEvent(ctx, state), nil
	}, deterministicCfg)

	coordinator, err := workflow.NewAgentNode(deps.Agents.Coordinator, agentCfg)
	if err != nil {
		return nil, fmt.Errorf("coordinator node: %w", err)
	}
	mergeCoordinator := workflow.NewFunctionNode("merge_coordinator", func(ctx adkagent.Context, raw any) (*session.Event, error) {
		result, err := agents.DecodeResult[agents.CoordinatorResult](raw)
		if err != nil {
			return nil, fmt.Errorf("coordinator output: %w", err)
		}
		state, err := loadState(ctx)
		if err != nil {
			return nil, err
		}
		state.Coordinator = result
		return stateEvent(ctx, state), nil
	}, deterministicCfg)

	explorer, err := workflow.NewAgentNode(deps.Agents.Explorer, agentCfg)
	if err != nil {
		return nil, fmt.Errorf("explorer node: %w", err)
	}
	discover := workflow.NewFunctionNode("run_discovery", func(ctx adkagent.Context, raw any) (*session.Event, error) {
		result, err := agents.DecodeResult[agents.ExplorerResult](raw)
		if err != nil {
			return nil, fmt.Errorf("explorer output: %w", err)
		}
		state, err := loadState(ctx)
		if err != nil {
			return nil, err
		}
		state.Explorer = result
		evidence, err := deps.Runner.Discover(ctx, DiscoveryRequest{
			Campaign:    state.Request,
			Attempt:     state.CandidatesTried + 1,
			Coordinator: state.Coordinator,
			Explorer:    result,
		})
		if err != nil {
			return nil, fmt.Errorf("run discovery: %w", err)
		}
		state.Discovery = evidence
		return stateEvent(ctx, state), nil
	}, deterministicCfg)

	analyst, err := workflow.NewAgentNode(deps.Agents.Analyst, agentCfg)
	if err != nil {
		return nil, fmt.Errorf("analyst node: %w", err)
	}
	mergeAnalysis := workflow.NewFunctionNode("merge_analysis", func(ctx adkagent.Context, raw any) (*session.Event, error) {
		result, err := agents.DecodeResult[agents.AnalystResult](raw)
		if err != nil {
			return nil, fmt.Errorf("analyst output: %w", err)
		}
		state, err := loadState(ctx)
		if err != nil {
			return nil, err
		}
		state.Analysis = result
		if collector, ok := deps.Runner.(ExcerptCollector); ok {
			// Best-effort enrichment; excerpts are optional, so failures just
			// leave them unset rather than failing the analysis node.
			if excerpts, err := collector.CollectExcerpts(ctx, result); err == nil {
				state.SourceExcerpts = excerpts
			}
		}
		return stateEvent(ctx, state), nil
	}, deterministicCfg)

	optimizer, err := workflow.NewAgentNode(deps.Agents.Optimizer, agentCfg)
	if err != nil {
		return nil, fmt.Errorf("optimizer node: %w", err)
	}
	evaluate := workflow.NewFunctionNode("evaluate_candidate", func(ctx adkagent.Context, raw any) (*session.Event, error) {
		proposal, err := agents.DecodeResult[agents.OptimizerResult](raw)
		if err != nil {
			return nil, fmt.Errorf("optimizer output: %w", err)
		}
		state, err := loadState(ctx)
		if err != nil {
			return nil, err
		}
		state.Proposal = proposal
		evidence, err := deps.Runner.EvaluateCandidate(ctx, CandidateRequest{
			Campaign: state.Request,
			Attempt:  state.CandidatesTried + 1,
			Analysis: state.Analysis,
			Proposal: proposal,
		})
		if err != nil {
			return nil, fmt.Errorf("evaluate candidate: %w", err)
		}
		if evidence.Candidate.ID == "" {
			return nil, fmt.Errorf("evaluate candidate: empty candidate ID")
		}
		state.Candidate = evidence
		return stateEvent(ctx, state), nil
	}, deterministicCfg)

	reviewer, err := workflow.NewAgentNode(deps.Agents.Reviewer, agentCfg)
	if err != nil {
		return nil, fmt.Errorf("reviewer node: %w", err)
	}
	decide := workflow.NewFunctionNode("apply_policy", func(ctx adkagent.Context, raw any) (*session.Event, error) {
		review, err := agents.DecodeResult[agents.ReviewerResult](raw)
		if err != nil {
			return nil, fmt.Errorf("reviewer output: %w", err)
		}
		state, err := loadState(ctx)
		if err != nil {
			return nil, err
		}
		state.Review = review
		evaluation, err := deps.Policy.Evaluate(ctx, PolicyInput{
			Campaign: state.Request,
			Evidence: state.Candidate,
			Review:   review,
		})
		if err != nil {
			return nil, fmt.Errorf("evaluate policy: %w", err)
		}
		if !validDecision(evaluation.Decision) {
			return nil, fmt.Errorf("evaluate policy: invalid decision %q", evaluation.Decision)
		}
		if evaluation.CandidateID == "" {
			evaluation.CandidateID = state.Candidate.Candidate.ID
		}
		if evaluation.CandidateID != state.Candidate.Candidate.ID {
			return nil, fmt.Errorf("evaluate policy: candidate ID %q does not match %q", evaluation.CandidateID, state.Candidate.Candidate.ID)
		}

		state.Evaluation = evaluation
		state.CandidatesTried++
		state.PriorCandidates = append(state.PriorCandidates, PriorCandidate{
			Attempt:    state.CandidatesTried,
			Hypothesis: state.Proposal.Hypothesis,
			Decision:   string(evaluation.Decision),
			Reasons:    evaluation.Reasons,
		})
		if evaluation.Decision == domain.DecisionAccepted {
			if err := deps.Runner.PromoteCandidate(ctx, state.Candidate.Candidate); err != nil {
				return nil, fmt.Errorf("promote candidate: %w", err)
			}
			state.ConsecutiveFailures = 0
			state.AcceptedCandidates = append(state.AcceptedCandidates, evaluation.CandidateID)
		} else {
			state.ConsecutiveFailures++
		}

		progress := CampaignProgress{
			CandidatesTried:     state.CandidatesTried,
			ConsecutiveFailures: state.ConsecutiveFailures,
			LastDecision:        evaluation.Decision,
			CandidateID:         evaluation.CandidateID,
		}
		if err := deps.Jobs.RecordProgress(ctx, state.Job, progress); err != nil {
			return nil, fmt.Errorf("record campaign progress: %w", err)
		}
		return stateEvent(ctx, state), nil
	}, deterministicCfg)

	route := workflow.NewFunctionNode("route_campaign", func(ctx adkagent.Context, state CampaignState) (*session.Event, error) {
		ev := stateEvent(ctx, state)
		switch {
		case state.CandidatesTried >= cfg.MaxCandidates:
			state.StopReason = "maximum candidate count reached"
			ev = stateEvent(ctx, state)
			ev.Routes = []string{routeFinish}
		case state.ConsecutiveFailures >= cfg.MaxConsecutiveFailures:
			state.StopReason = "consecutive rejection/inconclusive limit reached"
			ev = stateEvent(ctx, state)
			ev.Routes = []string{routeFinish}
		default:
			ev.Routes = []string{routeContinue}
		}
		return ev, nil
	}, deterministicCfg)

	finalize := workflow.NewFunctionNode("finalize_campaign", func(ctx adkagent.Context, state CampaignState) (CampaignResult, error) {
		result := CampaignResult{
			CampaignID:         state.Request.CampaignID,
			Job:                state.Job,
			CandidatesTried:    state.CandidatesTried,
			AcceptedCandidates: slices.Clone(state.AcceptedCandidates),
			FinalEvaluation:    state.Evaluation,
			StopReason:         state.StopReason,
		}
		job, err := deps.Jobs.CompleteCampaign(ctx, state.Job, result)
		if err != nil {
			return CampaignResult{}, fmt.Errorf("complete campaign job: %w", err)
		}
		result.Job = job
		return result, nil
	}, deterministicCfg)

	edges := workflow.NewEdgeBuilder().
		Add(workflow.Start, initialize).
		Add(initialize, inspect).
		Add(inspect, coordinator).
		Add(coordinator, mergeCoordinator).
		Add(mergeCoordinator, explorer).
		Add(explorer, discover).
		Add(discover, analyst).
		Add(analyst, mergeAnalysis).
		Add(mergeAnalysis, optimizer).
		Add(optimizer, evaluate).
		Add(evaluate, reviewer).
		Add(reviewer, decide).
		Add(decide, route).
		AddRoute(route, coordinator, workflow.StringRoute(routeContinue)).
		AddRoute(route, finalize, workflow.StringRoute(routeFinish)).
		Build()

	wf, err := workflow.New(cfg.WorkflowName, edges, workflow.WithMaxConcurrency(cfg.MaxConcurrency))
	if err != nil {
		return nil, fmt.Errorf("build campaign workflow: %w", err)
	}
	root, err := adkagent.New(adkagent.Config{
		Name:        "go_agent_optimizer",
		Description: "Runs a bounded, evidence-driven Go CLI optimization campaign.",
		SubAgents:   deps.Agents.All(),
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return wf.Run(ctx)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build root agent: %w", err)
	}

	return &Orchestrator{Workflow: wf, Agent: root, Config: cfg}, nil
}

func validateDependencies(deps Dependencies) error {
	if deps.Runner == nil {
		return fmt.Errorf("runner service is required")
	}
	if deps.Policy == nil {
		return fmt.Errorf("policy service is required")
	}
	if deps.Jobs == nil {
		return fmt.Errorf("job service is required")
	}
	for i, a := range deps.Agents.All() {
		if a == nil {
			return fmt.Errorf("%s agent is required", agents.AllRoles[i])
		}
	}
	return nil
}

func normalizeRequest(req CampaignRequest) (CampaignRequest, error) {
	if req.CampaignID == "" {
		return CampaignRequest{}, fmt.Errorf("campaign ID is required")
	}
	if req.Repository == "" {
		return CampaignRequest{}, fmt.Errorf("repository is required")
	}
	if req.BuildTarget == "" {
		return CampaignRequest{}, fmt.Errorf("build target is required")
	}
	if req.OptimizationMode == "" {
		req.OptimizationMode = domain.PolicyIdiomatic
	}
	switch req.OptimizationMode {
	case domain.PolicyIdiomatic, domain.PolicySpecialized, domain.PolicyNative:
		return req, nil
	default:
		return CampaignRequest{}, fmt.Errorf("unknown optimization mode %q", req.OptimizationMode)
	}
}

func decodeRequest(input any) (CampaignRequest, error) {
	if req, ok := input.(CampaignRequest); ok {
		return req, nil
	}
	data, err := json.Marshal(input)
	if err != nil {
		return CampaignRequest{}, fmt.Errorf("encode campaign request %T: %w", input, err)
	}
	if text, ok := input.(string); ok {
		data = []byte(text)
	}
	var req CampaignRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return CampaignRequest{}, fmt.Errorf("decode campaign request: %w", err)
	}
	return req, nil
}

func stateEvent(ctx adkagent.Context, state CampaignState) *session.Event {
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Output = state
	ev.Actions.StateDelta[stateKey] = state
	return ev
}

func loadState(ctx adkagent.Context) (CampaignState, error) {
	if ctx.Session() == nil || ctx.Session().State() == nil {
		return CampaignState{}, fmt.Errorf("campaign state unavailable: session state is nil")
	}
	raw, err := ctx.Session().State().Get(stateKey)
	if err != nil {
		if errors.Is(err, session.ErrStateKeyNotExist) {
			return CampaignState{}, fmt.Errorf("campaign state unavailable: %w", err)
		}
		return CampaignState{}, fmt.Errorf("read campaign state: %w", err)
	}
	switch value := raw.(type) {
	case CampaignState:
		return value, nil
	case *CampaignState:
		if value == nil {
			return CampaignState{}, fmt.Errorf("campaign state unavailable: nil value")
		}
		return *value, nil
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return CampaignState{}, fmt.Errorf("encode campaign state %T: %w", raw, err)
	}
	var state CampaignState
	if err := json.Unmarshal(data, &state); err != nil {
		return CampaignState{}, fmt.Errorf("decode campaign state %T: %w", raw, err)
	}
	return state, nil
}

func validDecision(decision domain.Decision) bool {
	switch decision {
	case domain.DecisionAccepted, domain.DecisionRejected, domain.DecisionInconclusive:
		return true
	default:
		return false
	}
}
