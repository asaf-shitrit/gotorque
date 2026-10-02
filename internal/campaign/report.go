package campaign

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/agents"
	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/jev"
	"github.com/asaf-shitrit/gotorque/internal/manifest"
)

// Report file names. A live campaign holds the database's exclusive lock, so
// these two files are the only artifacts an operator can read while the run is
// in flight.
const (
	ReportJSONName     = "report.json"
	ReportMarkdownName = "report.md"
)

// candidateEventSummary renders the verdict line the progress stream prints
// while a campaign runs. The full record, with every reason and the metric
// table, reaches only the report file: a live campaign holds the database's
// exclusive lock, so an operator watching a ninety-minute run has no other
// place to read why a candidate was refused.
func candidateEventSummary(record CandidateRecord, primaryMetric string) string {
	summary := fmt.Sprintf("attempt %d: %s", record.Attempt, record.Decision)
	if metric := primaryComparisonSummary(record.Comparisons, primaryMetric); metric != "" {
		summary += " — " + metric
	}
	if len(record.Reasons) > 0 {
		summary += ": " + oneLine(record.Reasons[0], maxEventReasonChars)
	}
	return summary
}

const maxEventReasonChars = 200

// primaryComparisonSummary describes the metric the verdict turns on, with its
// delta and whether benchstat supported it. The metric is the campaign's own:
// under --tradeoff lean a verdict turns on peak memory, and headlining wall
// time there named a number the policy never judged by.
func primaryComparisonSummary(comparisons []domain.MetricComparison, primaryMetric string) string {
	if primaryMetric == "" {
		primaryMetric = manifest.DefaultPrimaryMetric
	}
	chosen := -1
	for i, c := range comparisons {
		// The pooled reading is the headline number when it is present; a
		// per-workload reading is the fallback, never the preference.
		if c.Metric == primaryMetric && c.Workload == "" {
			chosen = i
			break
		}
		if chosen < 0 {
			chosen = i
		}
	}
	if chosen < 0 {
		return ""
	}
	c := comparisons[chosen]
	support := "unsupported"
	if c.StatisticallyFit {
		support = "supported"
	}
	return fmt.Sprintf("%s %+.2f%% (%s)", comparisonLabel(c), c.DeltaPercent, support)
}

// comparisonLabel names a comparison the way a verdict does: the workload it
// was measured on, or the metric when the reading is the pooled one.
func comparisonLabel(comparison domain.MetricComparison) string {
	if comparison.Workload != "" {
		return comparison.Workload
	}
	return comparison.Metric
}

// tableLabel names a comparison inside the metric tables, where the metric is
// not a column of its own and therefore has to be part of the label.
func tableLabel(comparison domain.MetricComparison) string {
	if comparison.Workload != "" {
		return comparison.Workload + "/" + comparison.Metric
	}
	return comparison.Metric
}

// oneLine collapses a reason's line structure so it cannot break the
// one-event-per-line progress format, and bounds its length.
func oneLine(text string, limit int) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	if len(collapsed) <= limit {
		return collapsed
	}
	return collapsed[:limit] + "…"
}

// ReportSchemaVersion stamps the shape of a written report. It exists so a
// campaign directory carries the shape it was written in: the previous
// structural change (comparisons gaining a metric and a workload instead of one
// name) was detectable only by classifying existing directories by hand, and a
// reader could not tell an old artifact from a broken one.
//
// Bump it when a field a reader depends on changes meaning or disappears, and
// note in docs/adr what the new number covers. State of any other version is
// refused when it is loaded (checkSchemaVersion), so every reader can rely on
// the current shape instead of rendering around missing fields.
const ReportSchemaVersion = 1

// checkSchemaVersion refuses state written in a shape this build does not
// read. Campaigns from before versioning carry version 0.
func checkSchemaVersion(state State) error {
	if state.SchemaVersion != ReportSchemaVersion {
		return fmt.Errorf("campaign state has report schema %d, this build reads %d; re-run the campaign", state.SchemaVersion, ReportSchemaVersion)
	}
	return nil
}

func WriteReports(dir string, state State) error {
	// The stamp belongs to the artifact, not to the engine's internal state, so
	// it is applied to the copy being written.
	state.SchemaVersion = ReportSchemaVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, ReportJSONName), data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ReportMarkdownName), []byte(RenderMarkdown(state)), 0o600)
}

