package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
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

	stopReasonMaxCandidates       = "maximum candidate count reached"
	stopReasonConsecutiveFailures = "consecutive rejection/inconclusive limit reached"
	// stopReasonConsecutiveInconclusive is reported only when the campaign
	// configures stop_after_inconclusive, which bounds unresolved verdicts
	// separately from failures.
	stopReasonConsecutiveInconclusive = "consecutive inconclusive limit reached"
	// stopReasonProviderFailure is followed by the last failure of the cycle
	// that tripped it.
	stopReasonProviderFailure = "model provider unavailable: every model role failed in the same cycle; last failure: "
)

// ErrProviderUnavailable marks a campaign the graph stopped because every model
// role failed in one cycle. A caller that sees CampaignResult.ProviderFailure
// wraps it, so the campaign ends as a failure rather than as completed.
var ErrProviderUnavailable = errors.New("model provider unavailable")

// Dependencies are intentionally narrow so deterministic execution, policy,
// and job persistence can be local, remote, or fake implementations.
type Dependencies struct {
	Runner RunnerService
	Policy PolicyService
	Jobs   JobService
	Agents agents.Set
	// Causes, when set, replaces the analyst agent with deterministic cause
	// classification. Agents.Analyst is then built but never run.
	Causes CauseAnalyst
	// Review, when set, replaces the reviewer agent with deterministic
	// behaviour-hazard checks. Agents.Reviewer is then built but never run.
	Review ReviewAnalyst
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
	if err := n.setAgents(g.deps.Agents, agt, g.deps.Jobs); err != nil {
		return n, err
	}
	if g.deps.Causes != nil {
		// The node keeps the role's name, so a degraded classification is
		// reported against "analyst" exactly as a failed model call would be,
		// and falls back to discovery's hot paths the same way.
		n.analyst = degradeNode(workflow.NewFunctionNode(string(agents.RoleAnalyst), g.analyzeCauses, agt), string(agents.RoleAnalyst), g.deps.Jobs)
		// With causes ranked, code chooses each cycle's target after the
		// analysis (planTarget), so the coordinator model has nothing to
		// decide: on a live gron campaign it took up to 2m40s a cycle and its
		// free-text plan steered the optimizer away from the top target.
		n.coordinator = workflow.NewFunctionNode(string(agents.RoleCoordinator), planCoordinator, det)
	}
	if g.deps.Review != nil {
		n.reviewer = degradeNode(workflow.NewFunctionNode(string(agents.RoleReviewer), g.reviewPatch, agt), string(agents.RoleReviewer), g.deps.Jobs)
	}
	return n, nil
}

func (g *campaignGraph) reviewPatch(ctx adkagent.Context, state CampaignState) (agents.ReviewerResult, error) {
	return g.deps.Review.ReviewPatch(ctx, ReviewRequest{Campaign: state.Request, Target: state.Target, Proposal: state.Proposal, Candidate: state.Candidate})
}

// planCoordinator states the plan the coordinator model used to write. The
// concrete experiment is filled in by planTarget once the analysis exists.
func planCoordinator(_ adkagent.Context, _ CampaignState) (agents.CoordinatorResult, error) {
	return agents.CoordinatorResult{
		Objective:      targetObjective,
		NextExperiment: "chosen by code after cause analysis",
	}, nil
}

const targetObjective = "patch the highest-ranked cause the analysis flagged that no earlier candidate has tried"

func (g *campaignGraph) analyzeCauses(ctx adkagent.Context, state CampaignState) (agents.AnalystResult, error) {
	return g.deps.Causes.AnalyzeCauses(ctx, CauseRequest{Campaign: state.Request, Discovery: state.Discovery})
}

func (n *graphNodes) setAgents(roleSet agents.Set, agt workflow.NodeConfig, jobs JobService) error {
	var err error
	if n.coordinator, err = agentNode(roleSet.Coordinator, agt, "coordinator", jobs); err != nil {
		return err
	}
	if n.explorer, err = agentNode(roleSet.Explorer, agt, "explorer", jobs); err != nil {
		return err
	}
	if n.analyst, err = agentNode(roleSet.Analyst, agt, "analyst", jobs); err != nil {
		return err
	}
	if n.optimizer, err = agentNode(roleSet.Optimizer, agt, "optimizer", jobs); err != nil {
		return err
	}
	if n.reviewer, err = agentNode(roleSet.Reviewer, agt, "reviewer", jobs); err != nil {
		return err
	}
	return nil
}

