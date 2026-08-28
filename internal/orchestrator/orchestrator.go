package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"slices"
	"strings"
	"time"

	"example.com/gotorque/internal/agents"
	"example.com/gotorque/internal/domain"
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

type campaignGraph struct {
	deps Dependencies
	cfg  Config
}

type graphNodes struct {
	initialize, inspect, coordinator, mergeCoordinator workflow.Node
	explorer, discover, analyst, mergeAnalysis         workflow.Node
	optimizer, evaluate, reviewer, decide              workflow.Node
	route, finalize                                    workflow.Node
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
	return (&campaignGraph{deps: deps, cfg: cfg}).build()
}

func (g *campaignGraph) build() (*Orchestrator, error) {
	nodes, err := g.nodes()
	if err != nil {
		return nil, err
	}
	wf, err := workflow.New(g.cfg.WorkflowName, nodes.edges(), workflow.WithMaxConcurrency(g.cfg.MaxConcurrency))
	if err != nil {
		return nil, fmt.Errorf("build campaign workflow: %w", err)
	}
	root, err := adkagent.New(adkagent.Config{
		Name:        "go_agent_optimizer",
		Description: "Runs a bounded, evidence-driven Go CLI optimization campaign.",
		SubAgents:   g.deps.Agents.All(),
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return wf.Run(ctx)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build root agent: %w", err)
	}
	return &Orchestrator{Workflow: wf, Agent: root, Config: g.cfg}, nil
}

func (g *campaignGraph) nodes() (graphNodes, error) {
	det := workflow.NodeConfig{Timeout: g.cfg.DeterministicTimeout}
	agt := workflow.NodeConfig{Timeout: g.cfg.AgentTimeout}
	n := graphNodes{
		initialize:       workflow.NewFunctionNode("initialize_campaign", g.initialize, det),
		inspect:          workflow.NewFunctionNode("inspect_repository", g.inspect, det),
		mergeCoordinator: workflow.NewFunctionNode("merge_coordinator", g.mergeCoordinator, det),
		discover:         workflow.NewFunctionNode("run_discovery", g.discover, det),
		mergeAnalysis:    workflow.NewFunctionNode("merge_analysis", g.mergeAnalysis, det),
		evaluate:         workflow.NewFunctionNode("evaluate_candidate", g.evaluate, det),
		decide:           workflow.NewFunctionNode("apply_policy", g.applyPolicy, det),
		route:            workflow.NewFunctionNode("route_campaign", g.route, det),
		finalize:         workflow.NewFunctionNode("finalize_campaign", g.finalize, det),
	}
	return n, n.setAgents(g.deps.Agents, agt)
}

func (n *graphNodes) setAgents(roleSet agents.Set, agt workflow.NodeConfig) error {
	var err error
	if n.coordinator, err = agentNode(roleSet.Coordinator, agt, "coordinator"); err != nil {
		return err
	}
	if n.explorer, err = agentNode(roleSet.Explorer, agt, "explorer"); err != nil {
		return err
	}
	if n.analyst, err = agentNode(roleSet.Analyst, agt, "analyst"); err != nil {
		return err
	}
	if n.optimizer, err = agentNode(roleSet.Optimizer, agt, "optimizer"); err != nil {
		return err
	}
	if n.reviewer, err = agentNode(roleSet.Reviewer, agt, "reviewer"); err != nil {
		return err
	}
	return nil
}

func agentNode(a adkagent.Agent, cfg workflow.NodeConfig, role string) (workflow.Node, error) {
	n, err := workflow.NewAgentNode(a, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s node: %w", role, err)
	}
	return n, nil
}

func (n graphNodes) edges() []workflow.Edge {
	return workflow.NewEdgeBuilder().
		Add(workflow.Start, n.initialize).
		Add(n.initialize, n.inspect).
		Add(n.inspect, n.coordinator).
		Add(n.coordinator, n.mergeCoordinator).
		Add(n.mergeCoordinator, n.explorer).
		Add(n.explorer, n.discover).
		Add(n.discover, n.analyst).
		Add(n.analyst, n.mergeAnalysis).
		Add(n.mergeAnalysis, n.optimizer).
		Add(n.optimizer, n.evaluate).
		Add(n.evaluate, n.reviewer).
		Add(n.reviewer, n.decide).
		Add(n.decide, n.route).
		AddRoute(n.route, n.coordinator, workflow.StringRoute(routeContinue)).
		AddRoute(n.route, n.finalize, workflow.StringRoute(routeFinish)).
		Build()
}

func (g *campaignGraph) initialize(ctx adkagent.Context, input any) (*session.Event, error) {
	req, err := decodeRequest(input)
	if err != nil {
		return nil, err
	}
	req, err = normalizeRequest(req)
	if err != nil {
		return nil, err
	}
	job, err := g.deps.Jobs.StartCampaign(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("start campaign job: %w", err)
	}
	if job.ID == "" {
		return nil, fmt.Errorf("start campaign job: empty job ID")
	}
	state := CampaignState{Request: req, Job: job, StartedAt: time.Now()}
	return stateEvent(ctx, state), nil
}

func (g *campaignGraph) inspect(ctx adkagent.Context, state CampaignState) (*session.Event, error) {
	inventory, err := g.deps.Runner.Inspect(ctx, state.Request)
	if err != nil {
		return nil, fmt.Errorf("inspect repository: %w", err)
	}
	state.Inspection = inventory
	return stateEvent(ctx, state), nil
}

func (g *campaignGraph) mergeCoordinator(ctx adkagent.Context, raw any) (*session.Event, error) {
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
}

func (g *campaignGraph) discover(ctx adkagent.Context, raw any) (*session.Event, error) {
	result, err := agents.DecodeResult[agents.ExplorerResult](raw)
	if err != nil {
		return nil, fmt.Errorf("explorer output: %w", err)
	}
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.Explorer = result
	evidence, err := g.deps.Runner.Discover(ctx, DiscoveryRequest{
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
}

func (g *campaignGraph) mergeAnalysis(ctx adkagent.Context, raw any) (*session.Event, error) {
	debugAnalystRaw(raw)
	result, err := salvageAnalystResult(raw)
	if err != nil {
		return nil, fmt.Errorf("analyst output: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[analyst_raw_debug] decoded hot_paths=%d\n", len(result.HotPaths))
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.Analysis = result
	attachExcerpts(ctx, g.deps.Runner, &state, result)
	return stateEvent(ctx, state), nil
}

func debugAnalystRaw(raw any) {
	debugText, ok := raw.(string)
	if !ok {
		fmt.Fprintf(os.Stderr, "[analyst_raw_debug] non-string raw %T\n", raw)
		return
	}
	head := debugText
	if len(head) > 200 {
		head = head[:200]
	}
	tail := ""
	if len(debugText) > 200 {
		tail = debugText[len(debugText)-200:]
	}
	fmt.Fprintf(os.Stderr, "[analyst_raw_debug] len=%d head=%q tail=%q\n", len(debugText), head, tail)
}

func salvageAnalystResult(raw any) (agents.AnalystResult, error) {
	result, err := agents.DecodeResult[agents.AnalystResult](raw)
	if err != nil || len(result.HotPaths) == 0 {
		if prior, ok := priorAnalystResult(raw); ok && len(prior.HotPaths) > len(result.HotPaths) {
			return prior, nil
		}
	}
	return result, err
}

func priorAnalystResult(raw any) (agents.AnalystResult, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return agents.AnalystResult{}, false
	}
	analysisRaw, ok := m["analysis"]
	if !ok {
		return agents.AnalystResult{}, false
	}
	prior, err := agents.DecodeResult[agents.AnalystResult](analysisRaw)
	if err != nil {
		return agents.AnalystResult{}, false
	}
	return prior, true
}

func attachExcerpts(ctx adkagent.Context, runner RunnerService, state *CampaignState, result agents.AnalystResult) {
	collector, ok := runner.(ExcerptCollector)
	if !ok {
		return
	}
	analysis := result
	if len(analysis.HotPaths) == 0 {
		analysis.HotPaths = discoveryHotPaths(state.Discovery.HotFunctions)
	}
	if excerpts, err := collector.CollectExcerpts(ctx, analysis); err == nil {
		state.SourceExcerpts = excerpts
	}
}

func discoveryHotPaths(fns []string) []agents.HotPath {
	var targets []agents.HotPath
	for _, fn := range fns {
		if strings.Contains(fn, ":") && !strings.HasPrefix(fn, "runtime") {
			targets = append(targets, agents.HotPath{Location: fn})
		}
	}
	return targets
}

func (g *campaignGraph) evaluate(ctx adkagent.Context, raw any) (*session.Event, error) {
	proposal, err := agents.DecodeResult[agents.OptimizerResult](raw)
	if err != nil {
		return nil, fmt.Errorf("optimizer output: %w", err)
	}
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.Proposal = proposal
	evidence, err := g.deps.Runner.EvaluateCandidate(ctx, CandidateRequest{
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
}

func (g *campaignGraph) applyPolicy(ctx adkagent.Context, raw any) (*session.Event, error) {
	review, err := agents.DecodeResult[agents.ReviewerResult](raw)
	if err != nil {
		return nil, fmt.Errorf("reviewer output: %w", err)
	}
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.Review = review
	evaluation, err := g.deps.Policy.Evaluate(ctx, PolicyInput{
		Campaign: state.Request,
		Evidence: state.Candidate,
		Review:   review,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate policy: %w", err)
	}
	evaluation, err = bindEvaluation(state, evaluation)
	if err != nil {
		return nil, err
	}
	if err := applyDecision(ctx, g.deps.Runner, &state, evaluation); err != nil {
		return nil, err
	}
	progress := CampaignProgress{
		CandidatesTried:     state.CandidatesTried,
		ConsecutiveFailures: state.ConsecutiveFailures,
		LastDecision:        evaluation.Decision,
		CandidateID:         evaluation.CandidateID,
	}
	if err := g.deps.Jobs.RecordProgress(ctx, state.Job, progress); err != nil {
		return nil, fmt.Errorf("record campaign progress: %w", err)
	}
	return stateEvent(ctx, state), nil
}

func bindEvaluation(state CampaignState, evaluation domain.Evaluation) (domain.Evaluation, error) {
	if !validDecision(evaluation.Decision) {
		return evaluation, fmt.Errorf("evaluate policy: invalid decision %q", evaluation.Decision)
	}
	if evaluation.CandidateID == "" {
		evaluation.CandidateID = state.Candidate.Candidate.ID
	}
	if evaluation.CandidateID != state.Candidate.Candidate.ID {
		return evaluation, fmt.Errorf("evaluate policy: candidate ID %q does not match %q", evaluation.CandidateID, state.Candidate.Candidate.ID)
	}
	return evaluation, nil
}

func applyDecision(ctx adkagent.Context, runner RunnerService, state *CampaignState, evaluation domain.Evaluation) error {
	state.Evaluation = evaluation
	state.CandidatesTried++
	state.PriorCandidates = append(state.PriorCandidates, PriorCandidate{
		Attempt:       state.CandidatesTried,
		Hypothesis:    state.Proposal.Hypothesis,
		Decision:      string(evaluation.Decision),
		Reasons:       evaluation.Reasons,
		FailureDetail: state.Candidate.FailureDetail,
	})
	if evaluation.Decision != domain.DecisionAccepted {
		state.ConsecutiveFailures++
		return nil
	}
	if err := runner.PromoteCandidate(ctx, state.Candidate.Candidate); err != nil {
		return fmt.Errorf("promote candidate: %w", err)
	}
	state.ConsecutiveFailures = 0
	state.AcceptedCandidates = append(state.AcceptedCandidates, evaluation.CandidateID)
	return nil
}

func (g *campaignGraph) route(ctx adkagent.Context, state CampaignState) (*session.Event, error) {
	ev := stateEvent(ctx, state)
	switch {
	case state.CandidatesTried >= g.cfg.MaxCandidates:
		state.StopReason = "maximum candidate count reached"
		ev = stateEvent(ctx, state)
		ev.Routes = []string{routeFinish}
	case state.ConsecutiveFailures >= g.cfg.MaxConsecutiveFailures:
		state.StopReason = "consecutive rejection/inconclusive limit reached"
		ev = stateEvent(ctx, state)
		ev.Routes = []string{routeFinish}
	default:
		ev.Routes = []string{routeContinue}
	}
	return ev, nil
}

func (g *campaignGraph) finalize(ctx adkagent.Context, state CampaignState) (CampaignResult, error) {
	result := CampaignResult{
		CampaignID:         state.Request.CampaignID,
		Job:                state.Job,
		CandidatesTried:    state.CandidatesTried,
		AcceptedCandidates: slices.Clone(state.AcceptedCandidates),
		FinalEvaluation:    state.Evaluation,
		StopReason:         state.StopReason,
	}
	job, err := g.deps.Jobs.CompleteCampaign(ctx, state.Job, result)
	if err != nil {
		return CampaignResult{}, fmt.Errorf("complete campaign job: %w", err)
	}
	result.Job = job
	return result, nil
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