func RenderMarkdown(state State) string {
	var b strings.Builder
	writeReportHeader(&b, state)
	writeInventory(&b, state)
	writeBehaviorGate(&b, state)
	writeBaselineWorkloads(&b, state)
	writeSandboxIsolationNotes(&b, state)
	writeDegradedRoles(&b, state)
	writeHistory(&b, state)
	writeCandidateExperiments(&b, state)
	writeVerifications(&b, state)
	writeTokenUsage(&b, state)
	writeJevCache(&b, state)
	fmt.Fprintf(&b, "## Reproduction\n\n```sh\ngotorque optimize --repo %q --manifest %q%s%s\n```\n", state.Repository, state.ManifestPath, tradeoffFlags(state.Tradeoff), historyFlags(state.HistorySources))
	return b.String()
}

func writeReportHeader(b *strings.Builder, state State) {
	fmt.Fprintf(b, "# Go optimization campaign `%s`\n\n", state.ID)
	fmt.Fprintf(b, "**%s evidence** (%s/%s)\n\n", strings.ToUpper(state.Environment.Authority), state.Environment.OS, state.Environment.Architecture)
	fmt.Fprintf(b, "- Status: `%s`\n- Stop reason: %s\n- Repository: `%s`\n- Revision: `%s`\n- Go: `%s`\n- CPU: `%s`\n- Build flags: `%s`\n", state.Status, state.StopReason, state.Repository, state.Environment.Revision, state.Environment.GoVersion, state.Environment.CPU, strings.Join(state.Environment.BuildFlags, " "))
	if state.Analyst == AnalystJev {
		fmt.Fprintf(b, "- Analyst: Jev cause classification (`%s`), not a model role\n", jev.Model)
	}
	if state.Reviewer == ReviewerJev {
		fmt.Fprintf(b, "- Reviewer: Jev behaviour-hazard checks (`%s`), not a model role\n", jev.Model)
	}
	if state.Explorer == ExplorerJev {
		fmt.Fprintf(b, "- Explorer: the target's own options, judged by Jev (`%s`); discovery also sampled: %s\n", jev.Model, orNone(strings.Join(state.DiscoveryWorkloads, "; ")))
	}
	fmt.Fprintf(b, "- Judged under: %s: %s\n", tradeoffName(state.Tradeoff), manifest.Describe(state.Manifest.Performance))
	if state.DiscoveryProfileSource != "" {
		fmt.Fprintf(b, "- Discovery profile: targets chosen from %s\n", state.DiscoveryProfileSource)
	}
	fmt.Fprintf(b, "\n- Report schema: `%d`\n\n", state.SchemaVersion)
}

// tradeoffName names the trade-off a campaign's verdicts were judged under.
func tradeoffName(t manifest.Tradeoff) string {
	switch {
	case t.IsZero():
		return "the manifest"
	case t.Name == "":
		return "custom trade-off"
	case !sameAllowances(t, manifest.Presets[t.Name]):
		return fmt.Sprintf("trade-off `%s` with overrides", t.Name)
	}
	return fmt.Sprintf("trade-off `%s`", t.Name)
}

func sameAllowances(a, b manifest.Tradeoff) bool {
	return maps.Equal(a.Allow, b.Allow) && a.Objective == b.Objective
}

// tradeoffFlags renders a trade-off as the flags that reproduce it.
func tradeoffFlags(t manifest.Tradeoff) string {
	if t.IsZero() {
		return ""
	}
	var flags strings.Builder
	if t.Name != "" {
		fmt.Fprintf(&flags, " --tradeoff %s", t.Name)
	}
	preset := manifest.Presets[t.Name]
	for _, metric := range slices.Sorted(maps.Keys(t.Allow)) {
		if v, ok := preset.Allow[metric]; !ok || v != t.Allow[metric] {
			fmt.Fprintf(&flags, " --allow %s=%g%%", metric, t.Allow[metric])
		}
	}
	return flags.String()
}