func agentNode(a adkagent.Agent, cfg workflow.NodeConfig, role string, jobs JobService) (workflow.Node, error) {
	n, err := workflow.NewAgentNode(a, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s node: %w", role, err)
	}
	return degradeNode(n, role, jobs), nil
}

// emptyRoleResult is the degraded output every role decodes into a zero value.
const emptyRoleResult = "{}"

// degradingNode stops one role's model-boundary failure from ending the
// campaign.
//
// A workflow node error ends the whole ADK run, and every agent node here is
// one model call away from an error: a provider that stalls past the retry
// ladder ended a campaign before it evaluated a single candidate. Each role
// already has a deterministic fallback for an absent answer — the manifest's
// seed workloads, discoveryHotPaths for an empty analysis, and a candidate the
// evaluator rejects for an empty patch — so an empty JSON object is the
// correct degraded output. It decodes to a zero-value result, the failure is
// reported, and the authoritative decision stays with the deterministic half.
type degradingNode struct {
	workflow.Node
	role string
	jobs JobService
}

func degradeNode(inner workflow.Node, role string, jobs JobService) workflow.Node {
	return degradingNode{Node: inner, role: role, jobs: jobs}
}

func (n degradingNode) Run(ctx adkagent.Context, input any) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		delivered := false
		var failure error
		for event, err := range n.Node.Run(ctx, input) {
			if err != nil {
				failure = err
				n.reportDegraded(ctx, err)
				break
			}
			if event != nil && event.Output != nil {
				delivered = true
			}
			if !yield(event, nil) {
				return
			}
		}
		if delivered {
			return
		}
		yield(n.emptyResult(ctx, failure), nil)
	}
}

// emptyResult is the degraded output, carrying the failure that caused it into
// the cycle's CycleFailures.
//
// Absorbing every failure had a cost once the provider itself went down or the
// key was revoked: every role degraded on every cycle, each after up to two
// minutes of retries, the optimizer's empty patch was rejected as if it were a
// bad patch, and the campaign ended "completed" at the consecutive-rejection
// bound, blaming the patches for the provider. The route node can only stop
// that if it sees which roles failed this cycle, and a node's output is all the
// next node receives, so the record travels in the campaign state. A role that
// delivered an answer before failing is not counted: the provider answered.
func (n degradingNode) emptyResult(ctx adkagent.Context, failure error) *session.Event {
	event := session.NewEvent(ctx, ctx.InvocationID())
	event.Output = emptyRoleResult
	if failure == nil {
		return event
	}
	if state, err := loadState(ctx); err == nil {
		state.CycleFailures = append(state.CycleFailures, RoleFailure{Role: n.role, Cause: failure.Error()})
		event.Actions.StateDelta[stateKey] = state
	}
	return event
}

// reportDegraded records the cause of an absorbed role failure. The node
// continues with an empty result either way, so an operator reading the report
// learns why a candidate looks empty rather than merely that it does. A wrapper
// built without a job service simply absorbs the failure.
func (n degradingNode) reportDegraded(ctx adkagent.Context, cause error) {
	if n.jobs == nil {
		return
	}
	_ = n.jobs.RecordRoleDegraded(ctx, n.role, cause)
}

func (n graphNodes) edges() []workflow.Edge {
	return workflow.NewEdgeBuilder().
		Add(workflow.Start, n.initialize).
		AddRoute(n.initialize, n.inspect, workflow.StringRoute(routeContinue)).
		AddRoute(n.initialize, n.finalize, workflow.StringRoute(routeFinish)).
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
		return nil, errors.New("start campaign job: empty job ID")
	}
	// Seeding the tally rather than starting at zero is what makes
	// MaxConsecutiveFailures a campaign bound instead of a per-process one.
	state := CampaignState{Request: req, Job: job, StartedAt: time.Now(), ConsecutiveFailures: req.PriorConsecutiveFailures, ConsecutiveInconclusive: req.PriorConsecutiveInconclusive}
	// The route node only runs after a decision, so a campaign that resumes
	// already at its failure bound would spend one more candidate proving what
	// the carried-in tally already says. Finishing from here keeps the bound
	// exact instead of off by the resumed process's first candidate.
	next := routeContinue
	if reason, hit := g.consecutiveBound(state); hit {
		state.StopReason = reason
		next = routeFinish
	}
	ev := stateEvent(ctx, state)
	ev.Routes = []string{next}
	return ev, nil
}

func (g *campaignGraph) inspect(ctx adkagent.Context, state CampaignState) (*session.Event, error) {
	inventory, err := g.deps.Runner.Inspect(ctx, state.Request)
	if err != nil {
		return nil, fmt.Errorf("inspect repository: %w", err)
	}
	state.Inspection = inventory
	return stateEvent(ctx, state), nil
}

