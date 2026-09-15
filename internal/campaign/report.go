package campaign

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/manifest"
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
func candidateEventSummary(record CandidateRecord) string {
	summary := fmt.Sprintf("attempt %d: %s", record.Attempt, record.Decision)
	if metric := primaryComparisonSummary(record.Comparisons); metric != "" {
		summary += " — " + metric
	}
	if len(record.Reasons) > 0 {
		summary += ": " + oneLine(record.Reasons[0], maxEventReasonChars)
	}
	return summary
}

const maxEventReasonChars = 200

// primaryComparisonSummary describes the metric the verdict turns on, with its
// delta and whether benchstat supported it.
func primaryComparisonSummary(comparisons []domain.MetricComparison) string {
	chosen := -1
	for i, c := range comparisons {
		// The pooled reading is the headline number when it is present; a
		// per-workload reading is the fallback, never the preference.
		if c.Metric == manifest.DefaultPrimaryMetric && c.Workload == "" {
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
// was measured on, or the metric when the reading is the pooled one. A record
// written before comparisons carried their workload keeps the old shape and
// reports that it has no label rather than an empty cell.
func comparisonLabel(comparison domain.MetricComparison) string {
	if comparison.Workload != "" {
		return comparison.Workload
	}
	return metricLabel(comparison)
}

// tableLabel names a comparison inside the metric tables, where the metric is
// not a column of its own and therefore has to be part of the label.
func tableLabel(comparison domain.MetricComparison) string {
	if comparison.Workload != "" && comparison.Metric != "" {
		return comparison.Workload + "/" + comparison.Metric
	}
	return metricLabel(comparison)
}

// metricLabel falls back to the metric name, and to a placeholder for records
// that predate structured comparisons and therefore carry neither field.
func metricLabel(comparison domain.MetricComparison) string {
	if comparison.Metric == "" {
		return "unlabelled"
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

func WriteReports(dir string, state State) error {
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
	writeCandidateExperiments(&b, state)
	writeTokenUsage(&b, state)
	fmt.Fprintf(&b, "## Reproduction\n\n```sh\ngotorque optimize --repo %q --manifest %q\n```\n", state.Repository, state.ManifestPath)
	return b.String()
}

func writeReportHeader(b *strings.Builder, state State) {
	fmt.Fprintf(b, "# Go optimization campaign `%s`\n\n", state.ID)
	fmt.Fprintf(b, "**%s evidence** (%s/%s)\n\n", strings.ToUpper(state.Environment.Authority), state.Environment.OS, state.Environment.Architecture)
	fmt.Fprintf(b, "- Status: `%s`\n- Stop reason: %s\n- Repository: `%s`\n- Revision: `%s`\n- Go: `%s`\n- CPU: `%s`\n- Build flags: `%s`\n\n", state.Status, state.StopReason, state.Repository, state.Environment.Revision, state.Environment.GoVersion, state.Environment.CPU, strings.Join(state.Environment.BuildFlags, " "))
}

func writeInventory(b *strings.Builder, state State) {
	fmt.Fprintf(b, "## Repository inventory\n\nDiscovered %d packages and %d command entry points.\n\n", len(state.Inventory.Packages), len(state.Inventory.Commands))
	for _, command := range state.Inventory.Commands {
		fmt.Fprintf(b, "- `%s`\n", command)
	}
}

// writeBehaviorGate states what the candidate gate could verify, because a
// target whose own suite is already red cannot be held to "the suite passes"
// and the report must not imply otherwise.
func writeBehaviorGate(b *strings.Builder, state State) {
	if len(state.BaselineTestFailures) == 0 {
		b.WriteString("\n## Behavior gate\n\nThe upstream test suite passes on the unpatched revision, so every candidate's full suite must pass.\n")
		return
	}
	fmt.Fprintf(b, "\n## Behavior gate\n\nThe upstream test suite already fails on the unpatched revision: %d test(s) are excluded from the gate, and a candidate is rejected only for failures not listed here.\n\n", len(state.BaselineTestFailures))
	for _, failure := range state.BaselineTestFailures {
		fmt.Fprintf(b, "- `%s`\n", failure)
	}
}

func writeBaselineWorkloads(b *strings.Builder, state State) {
	fmt.Fprintf(b, "\n## Baseline workloads\n\n| Workload | Exit | Wall time | Evidence |\n|---|---:|---:|---|\n")
	for _, run := range state.Runs {
		fmt.Fprintf(b, "| `%s` | %d | %s | `%s` |\n", run.WorkloadID, run.ExitCode, run.Duration, run.ID)
	}
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
	writeCandidateSamples(b, record)
	writeCandidateComparisons(b, record)
	writeCandidatePGO(b, record)
}

func writeCandidateMeta(b *strings.Builder, record CandidateRecord) {
	if record.Hypothesis != "" {
		fmt.Fprintf(b, "- Hypothesis: %s\n", record.Hypothesis)
	}
	if record.PatchPath != "" {
		fmt.Fprintf(b, "- Patch: `%s`%s\n", record.PatchPath, acceptedMarker(record.Accepted))
	}
	if record.Summary != "" {
		fmt.Fprintf(b, "- Evidence: %s\n", record.Summary)
	}
	for _, reason := range record.Reasons {
		fmt.Fprintf(b, "- Policy: %s\n", reason)
	}
}

func writeCandidateSamples(b *strings.Builder, record CandidateRecord) {
	if len(record.Samples) == 0 {
		return
	}
	b.WriteString("\nPer-repetition wall times (ns):\n\n")
	for _, s := range record.Samples {
		workload := s.Workload
		if workload == "" {
			workload = "unlabelled"
		}
		fmt.Fprintf(b, "- `%s` baseline %v / candidate %v\n", workload, fmtFloats(s.BaselineNs), fmtFloats(s.CandidateNs))
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
	return store.Load()
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
	return state, nil
}

func fmtFloats(values []float64) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.FormatInt(int64(v), 10))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