func writeInventory(b *strings.Builder, state State) {
	fmt.Fprintf(b, "## Repository inventory\n\nDiscovered %d packages and %d command entry points.\n\n", len(state.Inventory.Packages), len(state.Inventory.Commands))
	for _, command := range state.Inventory.Commands {
		fmt.Fprintf(b, "- `%s`\n", command)
	}
	if len(state.Inventory.Unloadable) > 0 {
		fmt.Fprintf(b, "\n%d package(s) could not be loaded and were left out:\n\n", len(state.Inventory.Unloadable))
		for _, p := range state.Inventory.Unloadable {
			fmt.Fprintf(b, "- `%s`\n", p)
		}
	}
}

// writeBehaviorGate states what the candidate gate could verify, because a
// target whose own suite is already red cannot be held to "the suite passes"
// and the report must not imply otherwise.
func writeBehaviorGate(b *strings.Builder, state State) {
	defer writeUnbuildable(b, state.BaselineUnbuildable)
	if len(state.BaselineTestFailures) == 0 && len(state.BaselineTestPasses) == 0 && state.CompletedSteps[baselinePassesStep] {
		// jj and mdtohtml have no tests of their own. "The suite passes" is
		// true of an empty suite and says nothing, so the report names what
		// actually guards behavior there.
		b.WriteString("\n## Behavior gate\n\n**The target has no tests of its own.** Nothing but the seed workloads' outputs guards behavior: a candidate that changes output only on inputs the workloads never give it cannot be caught here.\n")
		return
	}
	if len(state.BaselineTestFailures) == 0 {
		b.WriteString("\n## Behavior gate\n\nThe upstream test suite passes on the unpatched revision, so every candidate's full suite must pass.\n")
		return
	}
	fmt.Fprintf(b, "\n## Behavior gate\n\nThe upstream test suite already fails on the unpatched revision: %d test(s) are excluded from the gate, and a candidate is rejected only for failures not listed here.\n\n", len(state.BaselineTestFailures))
	for _, failure := range state.BaselineTestFailures {
		fmt.Fprintf(b, "- `%s`\n", failure)
	}
}

// writeUnbuildable lists packages whose tests could not build or set up on
// the unpatched revision, which the gate therefore cannot use.
func writeUnbuildable(b *strings.Builder, packages []string) {
	if len(packages) == 0 {
		return
	}
	fmt.Fprintf(b, "\nThe tests of %d package(s) cannot build or set up on the unpatched revision, so they do not gate candidates:\n\n", len(packages))
	for _, p := range packages {
		fmt.Fprintf(b, "- `%s`\n", p)
	}
}

func writeBaselineWorkloads(b *strings.Builder, state State) {
	fmt.Fprintf(b, "\n## Baseline workloads\n\n| Workload | Exit | Wall time | Evidence |\n|---|---:|---:|---|\n")
	for _, run := range state.Runs {
		fmt.Fprintf(b, "| `%s` | %d | %s | `%s` |\n", run.Workload, run.ExitCode, run.Duration, run.ID)
	}
}

// writeSandboxIsolationNotes explains, next to the runs it affected, any gap
// between what the manifest's sandbox block asked for and what this host
// could actually enforce. Evidence gathered while this section is non-empty
// is not equivalent to a fully isolated run (see docs/architecture.md's
// isolation section), so it belongs beside the runs, not buried in a log.
func writeSandboxIsolationNotes(b *strings.Builder, state State) {
	if len(state.SandboxIsolationNotes) == 0 {
		return
	}
	b.WriteString("\n## Sandbox isolation\n\nThe manifest's `sandbox` block asked for isolation this host could not fully provide. Evidence collected under these notes is not equivalent to a fully isolated run.\n\n")
	for _, note := range state.SandboxIsolationNotes {
		fmt.Fprintf(b, "- %s\n", note)
	}
}

// writeDegradedRoles explains a campaign in which an agent node failed and the
// graph carried on: the roles are listed immediately above the candidates they
// may have left empty, so `patch is empty` is read next to its cause rather
// than as a mystery.
func writeDegradedRoles(b *strings.Builder, state State) {
	if len(state.DegradedRoles) == 0 {
		return
	}
	b.WriteString("\n## Degraded roles\n\nA role whose model call failed is absorbed rather than fatal: the campaign continues with an empty result, so a candidate below may be missing that role's output. Two consecutive cycles in which the optimizer failed stop the campaign as `failed` instead, naming the provider; resume it once the provider answers.\n\n")
	for _, degraded := range state.DegradedRoles {
		fmt.Fprintf(b, "- `%s`: %s\n", degraded.Role, degraded.Cause)
	}
	b.WriteString("\n")
}