// decodeRole decodes one role's output and records the repair it needed, if
// any. The repaired value is still the role's answer; the record exists so an
// answer the decoder salvaged is not mistaken for the one the model sent.
func decodeRole[T any](ctx context.Context, jobs JobService, role agents.Role, raw any) (T, agents.Repair, error) {
	result, repair, err := agents.DecodeResultWithRepair[T](raw)
	if err == nil {
		recordRepair(ctx, jobs, role, repair)
	}
	return result, repair, err
}

// recordRepair reports a repaired role output. Like a degraded role, a failed
// record is not a reason to stop the campaign.
func recordRepair(ctx context.Context, jobs JobService, role agents.Role, repair agents.Repair) {
	if repair == "" {
		return
	}
	_ = jobs.RecordRoleRepaired(ctx, string(role), repair)
}

func (g *campaignGraph) mergeCoordinator(ctx adkagent.Context, raw any) (*session.Event, error) {
	result, _, err := decodeRole[agents.CoordinatorResult](ctx, g.deps.Jobs, agents.RoleCoordinator, raw)
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
	result, _, err := decodeRole[agents.ExplorerResult](ctx, g.deps.Jobs, agents.RoleExplorer, raw)
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
	result, repair, err := salvageAnalystResult(raw)
	if err != nil {
		return nil, fmt.Errorf("analyst output: %w", err)
	}
	recordRepair(ctx, g.deps.Jobs, agents.RoleAnalyst, repair)
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.Analysis = result
	attachExcerpts(ctx, g.deps.Runner, &state, result)
	planTarget(&state)
	return stateEvent(ctx, state), nil
}

// planTarget lets code choose what the optimizer attacks whenever the analysis
// ranks causes: the first target no earlier candidate tried. The optimizer then
// sees only that function's source, and its instruction forbids patching any
// other. Left to choose from a ranked list, the optimizer on a live gron
// campaign ignored the top target, the output loop whose bufio fix was once
// accepted at -11.8%, and micro-optimized validIdentifier instead. A model
// analyst ranks nothing, so this leaves its path exactly as it was.
func planTarget(state *CampaignState) {
	state.Target = nil
	tried := triedTargets(*state)
	target, ok := nextTarget(state.Analysis.Targets, tried)
	if !ok {
		return
	}
	state.Target = &target
	state.Coordinator = agents.CoordinatorResult{
		Objective:      targetObjective,
		NextExperiment: target.Remedy,
		Rationale:      []string{fmt.Sprintf("the analysis flagged %s in %s at %+.1f sd; %d target(s) already tried", target.Cause, target.Function, target.Z, len(tried))},
	}
	if excerpts := excerptsAt(state.SourceExcerpts, target.Location); len(excerpts) > 0 {
		state.SourceExcerpts = excerpts
	}
}

func triedTargets(state CampaignState) map[string]bool {
	tried := map[string]bool{}
	for _, t := range state.Request.PriorTargets {
		tried[targetKey(t)] = true
	}
	for _, prior := range state.PriorCandidates {
		if prior.Target != nil {
			tried[targetKey(*prior.Target)] = true
		}
	}
	return tried
}

func targetKey(t agents.Target) string { return t.Location + "\x00" + t.Cause }

func nextTarget(targets []agents.Target, tried map[string]bool) (agents.Target, bool) {
	for _, t := range targets {
		if !tried[targetKey(t)] {
			return t, true
		}
	}
	return agents.Target{}, false
}

func excerptsAt(excerpts []SourceExcerpt, location string) []SourceExcerpt {
	var kept []SourceExcerpt
	for _, e := range excerpts {
		if e.HotPath == location {
			kept = append(kept, e)
		}
	}
	return kept
}

// salvageAnalystResult decodes the analyst's output, falling back to an
// analysis carried in the input. The repair it reports belongs to the value it
// returns: the carried-in analysis is already decoded and never repaired.
func salvageAnalystResult(raw any) (agents.AnalystResult, agents.Repair, error) {
	result, repair, err := agents.DecodeResultWithRepair[agents.AnalystResult](raw)
	if err != nil || len(result.HotPaths) == 0 {
		if prior, ok := priorAnalystResult(raw); ok && len(prior.HotPaths) > len(result.HotPaths) {
			return prior, "", nil
		}
	}
	return result, repair, err
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
	proposal, repair, err := decodeRole[agents.OptimizerResult](ctx, g.deps.Jobs, agents.RoleOptimizer, raw)
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
		return nil, errors.New("evaluate candidate: empty candidate ID")
	}
	// Stamped after the runner returns so no step of the evaluation can see
	// it: the note rides to the candidate's record and nowhere else.
	evidence.ProposalRepair = repair
	state.Candidate = evidence
	return stateEvent(ctx, state), nil
}

