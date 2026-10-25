package campaign

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/asaf-shitrit/gotorque/internal/domain"
	"github.com/asaf-shitrit/gotorque/internal/version"
)

// Verdict classes a scorecard counts. A candidate lands in exactly one, first
// match wins, and harness comes first: a verdict the harness caused says
// nothing about the patch, whatever else it looks like.
const (
	classHarness      = "harness"
	classAccepted     = "accepted"
	classInconclusive = "inconclusive"
	classGuardrail    = "guardrail"
	classTests        = "tests"
	classBuild        = "build"
	classOther        = "other"
)

// ScorecardRow is one campaign's verdicts by class.
type ScorecardRow struct {
	Campaign string
	Target   string
	// Builds are the gotorque builds this campaign ran on, the one that
	// started it first, without repeats. A campaign written before the build
	// was recorded carries version.Unknown, so it groups with the other
	// pre-record campaigns instead of posing as a build of its own.
	Builds []string
	// DirtyBuild marks a campaign that ran at least one build from a checkout
	// with uncommitted changes. Such a build names the revision it sat on, not
	// the code it ran, so two of them read the same name above and can still
	// reach different verdicts.
	DirtyBuild bool
	StopReason string
	// NullCandidates is the campaign's persisted null-candidate bound. A
	// positive value marks a campaign that measured code-generated patches
	// changing nothing instead of running the agent graph (runNullCandidates),
	// so any accept it recorded is a false accept of the harness itself.
	NullCandidates int
	Counts         map[string]int
	Verified       int
	Held           int
}