func writeCandidateExperiments(b *strings.Builder, state State) {
	b.WriteString("\n## Candidate experiments\n\n")
	if len(state.CandidateRecords) == 0 {
		b.WriteString("No source candidate was attempted. Completing with no accepted candidate is a successful harness result.\n\n")
		return
	}
	accepted := 0
	for i := range state.CandidateRecords {
		if state.CandidateRecords[i].Accepted {
			accepted++
		}
	}
	fmt.Fprintf(b, "%d candidate(s) evaluated, %d accepted by policy.\n\n", len(state.CandidateRecords), accepted)
	for _, record := range state.CandidateRecords {
		writeCandidateRecord(b, record)
	}
}

func writeCandidateRecord(b *strings.Builder, record CandidateRecord) {
	fmt.Fprintf(b, "### Attempt %d: `%s` **%s**\n\n", record.Attempt, record.CandidateID, strings.ToUpper(string(record.Decision)))
	writeCandidateMeta(b, record)
	writeCandidateLoad(b, record)
	writeCandidateFailure(b, record)
	writeCandidateSamples(b, record)
	writeCandidateComparisons(b, record)
	writeCandidatePGO(b, record)
}

// targetEvidence says what raised a target: Jev's deviation from its usual
// answer, or code, whose causes carry no z-score and used to print as
// "+0.0 sd", as if Jev had found nothing unusual.
func targetEvidence(t agents.Target) string {
	if t.Cause == causeThrowawayResult || t.Cause == causeUnbufferedWrites {
		return "code-derived"
	}
	return fmt.Sprintf("%+.1f sd", t.Z)
}

func writeCandidateMeta(b *strings.Builder, record CandidateRecord) {
	if t := record.Target; t != nil {
		fmt.Fprintf(b, "- Target: `%s` at `%s`, %s (%s)\n", t.Function, t.Location, t.Cause, targetEvidence(*t))
	}
	if record.Hypothesis != "" {
		fmt.Fprintf(b, "- Hypothesis: %s\n", record.Hypothesis)
	}
	if record.PatchPath != "" {
		fmt.Fprintf(b, "- Patch: `%s`%s\n", record.PatchPath, acceptedMarker(record.Accepted))
	}
	switch record.Transport {
	case FunctionSourceTransport:
		b.WriteString("- Transport: function_source (code built the diff from the optimizer's replacement function)\n")
	case MultiFunctionSourceTransport:
		b.WriteString("- Transport: function_sources (code built a multi-file diff from the optimizer's replacement callee and callers, ADR 0027)\n")
	}
	if record.ProposalRepair != "" {
		// A salvaged proposal is judged like any other; this line only keeps
		// it from reading as the one the model sent.
		fmt.Fprintf(b, "- Proposal salvaged: the optimizer's output parsed only after the decoder %s, so the patch may not be the one the model intended\n", record.ProposalRepair)
	}
	if record.Summary != "" {
		fmt.Fprintf(b, "- Evidence: %s\n", record.Summary)
	}
	for _, reason := range record.Reasons {
		fmt.Fprintf(b, "- Policy: %s\n", reason)
	}
	for _, concern := range record.ReviewConcerns {
		fmt.Fprintf(b, "- Review: %s\n", concern)
	}
}

func writeCandidateFailure(b *strings.Builder, record CandidateRecord) {
	if record.FailureDetail == "" {
		return
	}
	b.WriteString("- Failure detail:\n\n```text\n")
	b.WriteString(strings.TrimRight(record.FailureDetail, "\n"))
	b.WriteString("\n```\n\n")
}

func writeCandidateSamples(b *strings.Builder, record CandidateRecord) {
	if len(record.Samples) == 0 {
		return
	}
	b.WriteString("\nPer-repetition wall times (ns):\n\n")
	for _, s := range record.Samples {
		fmt.Fprintf(b, "- `%s` baseline %v / candidate %v\n", s.Workload, fmtFloats(s.BaselineNs), fmtFloats(s.CandidateNs))
	}
	b.WriteString("\n")
}

