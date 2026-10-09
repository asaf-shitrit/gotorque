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

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
)

const (
	stateKey      = "optimizer:campaign_state"
	routeContinue = "continue"
	routeFinish   = "finish"
	// cycleName names the subgraph that runs one candidate cycle.
	cycleName = "campaign_cycle"
)

// ErrProviderUnavailable marks a campaign the graph stopped because a role it
// depends on could not answer: the optimizer in consecutive cycles, or the
// analyst for every hot function (see ended). A caller that sees
// CampaignResult.ProviderFailure wraps it, so the campaign ends as a failure
// rather than as completed.
var ErrProviderUnavailable = errors.New("model provider unavailable")

// Dependencies are the campaign's collaborators. The deterministic half is one
// Bench, the engine in production and a fake in tests; the optimizer is a live
// model or a stub (agents.Set), and the two Jev advisors are live or stubbed.
type Dependencies struct {
	Bench  Bench
	Agents agents.Set
	// Causes serves the analyst node: Jev cause classification ranked in code.
	Causes CauseAnalyst
	// Review serves the reviewer node: Jev behaviour-hazard checks.
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

// graphNodes are the campaign graph's two levels: the outer nodes run once per
// graph entry or once per cycle, and the cycle's nodes run inside the
// campaign_cycle subgraph.
type graphNodes struct {
	initialize, discover, cycle, route, finalize workflow.Node
	cycleNodes
}

// cycleNodes are the nodes of one candidate cycle, from the analysis to the
// settled verdict. skip ends a cycle the analysis already stopped.
type cycleNodes struct {
	analyst, mergeAnalysis, optimizer, evaluate workflow.Node
	reviewer, decide, skip                      workflow.Node
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
		SubAgents:   []adkagent.Agent{g.deps.Agents.Optimizer},
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
	n := graphNodes{
		initialize: workflow.NewFunctionNode("initialize_campaign", g.initialize, det),
		discover:   workflow.NewFunctionNode("run_discovery", g.discover, det),
		route:      workflow.NewFunctionNode("route_campaign", g.route, det),
		finalize:   workflow.NewFunctionNode("finalize_campaign", g.finalize, det),
	}
	inner, err := g.cycleNodes()
	if err != nil {
		return n, err
	}
	n.cycleNodes = inner
	cycle, err := workflow.NewWorkflowNode(cycleName, inner.edges())
	if err != nil {
		return n, fmt.Errorf("build %s subgraph: %w", cycleName, err)
	}
	n.cycle = cycle
	return n, nil
}

func (g *campaignGraph) cycleNodes() (cycleNodes, error) {
	det := workflow.NodeConfig{Timeout: g.cfg.DeterministicTimeout}
	agt := workflow.NodeConfig{Timeout: g.cfg.AgentTimeout}
	n := cycleNodes{
		mergeAnalysis: workflow.NewFunctionNode("merge_analysis", g.mergeAnalysis, det),
		evaluate:      workflow.NewFunctionNode("evaluate_candidate", g.evaluate, det),
		decide:        workflow.NewFunctionNode("apply_policy", g.applyPolicy, det),
		skip:          workflow.NewFunctionNode("skip_candidate", g.skipCandidate, det),
	}
	optimizer, err := agentNode(g.deps.Agents.Optimizer, agt, string(agents.RoleOptimizer), g.deps.Bench)
	if err != nil {
		return n, err
	}
	n.optimizer = optimizer
	// The Jev nodes keep their role's name, so a degraded classification or
	// review is reported against "analyst" or "reviewer", and the analyst
	// falls back to discovery's hot paths.
	n.analyst = degradeNode(workflow.NewFunctionNode(string(agents.RoleAnalyst), g.analyzeCauses, agt), string(agents.RoleAnalyst), g.deps.Bench)
	n.reviewer = degradeNode(workflow.NewFunctionNode(string(agents.RoleReviewer), g.reviewPatch, agt), string(agents.RoleReviewer), g.deps.Bench)
	return n, nil
}

func (g *campaignGraph) reviewPatch(ctx adkagent.Context, state CampaignState) (agents.ReviewerResult, error) {
	return g.deps.Review.ReviewPatch(ctx, ReviewRequest{Campaign: state.Request, Target: state.Target, Proposal: state.Proposal, Candidate: state.Candidate})
}

func (g *campaignGraph) analyzeCauses(ctx adkagent.Context, state CampaignState) (agents.AnalystResult, error) {
	return g.deps.Causes.AnalyzeCauses(ctx, CauseRequest{Campaign: state.Request, Discovery: state.Discovery})
}

func agentNode(a adkagent.Agent, cfg workflow.NodeConfig, role string, bench Bench) (workflow.Node, error) {
	n, err := workflow.NewAgentNode(a, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s node: %w", role, err)
	}
	return degradeNode(n, role, bench), nil
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
	role  string
	bench Bench
}

func degradeNode(inner workflow.Node, role string, bench Bench) workflow.Node {
	return degradingNode{Node: inner, role: role, bench: bench}
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
// built without a bench simply absorbs the failure.
func (n degradingNode) reportDegraded(ctx adkagent.Context, cause error) {
	if n.bench == nil {
		return
	}
	_ = n.bench.Note(ctx, Note{Kind: NoteDegraded, Role: n.role, Cause: cause.Error()})
}

// edges is the outer graph: one discovery, then campaign_cycle until
// route_campaign finishes the campaign. The loop runs at this level, so the
// subgraph is one cycle and its output is the state route_campaign judges.
func (n graphNodes) edges() []workflow.Edge {
	return workflow.NewEdgeBuilder().
		Add(workflow.Start, n.initialize).
		AddRoute(n.initialize, n.discover, workflow.StringRoute(routeContinue)).
		AddRoute(n.initialize, n.finalize, workflow.StringRoute(routeFinish)).
		Add(n.discover, n.cycle).
		Add(n.cycle, n.route).
		AddRoute(n.route, n.cycle, workflow.StringRoute(routeContinue)).
		AddRoute(n.route, n.finalize, workflow.StringRoute(routeFinish)).
		Build()
}

// edges is one candidate cycle. A subgraph forwards the output of the
// terminal node that ran, so apply_policy and skip_candidate both end it with
// the campaign state, and only one of them runs in a cycle.
func (n cycleNodes) edges() []workflow.Edge {
	return workflow.NewEdgeBuilder().
		Add(workflow.Start, n.analyst).
		Add(n.analyst, n.mergeAnalysis).
		AddRoute(n.mergeAnalysis, n.optimizer, workflow.StringRoute(routeContinue)).
		AddRoute(n.mergeAnalysis, n.skip, workflow.StringRoute(routeFinish)).
		Add(n.optimizer, n.evaluate).
		Add(n.evaluate, n.reviewer).
		Add(n.reviewer, n.decide).
		Build()
}

// skipCandidate ends a cycle whose analysis stopped the campaign. It reads the
// session rather than its input, because merge_analysis hands the optimizer
// its brief as output whenever it planned a target.
func (g *campaignGraph) skipCandidate(ctx adkagent.Context, _ any) (*session.Event, error) {
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	return stateEvent(ctx, state), nil
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
	if err := g.deps.Bench.Note(ctx, Note{Kind: NoteStarted, Request: req}); err != nil {
		return nil, fmt.Errorf("note campaign start: %w", err)
	}
	// Deriving the tallies from the recorded candidates rather than starting
	// at zero is what makes MaxCandidates and the streak bounds campaign
	// bounds instead of per-process ones, and keeps attempt numbers unique.
	t := talliesOf(req.RecordedCandidates, g.cfg.MaxConsecutiveInconclusive > 0)
	state := CampaignState{
		Request: req, StartedAt: time.Now(),
		CandidatesTried: t.tried, ConsecutiveFailures: t.consecutiveFailures, ConsecutiveInconclusive: t.consecutiveInconclusive,
		PriorCandidates: slices.Clone(req.RecordedCandidates),
	}
	// The route node only runs after a decision, so a campaign that resumes
	// already at its failure bound would spend one more candidate proving what
	// the carried-in tally already says. Finishing from here keeps the bound
	// exact instead of off by the resumed process's first candidate.
	next := routeContinue
	if end, done := g.ended(state, atStart); done {
		state.StopReason = end.reason
		next = routeFinish
	}
	ev := stateEvent(ctx, state)
	ev.Routes = []string{next}
	return ev, nil
}

// decodeRole decodes one role's output and records the repair it needed, if
// any. The repaired value is still the role's answer; the record exists so an
// answer the decoder salvaged is not mistaken for the one the model sent.
func decodeRole[T any](ctx context.Context, bench Bench, role agents.Role, raw any) (T, agents.Repair, error) {
	result, repair, err := agents.DecodeResultWithRepair[T](raw)
	if err == nil {
		recordRepair(ctx, bench, role, repair)
	}
	return result, repair, err
}

// recordRepair reports a repaired role output. Like a degraded role, a failed
// record is not a reason to stop the campaign.
func recordRepair(ctx context.Context, bench Bench, role agents.Role, repair agents.Repair) {
	if repair == "" {
		return
	}
	_ = bench.Note(ctx, Note{Kind: NoteRepaired, Role: string(role), Repair: repair})
}

// discover runs once per graph entry, between initialize_campaign and the
// first analyst. Discovery's evidence is a property of the campaign, not of a
// cycle: the engine measured it before the graph started (baseline, profile,
// the workloads beyond the manifest's seeds that code and Jev chose in
// internal/campaign/explore.go), and nothing a cycle does changes it. The
// graph used to fetch it again at the head of every cycle, which returned the
// same value each time. The node stays a node of its own so a failed fetch is
// still reported as "run discovery" and the event path readers know is kept.
// Code chooses each cycle's target after the analysis (planTarget); a model
// coordinator and explorer once filled both jobs, and on a live gron campaign
// the coordinator took up to 2m40s a cycle and its free-text plan steered the
// optimizer away from the top target.
func (g *campaignGraph) discover(ctx adkagent.Context, _ any) (*session.Event, error) {
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := g.deps.Bench.Discovery(ctx)
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
	recordRepair(ctx, g.deps.Bench, agents.RoleAnalyst, repair)
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.Analysis = result
	attachExcerpts(ctx, g.deps.Bench, &state, result)
	planTarget(&state)
	next := routeContinue
	if end, done := g.ended(state, afterAnalysis); done {
		state.StopReason, state.ProviderFailure = end.reason, end.failure
		next = routeFinish
	}
	ev := stateEvent(ctx, state)
	if state.Target != nil {
		ev.Output = optimizerBrief(state)
	}
	ev.Routes = []string{next}
	return ev, nil
}

// OptimizerBrief is the optimizer's whole input once code has chosen its
// target: the fields its instruction names, and nothing it is told to ignore.
// Handed the full CampaignState instead, the model read discovery evidence,
// every hot path and target in the analysis, and repository inventory, all of
// which its instruction forbids it to act on once a target is chosen. The
// session still carries the full state; only the model's view narrows, and a
// cycle with no target left still hands the optimizer everything.
type OptimizerBrief struct {
	OptimizationMode domain.OptimizationPolicy `json:"optimization_mode"`
	Target           agents.Target             `json:"target"`
	SourceExcerpts   []SourceExcerpt           `json:"source_excerpts,omitempty"`
	PriorCandidates  []PriorCandidate          `json:"prior_candidates,omitempty"`
	// EarlierCandidates are the measured candidates of earlier campaigns
	// (CampaignRequest.EarlierCandidates).
	EarlierCandidates []PriorCandidate `json:"earlier_candidates,omitempty"`
	// GoVersion is the target module's declared Go language version.
	GoVersion string `json:"go_version,omitempty"`
}

func optimizerBrief(state CampaignState) OptimizerBrief {
	return OptimizerBrief{
		OptimizationMode:  state.Request.OptimizationMode,
		Target:            *state.Target,
		SourceExcerpts:    state.SourceExcerpts,
		PriorCandidates:   state.PriorCandidates,
		EarlierCandidates: state.Request.EarlierCandidates,
		GoVersion:         state.Request.GoVersion,
	}
}

// planTarget lets code choose what the optimizer attacks whenever the analysis
// ranks causes: the first target no earlier candidate tried. The optimizer then
// sees only that function's source, and its instruction forbids patching any
// other. Left to choose from a ranked list, the optimizer on a live gron
// campaign ignored the top target, the output loop whose bufio fix was once
// accepted at -11.8%, and micro-optimized validIdentifier instead.
func planTarget(state *CampaignState) {
	state.Target = nil
	tried := triedTargets(*state)
	target, ok := nextTarget(state.Analysis.Targets, tried)
	if !ok {
		return
	}
	state.Target = &target
	if excerpts := excerptsAt(state.SourceExcerpts, target); len(excerpts) > 0 {
		state.SourceExcerpts = excerpts
	}
}

// triedTargets is every target already judged. A candidate rejected before
// measurement (its patch did not apply, failed the shape check, or did not
// build) said nothing about its target, so that target gets one more attempt,
// with the reason in prior_candidates; a second unmeasured attempt closes it,
// so a target the optimizer cannot patch does not hold the campaign. A resumed
// campaign's recorded candidates carry whether they were measured, so the
// retry survives a resume (ADR 0017, amended); --history targets count as
// tried, whatever became of them.
func triedTargets(state CampaignState) map[string]bool {
	tried := map[string]bool{}
	for _, t := range state.Request.PriorTargets {
		tried[targetKey(t)] = true
	}
	unmeasured := map[string]int{}
	for _, prior := range state.PriorCandidates {
		if prior.Target == nil {
			continue
		}
		key := targetKey(*prior.Target)
		if prior.Unmeasured {
			unmeasured[key]++
		}
		if !prior.Unmeasured || unmeasured[key] >= maxUnmeasuredAttempts {
			tried[key] = true
		}
	}
	return tried
}

// maxUnmeasuredAttempts is how many candidates one target may spend without
// reaching measurement.
const maxUnmeasuredAttempts = 2

func targetKey(t agents.Target) string { return t.Location + "\x00" + t.Cause }

func nextTarget(targets []agents.Target, tried map[string]bool) (agents.Target, bool) {
	for _, t := range targets {
		if !tried[targetKey(t)] {
			return t, true
		}
	}
	return agents.Target{}, false
}

// excerptsAt keeps the excerpts for the target's location, and, for a
// throwaway_result target (ADR 0027), every function in its Callers
// too: the optimizer needs to see every caller it may rewrite, not only the
// callee. It also keeps the header of each such location's file (the excerpt
// that starts at line 1), which carries the imports a remedy may have to
// extend whichever hot path of that file it was collected for.
func excerptsAt(excerpts []SourceExcerpt, target agents.Target) []SourceExcerpt {
	locations := map[string]bool{target.Location: true}
	for _, f := range target.Callers {
		locations[f.Location] = true
	}
	var kept []SourceExcerpt
	for _, e := range excerpts {
		if locations[e.HotPath] {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	files := map[string]bool{}
	for location := range locations {
		file, _, _ := strings.Cut(location, ":")
		files[file] = true
	}
	for _, e := range excerpts {
		if files[e.Path] && e.StartLine == 1 && !locations[e.HotPath] {
			kept = append([]SourceExcerpt{e}, kept...)
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

func attachExcerpts(ctx adkagent.Context, bench Bench, state *CampaignState, result agents.AnalystResult) {
	analysis := result
	if len(analysis.HotPaths) == 0 {
		analysis.HotPaths = discoveryHotPaths(state.Discovery.HotFunctions)
	}
	if excerpts, err := bench.Excerpts(ctx, analysis); err == nil {
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
	proposal, repair, err := decodeRole[agents.OptimizerResult](ctx, g.deps.Bench, agents.RoleOptimizer, raw)
	if err != nil {
		return nil, fmt.Errorf("optimizer output: %w", err)
	}
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.Proposal = proposal
	assessment, err := g.deps.Bench.Assess(ctx, CandidateRequest{
		Campaign:    state.Request,
		Attempt:     state.CandidatesTried + 1,
		Analysis:    state.Analysis,
		Proposal:    proposal,
		Target:      state.Target,
		RoleFailure: roleFailure(state.CycleFailures, string(agents.RoleOptimizer)),
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate candidate: %w", err)
	}
	assessment, err = bindAssessment(assessment)
	if err != nil {
		return nil, err
	}
	// Stamped after the runner returns so no step of the evaluation can see
	// it: the note rides to the candidate's record and nowhere else.
	assessment.Evidence.ProposalRepair = repair
	state.Candidate, state.Verdict = assessment.Evidence, assessment.Verdict
	return stateEvent(ctx, state), nil
}

// applyPolicy is the last node of a cycle. The verdict was reached when the
// candidate was assessed, before the reviewer spoke, so this node decides
// nothing: it counts the verdict, hands it to the settler to make durable and
// carries the result forward. The review is decoded only to be recorded and
// fed to the next cycle's brief. The node keeps its name for the readers of
// the event path.
func (g *campaignGraph) applyPolicy(ctx adkagent.Context, raw any) (*session.Event, error) {
	review, _, err := decodeRole[agents.ReviewerResult](ctx, g.deps.Bench, agents.RoleReviewer, raw)
	if err != nil {
		return nil, fmt.Errorf("reviewer output: %w", err)
	}
	state, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	state.Review = review
	settled := counted(state, g.cfg.MaxConsecutiveInconclusive > 0)
	err = g.deps.Bench.Settle(ctx, Settlement{
		Assessment: Assessment{Evidence: state.Candidate, Verdict: state.Verdict},
		Target:     state.Target,
		Review:     review,
		Progress: CampaignProgress{
			CandidatesTried:         settled.CandidatesTried,
			ConsecutiveFailures:     settled.ConsecutiveFailures,
			ConsecutiveInconclusive: settled.ConsecutiveInconclusive,
			LastDecision:            state.Verdict.Decision,
			CandidateID:             state.Verdict.CandidateID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("settle candidate: %w", err)
	}
	return stateEvent(ctx, settled), nil
}

// bindAssessment holds an assessment to the invariants the graph relies on: a
// candidate with an ID, a verdict that is one of the three decisions, and a
// verdict that is about that candidate and no other.
func bindAssessment(a Assessment) (Assessment, error) {
	if a.Evidence.Candidate.ID == "" {
		return a, errors.New("evaluate candidate: empty candidate ID")
	}
	if !validDecision(a.Verdict.Decision) {
		return a, fmt.Errorf("evaluate candidate: invalid decision %q", a.Verdict.Decision)
	}
	if a.Verdict.CandidateID == "" {
		a.Verdict.CandidateID = a.Evidence.Candidate.ID
	}
	if a.Verdict.CandidateID != a.Evidence.Candidate.ID {
		return a, fmt.Errorf("evaluate candidate: verdict candidate ID %q does not match %q", a.Verdict.CandidateID, a.Evidence.Candidate.ID)
	}
	return a, nil
}

// counted is the campaign state once the pending verdict is counted: the
// attempt, the history the next brief reads, the streaks, and the accepted
// list. It changes a copy, so a settlement that fails leaves the state the
// graph holds as it was.
func counted(state CampaignState, separateInconclusiveBound bool) CampaignState {
	verdict := state.Verdict
	state.Evaluation = verdict
	state.CandidatesTried++
	state.PriorCandidates = append(slices.Clone(state.PriorCandidates), PriorCandidate{
		Attempt:        state.CandidatesTried,
		Hypothesis:     state.Proposal.Hypothesis,
		Decision:       string(verdict.Decision),
		Reasons:        verdict.Reasons,
		FailureDetail:  state.Candidate.FailureDetail,
		Target:         state.Target,
		Unmeasured:     state.Candidate.Unmeasured,
		ReviewConcerns: state.Review.Concerns,
	})
	countDecision(&state, verdict.Decision, separateInconclusiveBound)
	if verdict.Decision == domain.DecisionAccepted {
		state.AcceptedCandidates = append(slices.Clone(state.AcceptedCandidates), verdict.CandidateID)
	}
	return state
}

// countDecision counts one more verdict on the state's streaks (see
// tallies.after).
func countDecision(state *CampaignState, decision domain.Decision, separateInconclusiveBound bool) {
	t := tallies{consecutiveFailures: state.ConsecutiveFailures, consecutiveInconclusive: state.ConsecutiveInconclusive}.after(decision, separateInconclusiveBound)
	state.ConsecutiveFailures, state.ConsecutiveInconclusive = t.consecutiveFailures, t.consecutiveInconclusive
}

// route finishes the campaign or starts the next cycle. A cycle in which the
// optimizer failed is checked before any bound: its candidate was rejected for
// an empty patch no model wrote, so naming the candidate budget or the
// rejection streak would blame the patches for the provider.
func (g *campaignGraph) route(ctx adkagent.Context, state CampaignState) (*session.Event, error) {
	next := routeFinish
	// The analysis already ended the campaign (skip_candidate): no candidate
	// was tried this cycle, so there is nothing to judge.
	if state.StopReason != "" {
		ev := stateEvent(ctx, state)
		ev.Routes = []string{next}
		return ev, nil
	}
	if _, down := providerFailure(state); down {
		state.OutageCycles++
	} else {
		state.OutageCycles = 0
	}
	if end, done := g.ended(state, afterCycle); done {
		state.StopReason, state.ProviderFailure = end.reason, end.failure
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

func (g *campaignGraph) finalize(ctx adkagent.Context, state CampaignState) (CampaignResult, error) {
	result := CampaignResult{
		CampaignID:         state.Request.CampaignID,
		CandidatesTried:    state.CandidatesTried,
		AcceptedCandidates: slices.Clone(state.AcceptedCandidates),
		FinalEvaluation:    state.Evaluation,
		StopReason:         state.StopReason,
		ProviderFailure:    state.ProviderFailure,
	}
	// The result's status is what ended decided: ProviderFailure is set when a
	// role could not answer, and the caller reads it from here. Nothing is
	// derived a second time for the note.
	if err := g.deps.Bench.Note(ctx, Note{Kind: NoteFinished, Result: result}); err != nil {
		return CampaignResult{}, fmt.Errorf("note campaign finish: %w", err)
	}
	return result, nil
}

func validateDependencies(deps Dependencies) error {
	if deps.Bench == nil {
		return errors.New("bench is required")
	}
	if deps.Agents.Optimizer == nil {
		return errors.New("optimizer agent is required")
	}
	if deps.Causes == nil {
		return errors.New("cause analyst is required")
	}
	if deps.Review == nil {
		return errors.New("review analyst is required")
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
	// A recorded verdict the tallies cannot count would quietly break a
	// streak and buy the resumed process extra candidates before a bound
	// binds, so it is rejected: the bounds are not negotiable by request
	// content.
	for _, c := range req.RecordedCandidates {
		if !validDecision(domain.Decision(c.Decision)) {
			return CampaignRequest{}, fmt.Errorf("recorded candidate %d has invalid decision %q", c.Attempt, c.Decision)
		}
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

// roleFailure returns the cause of role's last absorbed failure this cycle,
// or "" when it answered.
func roleFailure(failures []RoleFailure, role string) string {
	for i := len(failures) - 1; i >= 0; i-- {
		if failures[i].Role == role {
			return failures[i].Cause
		}
	}
	return ""
}