func (g *campaignGraph) applyPolicy(ctx adkagent.Context, raw any) (*session.Event, error) {
	review, _, err := decodeRole[agents.ReviewerResult](ctx, g.deps.Jobs, agents.RoleReviewer, raw)
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
		Target:   state.Target,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate policy: %w", err)
	}
	evaluation, err = bindEvaluation(state, evaluation)
	if err != nil {
		return nil, err
	}
	if err := applyDecision(ctx, g.deps.Runner, &state, evaluation, g.cfg.MaxConsecutiveInconclusive > 0); err != nil {
		return nil, err
	}
	progress := CampaignProgress{
		CandidatesTried:         state.CandidatesTried,
		ConsecutiveFailures:     state.ConsecutiveFailures,
		ConsecutiveInconclusive: state.ConsecutiveInconclusive,
		LastDecision:            evaluation.Decision,
		CandidateID:             evaluation.CandidateID,
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

// consecutiveBound reports which consecutive-verdict bound a campaign has
// reached, if any. A campaign that configured stop_after_inconclusive tracks
// unresolved verdicts on their own; otherwise they extend the failure streak,
// which is the historical behavior.
func (g *campaignGraph) consecutiveBound(state CampaignState) (string, bool) {
	if g.cfg.MaxConsecutiveInconclusive > 0 && state.ConsecutiveInconclusive >= g.cfg.MaxConsecutiveInconclusive {
		return stopReasonConsecutiveInconclusive, true
	}
	if state.ConsecutiveFailures >= g.cfg.MaxConsecutiveFailures {
		return stopReasonConsecutiveFailures, true
	}
	return "", false
}

func applyDecision(ctx adkagent.Context, runner RunnerService, state *CampaignState, evaluation domain.Evaluation, separateInconclusiveBound bool) error {
	state.Evaluation = evaluation
	state.CandidatesTried++
	state.PriorCandidates = append(state.PriorCandidates, PriorCandidate{
		Attempt:        state.CandidatesTried,
		Hypothesis:     state.Proposal.Hypothesis,
		Decision:       string(evaluation.Decision),
		Reasons:        evaluation.Reasons,
		FailureDetail:  state.Candidate.FailureDetail,
		Target:         state.Target,
		ReviewConcerns: state.Review.Concerns,
	})
	countDecision(state, evaluation.Decision, separateInconclusiveBound)
	if evaluation.Decision != domain.DecisionAccepted {
		return nil
	}
	if err := runner.PromoteCandidate(ctx, state.Candidate.Candidate); err != nil {
		return fmt.Errorf("promote candidate: %w", err)
	}
	state.AcceptedCandidates = append(state.AcceptedCandidates, evaluation.CandidateID)
	return nil
}

// countDecision keeps the two streaks. An accepted candidate clears both. A
// rejection counts as a failure and breaks any run of unresolved verdicts. An
// inconclusive verdict counts on its own streak when the campaign configured
// one, and otherwise counts as a failure, which is the default behavior.
func countDecision(state *CampaignState, decision domain.Decision, separateInconclusiveBound bool) {
	switch decision {
	case domain.DecisionAccepted:
		state.ConsecutiveFailures, state.ConsecutiveInconclusive = 0, 0
	case domain.DecisionRejected:
		state.ConsecutiveFailures++
		state.ConsecutiveInconclusive = 0
	case domain.DecisionInconclusive:
		state.ConsecutiveInconclusive++
		if !separateInconclusiveBound {
			state.ConsecutiveFailures++
		}
	}
}

// route finishes the campaign or starts the next cycle. A cycle in which every
// model role failed is checked before any bound: its candidate was rejected for
// an empty patch no model wrote, so naming the candidate budget or the
// rejection streak would blame the patches for the provider.
func (g *campaignGraph) route(ctx adkagent.Context, state CampaignState) (*session.Event, error) {
	next := routeFinish
	if failure, down := g.providerFailure(state); down {
		state.ProviderFailure = failure
		state.StopReason = stopReasonProviderFailure + failure
	} else if reason := g.stopReason(state); reason != "" {
		state.StopReason = reason
	} else {
		// Each cycle is judged on its own failures, so a role that failed
		// once and recovered cannot add up to an outage across cycles.
		state.CycleFailures = nil
		next = routeContinue
	}
	ev := stateEvent(ctx, state)
	ev.Routes = []string{next}
	return ev, nil
}

// modelRoles are the roles whose answers come from the model provider, which
// today is every role. A role served by anything else must be left out: its
// failures say nothing about the provider the others share, and counting it
// would keep a campaign running whose other roles all failed while it kept
// answering.
func (*campaignGraph) modelRoles() []string {
	roles := make([]string, 0, len(agents.AllRoles))
	for _, role := range agents.AllRoles {
		roles = append(roles, string(role))
	}
	return roles
}

// providerFailure reports the cycle's last failure when every model role failed
// in it. One role failing, or several, is the transient case the degrading
// wrapper exists for; every role failing in the same cycle is a provider that
// is down or a key that was revoked, and another cycle would only spend the
// same retries to reach the same empty patch.
func (g *campaignGraph) providerFailure(state CampaignState) (string, bool) {
	if len(state.CycleFailures) == 0 {
		return "", false
	}
	failed := make(map[string]bool, len(state.CycleFailures))
	for _, f := range state.CycleFailures {
		failed[f.Role] = true
	}
	for _, role := range g.modelRoles() {
		if !failed[role] {
			return "", false
		}
	}
	last := state.CycleFailures[len(state.CycleFailures)-1]
	return last.Role + ": " + last.Cause, true
}

// stopReason reports why the campaign should finish instead of trying another
// candidate, or an empty string to continue. The candidate budget is checked
// first so a campaign that used its last patch reports that rather than a
// streak bound it also happens to meet.
func (g *campaignGraph) stopReason(state CampaignState) string {
	if state.CandidatesTried >= g.cfg.MaxCandidates {
		return stopReasonMaxCandidates
	}
	if reason, hit := g.consecutiveBound(state); hit {
		return reason
	}
	return ""
}

func (g *campaignGraph) finalize(ctx adkagent.Context, state CampaignState) (CampaignResult, error) {
	result := CampaignResult{
		CampaignID:         state.Request.CampaignID,
		Job:                state.Job,
		CandidatesTried:    state.CandidatesTried,
		AcceptedCandidates: slices.Clone(state.AcceptedCandidates),
		FinalEvaluation:    state.Evaluation,
		StopReason:         state.StopReason,
		ProviderFailure:    state.ProviderFailure,
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
		return errors.New("runner service is required")
	}
	if deps.Policy == nil {
		return errors.New("policy service is required")
	}
	if deps.Jobs == nil {
		return errors.New("job service is required")
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
		return CampaignRequest{}, errors.New("campaign ID is required")
	}
	if req.Repository == "" {
		return CampaignRequest{}, errors.New("repository is required")
	}
	if req.BuildTarget == "" {
		return CampaignRequest{}, errors.New("build target is required")
	}
	// A negative carried-in tally would buy the resumed process extra
	// failures before MaxConsecutiveFailures binds, so it is rejected rather
	// than clamped: the bound is not negotiable by request content.
	if req.PriorConsecutiveFailures < 0 {
		return CampaignRequest{}, errors.New("prior consecutive failures cannot be negative")
	}
	if req.PriorConsecutiveInconclusive < 0 {
		return CampaignRequest{}, errors.New("prior consecutive inconclusive cannot be negative")
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
	return loadSessionState(ctx.Session())
}

// loadSessionState reads the persisted campaign state out of the session.
// The nil and read-error branches live here rather than in loadState so each
// can be exercised directly against a session double.
func loadSessionState(sess session.Session) (CampaignState, error) {
	if sess == nil || sess.State() == nil {
		return CampaignState{}, errors.New("campaign state unavailable: session state is nil")
	}
	raw, err := sess.State().Get(stateKey)
	if err != nil {
		if errors.Is(err, session.ErrStateKeyNotExist) {
			return CampaignState{}, fmt.Errorf("campaign state unavailable: %w", err)
		}
		return CampaignState{}, fmt.Errorf("read campaign state: %w", err)
	}
	return decodeCampaignState(raw)
}

// decodeCampaignState normalizes whatever the session store returned into a
// CampaignState, accepting the typed values the graph writes and the JSON
// shape a store may have persisted.
func decodeCampaignState(raw any) (CampaignState, error) {
	switch value := raw.(type) {
	case CampaignState:
		return value, nil
	case *CampaignState:
		if value == nil {
			return CampaignState{}, errors.New("campaign state unavailable: nil value")
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