func writeCandidateComparisons(b *strings.Builder, record CandidateRecord) {
	if len(record.Comparisons) > 0 {
		writeMetricTable(b, "\n| Comparison | Baseline | Candidate | Δ | Supported |\n|---|---:|---:|---:|---|\n", record.Comparisons)
	}
	if record.BenchstatOutput != "" {
		fmt.Fprintf(b, "\nBenchstat comparison:\n\n```text\n%s\n```\n", record.BenchstatOutput)
	}
}

func writeCandidatePGO(b *strings.Builder, record CandidateRecord) {
	// PGO lane is informational by design: it attributes the compiler's
	// profile-guided effect on top of the ordinary verdict and never
	// changes accept/reject decisions.
	if len(record.PgoComparisons) == 0 && record.PgoNote == "" {
		return
	}
	fmt.Fprintf(b, "\n#### PGO lane (informational; never changes accept/reject decisions)\n\n")
	if record.PgoNote != "" {
		fmt.Fprintf(b, "%s\n\n", record.PgoNote)
	}
	if len(record.PgoComparisons) > 0 {
		writeMetricTable(b, "| PGO comparison | Baseline-pgo | Candidate-pgo | Δ | Supported |\n|---|---:|---:|---:|---|\n", record.PgoComparisons)
	}
}

func writeMetricTable(b *strings.Builder, header string, comparisons []domain.MetricComparison) {
	b.WriteString(header)
	for _, c := range comparisons {
		fmt.Fprintf(b, "| `%s` | %.4g | %.4g | %s | %s |\n", tableLabel(c), c.Baseline, c.Candidate, formatDelta(c.DeltaPercent), fitLabel(c.StatisticallyFit))
	}
	b.WriteString("\n")
}

func fitLabel(fit bool) string {
	if fit {
		return "yes"
	}
	return "no"
}

func formatDelta(delta float64) string {
	if math.IsNaN(delta) {
		return "n/a"
	}
	return fmt.Sprintf("%+.2f%%", delta)
}

func writeTokenUsage(b *strings.Builder, state State) {
	if len(state.TokenUsage) == 0 {
		return
	}
	b.WriteString("## Model usage\n\n| Role | Requests | Prompt tokens | Completion tokens | Total tokens |\n|---|---:|---:|---:|---:|\n")
	roles := make([]string, 0, len(state.TokenUsage))
	for role := range state.TokenUsage {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		usage := state.TokenUsage[role]
		fmt.Fprintf(b, "| `%s` | %d | %d | %d | %d |\n", role, usage.Requests, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
	b.WriteString("\n")
}

// writeJevCache reports how many of this campaign's Jev requests were
// answered from the cache rather than the network (ADR 0028): a role missing
// from the map ran without a Jev evaluator, or cached nothing because none of
// its requests repeated.
func writeJevCache(b *strings.Builder, state State) {
	if len(state.JevCache) == 0 {
		return
	}
	b.WriteString("## Jev cache\n\n| Role | Hits | Misses |\n|---|---:|---:|\n")
	roles := make([]string, 0, len(state.JevCache))
	for role := range state.JevCache {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		snapshot := state.JevCache[role]
		fmt.Fprintf(b, "| `%s` | %d | %d |\n", role, snapshot.Hits, snapshot.Misses)
	}
	b.WriteString("\n")
}

func acceptedMarker(accepted bool) string {
	if accepted {
		return " (accepted)"
	}
	return ""
}

func LoadReport(dir string) (State, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return State{}, err
	}
	store, err := OpenStore(filepath.Join(abs, DatabaseName))
	if err != nil {
		// A running campaign holds the database's exclusive lock, so the
		// snapshot it writes after every verdict is the only readable copy.
		// Falling back to it turns "timeout" into the campaign's state as of
		// its last candidate.
		if state, snapshotErr := loadReportSnapshot(filepath.Join(abs, ReportJSONName)); snapshotErr == nil {
			return state, nil
		}
		return State{}, err
	}
	// Read-only load; a Close error carries no data-loss meaning.
	defer func() { _ = store.Close() }()
	state, err := store.Load()
	if err != nil {
		return State{}, err
	}
	return markStaleRunning(state), nil
}

// markStaleRunning reports a campaign whose persisted Status is still
// "running" as interrupted instead. OpenStore only returns a live *Store when
// it can take bbolt's exclusive lock, and no live campaign process ever
// releases that lock voluntarily -- it holds it until the run stops and
// records a terminal status. Reaching this line with Status still "running"
// therefore means the process that would have kept it "running" is gone
// without recording why, most likely SIGKILLed, not that the campaign is
// live. LoadReport never writes back: the correction only affects what this
// call returns, so a campaign a resumed process is genuinely still running
// keeps reporting "running" in its own state and to a caller who takes the
// lock.
func markStaleRunning(state State) State {
	if state.Status != StatusRunning {
		return state
	}
	state.Status = StatusInterrupted
	state.StopReason = "process exited without recording a stop (no process holds the campaign database lock)"
	return state
}

func loadReportSnapshot(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	return state, checkSchemaVersion(state)
}

func fmtFloats(values []float64) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.FormatInt(int64(v), 10))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// writeHistory lists the earlier campaigns --history read and how many
// measured targets each one carried into this campaign as already tried.
func writeHistory(b *strings.Builder, state State) {
	if len(state.HistorySources) == 0 {
		return
	}
	fmt.Fprintf(b, "## Campaign history\n\n%d target(s) earlier campaigns of this revision already measured were counted as tried, so no candidate here re-proposed them.\n\n", len(state.HistoryTargets))
	b.WriteString("| Campaign | Directory | Targets carried |\n|---|---|---:|\n")
	for _, s := range state.HistorySources {
		carried := strconv.Itoa(s.Targets)
		if s.Skipped != "" {
			carried = "skipped: " + s.Skipped
		}
		fmt.Fprintf(b, "| `%s` | `%s` | %s |\n", orNone(s.CampaignID), s.Directory, carried)
	}
	for _, t := range state.HistoryTargets {
		fmt.Fprintf(b, "\n- `%s` at `%s`, %s", t.Function, t.Location, t.Cause)
	}
	b.WriteString("\n\n")
}

