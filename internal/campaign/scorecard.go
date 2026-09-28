package campaign

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"example.com/gotorque/internal/domain"
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
	Campaign   string
	Target     string
	StopReason string
	Counts     map[string]int
	Verified   int
	Held       int
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
	row := ScorecardRow{Campaign: name, Target: state.Manifest.Name, StopReason: state.StopReason, Counts: map[string]int{}}
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

// RenderScorecard is the scorecard as a Markdown table with a totals row.
func RenderScorecard(rows []ScorecardRow) string {
	var b strings.Builder
	b.WriteString("| Campaign | Target | " + strings.Join(scorecardColumns, " | ") + " | verified/held | stop |\n")
	b.WriteString("|---|---|" + strings.Repeat("---:|", len(scorecardColumns)) + "---|---|\n")
	total := ScorecardRow{Counts: map[string]int{}}
	for _, r := range rows {
		writeScorecardRow(&b, r.Campaign, r.Target, r, r.StopReason)
		for class, n := range r.Counts {
			total.Counts[class] += n
		}
		total.Verified += r.Verified
		total.Held += r.Held
	}
	writeScorecardRow(&b, "**total**", fmt.Sprintf("%d campaigns", len(rows)), total, "")
	return b.String()
}

func writeScorecardRow(b *strings.Builder, campaign, target string, r ScorecardRow, stop string) {
	cells := make([]string, 0, len(scorecardColumns))
	for _, class := range scorecardColumns {
		cells = append(cells, strconv.Itoa(r.Counts[class]))
	}
	fmt.Fprintf(b, "| %s | %s | %s | %d/%d | %s |\n", campaign, target, strings.Join(cells, " | "), r.Verified, r.Held, stop)
}