// Scorecard summarises campaigns for the stability criteria: how many
// acceptances were verified and held (false acceptances), how many
// rejections came from guardrails and gates, and how many verdicts the
// harness itself caused.
func Scorecard(dirs []string) ([]ScorecardRow, error) {
	rows := make([]ScorecardRow, 0, len(dirs))
	for _, dir := range dirs {
		state, err := LoadReport(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		rows = append(rows, scorecardRow(filepath.Base(dir), state))
	}
	return rows, nil
}

func scorecardRow(name string, state State) ScorecardRow {
	row := ScorecardRow{Campaign: name, Target: state.Manifest.Name, StopReason: state.StopReason, NullCandidates: state.NullCandidates, Builds: campaignBuilds(state), DirtyBuild: campaignRanDirtyBuild(state), Counts: map[string]int{}}
	held := map[int]bool{}
	for _, v := range state.Verifications {
		held[v.Attempt] = v.Confirmed()
	}
	for _, r := range state.CandidateRecords {
		class := verdictClass(r)
		row.Counts[class]++
		if class != classAccepted {
			continue
		}
		if confirmed, ok := held[r.Attempt]; ok {
			row.Verified++
			if confirmed {
				row.Held++
			}
		}
	}
	return row
}

// campaignBuilds names the gotorque builds a campaign ran on, without repeats:
// the one that started it, then each resume and verify run. A campaign written
// before builds were recorded reports version.Unknown.
func campaignBuilds(state State) []string {
	names := []string{state.Build.Describe()}
	for _, run := range state.LaterBuilds {
		if name := run.Build.Describe(); !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// campaignRanDirtyBuild reports whether a build the campaign ran came from a
// checkout with uncommitted changes.
func campaignRanDirtyBuild(state State) bool {
	if state.Build.Modified {
		return true
	}
	return slices.ContainsFunc(state.LaterBuilds, func(run BuildRun) bool { return run.Build.Modified })
}

// buildCell is a row's builds as one cell. A row built without the field, as a
// caller constructing rows directly can, still says Unknown rather than
// leaving the column blank.
func buildCell(row ScorecardRow) string {
	if len(row.Builds) == 0 {
		return version.Unknown
	}
	return strings.Join(row.Builds, ", ")
}

// writeBuildWarning says when the campaigns being grouped did not all run on
// the same gotorque build, or when one of those builds came from a checkout
// with uncommitted changes. It only says so: the rates below are computed from
// the persisted records whatever build wrote them, because a verdict must not
// depend on the reader (the build record is data, not policy). Grouping the
// campaigns by build stays the caller's job, the same way held-out versus
// development does.
func writeBuildWarning(b *strings.Builder, rows []ScorecardRow) {
	writeDifferingBuildWarning(b, buildNames(rows))
	if slices.ContainsFunc(rows, func(row ScorecardRow) bool { return row.DirtyBuild }) {
		b.WriteString("> Builds include a dirty checkout: a binary built from uncommitted changes names the revision it sat on, not the code it ran. Two dirty builds of one revision read the same here and can still reach different verdicts, so a rate below can mix verdicts that no build name separates.\n\n")
	}
}

// buildNames lists every build the grouped campaigns ran on, without repeats.
func buildNames(rows []ScorecardRow) []string {
	names := []string{}
	for _, row := range rows {
		for _, name := range row.Builds {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// writeDifferingBuildWarning names the builds a group mixed, when it mixed any.
func writeDifferingBuildWarning(b *strings.Builder, names []string) {
	if len(names) < 2 {
		return
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "`" + name + "`"
	}
	fmt.Fprintf(b, "> Builds differ: these campaigns ran on %d gotorque builds (%s), so a rate below mixes verdicts from different builds. Group them by build before comparing.\n\n",
		len(names), strings.Join(quoted, ", "))
}

// verdictClass puts one candidate's verdict in its scorecard class.
func verdictClass(r CandidateRecord) string {
	summary := r.Summary + " " + strings.Join(r.Reasons, " ")
	switch {
	case r.LoadContended || strings.Contains(summary, "the optimizer did not answer"):
		return classHarness
	case r.Accepted:
		return classAccepted
	case r.Decision == domain.DecisionInconclusive:
		return classInconclusive
	case strings.Contains(summary, "upstream test suite failed"):
		return classTests
	case strings.Contains(summary, "rejected before build") || strings.Contains(summary, "build failed"):
		return classBuild
	case strings.Contains(summary, "regressed by"):
		return classGuardrail
	default:
		return classOther
	}
}

var scorecardColumns = []string{classAccepted, classInconclusive, classGuardrail, classTests, classBuild, classHarness, classOther}

// splitScorecardRows separates the agent campaigns from the null-candidate
// campaigns. Only the latter's counts measure a false accept or a false
// reject, so the two never share a totals row.
func splitScorecardRows(rows []ScorecardRow) (agent, null []ScorecardRow) {
	for _, r := range rows {
		if r.NullCandidates > 0 {
			null = append(null, r)
			continue
		}
		agent = append(agent, r)
	}
	return agent, null
}

// RenderScorecard is the scorecard as Markdown: one table of agent-campaign
// verdicts with a totals row, and, when the input holds null campaigns, a
// second table of their false-accept and false-reject rates. A null campaign
// accepts nothing a sound harness could call a speedup, so its accepts are
// kept out of the agent totals and reported as false accepts.
func RenderScorecard(rows []ScorecardRow) string {
	agent, null := splitScorecardRows(rows)
	var b strings.Builder
	writeBuildWarning(&b, rows)
	if len(null) > 0 {
		b.WriteString("## Agent campaigns\n\n")
	}
	writeAgentScorecard(&b, agent)
	if len(null) > 0 {
		b.WriteString("\n")
		writeNullScorecard(&b, null)
	}
	return b.String()
}

func writeAgentScorecard(b *strings.Builder, rows []ScorecardRow) {
	b.WriteString("| Campaign | Target | build | " + strings.Join(scorecardColumns, " | ") + " | verified/held | stop |\n")
	b.WriteString("|---|---|---|" + strings.Repeat("---:|", len(scorecardColumns)) + "---|---|\n")
	total := ScorecardRow{Counts: map[string]int{}}
	for _, r := range rows {
		writeScorecardRow(b, r.Campaign, r.Target, buildCell(r), r, r.StopReason)
		for class, n := range r.Counts {
			total.Counts[class] += n
		}
		total.Verified += r.Verified
		total.Held += r.Held
	}
	writeScorecardRow(b, "**total**", fmt.Sprintf("%d campaigns", len(rows)), "", total, "")
}

func writeScorecardRow(b *strings.Builder, campaign, target, build string, r ScorecardRow, stop string) {
	cells := make([]string, 0, len(scorecardColumns))
	for _, class := range scorecardColumns {
		cells = append(cells, strconv.Itoa(r.Counts[class]))
	}
	fmt.Fprintf(b, "| %s | %s | %s | %s | %d/%d | %s |\n", campaign, target, build, strings.Join(cells, " | "), r.Verified, r.Held, stop)
}

// nullTableColumns are the null section's rate columns, each printed as a rate
// followed by its exact one-sided 95% upper bound.
var nullTableColumns = []string{"valid runs", "false accepts", "false rejects", "false-accept rate", "95% upper", "false-reject rate", "95% upper"}

// nullScore is one null campaign's share of the harness's own error rates.
type nullScore struct {
	Valid        int
	FalseAccepts int
	FalseRejects int
}

// nullScore counts what a null campaign's verdicts say about the harness: a
// harness-class verdict says nothing about the patch and is left out of the
// valid runs. An inconclusive verdict is the correct outcome for a null, so
// only guardrail, test and build rejections count as false rejects.
func (r ScorecardRow) nullScore() nullScore {
	score := nullScore{}
	for class, n := range r.Counts {
		if class != classHarness {
			score.Valid += n
		}
	}
	score.FalseAccepts = r.Counts[classAccepted]
	score.FalseRejects = r.Counts[classGuardrail] + r.Counts[classTests] + r.Counts[classBuild]
	return score
}

func (s nullScore) add(o nullScore) nullScore {
	return nullScore{Valid: s.Valid + o.Valid, FalseAccepts: s.FalseAccepts + o.FalseAccepts, FalseRejects: s.FalseRejects + o.FalseRejects}
}

func writeNullScorecard(b *strings.Builder, rows []ScorecardRow) {
	b.WriteString("## Null candidates\n\n")
	b.WriteString("Code-generated patches that change nothing. An accept is a false accept of the harness, and a guardrail, test or build rejection is a false reject; inconclusive is the right answer. Harness-class verdicts are left out of the valid runs. The bounds are exact one-sided 95% Clopper-Pearson limits.\n\n")
	b.WriteString("| Campaign | Target | build | " + strings.Join(nullTableColumns, " | ") + " | stop |\n")
	b.WriteString("|---|---|---|" + strings.Repeat("---:|", len(nullTableColumns)) + "---|\n")
	total := nullScore{}
	for _, r := range rows {
		score := r.nullScore()
		writeNullScorecardRow(b, r.Campaign, r.Target, buildCell(r), score, r.StopReason)
		total = total.add(score)
	}
	writeNullScorecardRow(b, "**total**", fmt.Sprintf("%d campaigns", len(rows)), "", total, "")
}

func writeNullScorecardRow(b *strings.Builder, campaign, target, build string, score nullScore, stop string) {
	acceptRate, acceptUpper := rateCell(score.FalseAccepts, score.Valid)
	rejectRate, rejectUpper := rateCell(score.FalseRejects, score.Valid)
	fmt.Fprintf(b, "| %s | %s | %s | %d | %d | %d | %s | %s | %s | %s | %s |\n", campaign, target, build, score.Valid,
		score.FalseAccepts, score.FalseRejects, acceptRate, acceptUpper, rejectRate, rejectUpper, stop)
}

// rateCell is a rate and its bound as percentages of the valid runs.
func rateCell(hits, runs int) (rate, upper string) {
	if runs == 0 {
		return "n/a", "n/a"
	}
	return percent(float64(hits) / float64(runs)), percent(clopperPearsonUpper(hits, runs))
}

func percent(v float64) string { return fmt.Sprintf("%.2f%%", 100*v) }