func historyFlags(sources []HistorySource) string {
	var b strings.Builder
	for _, s := range sources {
		fmt.Fprintf(&b, " --history %q", s.Directory)
	}
	return b.String()
}

// writeCandidateLoad shows the load averages sampled around a candidate's
// measurement, and warns when the machine was contended (loadavg.go).
func writeCandidateLoad(b *strings.Builder, record CandidateRecord) {
	if record.QuietWait > 0 {
		verdict := "until it was quiet"
		if record.QuietWaitExpired {
			verdict = "and gave up; it was measured while still contended"
		}
		fmt.Fprintf(b, "- Waited %s for the machine to go quiet before measuring, %s\n", record.QuietWait, verdict)
	}
	if len(record.DiscardedLoad) > 0 {
		fmt.Fprintf(b, "- Measured twice: the first pass ended contended (load %s) and was discarded\n", joinLoads(record.DiscardedLoad))
	}
	if len(record.LoadAverages) == 0 {
		return
	}
	fmt.Fprintf(b, "- Load average during measurement: %s", joinLoads(record.LoadAverages))
	if record.LoadContended {
		b.WriteString(" **(contended: load above 0.7 per CPU; other work may have moved these timings)**")
	}
	b.WriteString("\n")
}

func joinLoads(loads []float64) string {
	parts := make([]string, 0, len(loads))
	for _, l := range loads {
		parts = append(parts, strconv.FormatFloat(l, 'f', 2, 64))
	}
	return strings.Join(parts, " -> ")
}

// writeVerifications lists accepted candidates evaluated again from their
// recorded patches, and whether the acceptance held.
func writeVerifications(b *strings.Builder, state State) {
	if len(state.Verifications) == 0 {
		return
	}
	b.WriteString("## Verification\n\n| Attempt | Pairs | Originally | On verification | Load | Output variants | Held |\n|---:|---:|---|---|---|---|---|\n")
	for _, v := range state.Verifications {
		held := "no"
		if v.Confirmed() {
			held = "yes"
		}
		load := joinLoads(v.LoadAverages)
		if v.LoadContended {
			load += " (contended)"
		}
		outputs := fmt.Sprintf("%d checked", len(v.OutputChecks))
		if len(v.OutputMismatches) > 0 {
			outputs += ", differ: " + strings.Join(v.OutputMismatches, ", ")
		}
		fmt.Fprintf(b, "| %d | %d | %s | %s | %s | %s | %s |\n", v.Attempt, v.Pairs, v.Original, v.Decision, orNone(load), outputs, held)
	}
	b.WriteString("\n")
}
