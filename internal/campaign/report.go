package campaign

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func WriteReports(dir string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(RenderMarkdown(state)), 0o600)
}

func RenderMarkdown(state State) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Go optimization campaign `%s`\n\n", state.ID)
	fmt.Fprintf(&b, "**%s evidence** — %s/%s\n\n", strings.ToUpper(state.Environment.Authority), state.Environment.OS, state.Environment.Architecture)
	fmt.Fprintf(&b, "- Status: `%s`\n- Stop reason: %s\n- Repository: `%s`\n- Revision: `%s`\n- Go: `%s`\n- CPU: `%s`\n- Build flags: `%s`\n\n", state.Status, state.StopReason, state.Repository, state.Environment.Revision, state.Environment.GoVersion, state.Environment.CPU, strings.Join(state.Environment.BuildFlags, " "))
	fmt.Fprintf(&b, "## Repository inventory\n\nDiscovered %d packages and %d command entry points.\n\n", len(state.Inventory.Packages), len(state.Inventory.Commands))
	for _, command := range state.Inventory.Commands {
		fmt.Fprintf(&b, "- `%s`\n", command)
	}
	fmt.Fprintf(&b, "\n## Baseline workloads\n\n| Workload | Exit | Wall time | Evidence |\n|---|---:|---:|---|\n")
	for _, run := range state.Runs {
		fmt.Fprintf(&b, "| `%s` | %d | %s | `%s` |\n", run.WorkloadID, run.ExitCode, run.Duration, run.ID)
	}
	b.WriteString("\n## Candidate experiments\n\n")
	if len(state.CandidateRecords) == 0 {
		b.WriteString("No source candidate was attempted. Completing with no accepted candidate is a successful harness result.\n\n")
	} else {
		accepted := 0
		for i := range state.CandidateRecords {
			if state.CandidateRecords[i].Accepted {
				accepted++
			}
		}
		fmt.Fprintf(&b, "%d candidate(s) evaluated, %d accepted by policy.\n\n", len(state.CandidateRecords), accepted)
		for _, record := range state.CandidateRecords {
			fmt.Fprintf(&b, "### Attempt %d — `%s` — **%s**\n\n", record.Attempt, record.CandidateID, strings.ToUpper(string(record.Decision)))
			if record.Hypothesis != "" {
				fmt.Fprintf(&b, "- Hypothesis: %s\n", record.Hypothesis)
			}
			if record.PatchPath != "" {
				fmt.Fprintf(&b, "- Patch: `%s`%s\n", record.PatchPath, acceptedMarker(record.Accepted))
			}
			if record.Summary != "" {
				fmt.Fprintf(&b, "- Evidence: %s\n", record.Summary)
			}
			for _, reason := range record.Reasons {
				fmt.Fprintf(&b, "- Policy: %s\n", reason)
			}
			if len(record.Comparisons) > 0 {
				b.WriteString("\n| Comparison | Baseline | Candidate | Δ | Supported |\n|---|---:|---:|---:|---|\n")
				for _, c := range record.Comparisons {
					supported := "no"
					if c.StatisticallyFit {
						supported = "yes"
					}
					fmt.Fprintf(&b, "| `%s` | %.4g | %.4g | %+.2f%% | %s |\n", c.Name, c.Baseline, c.Candidate, c.DeltaPercent, supported)
				}
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("## Reproduction\n\n```sh\n")
	fmt.Fprintf(&b, "goharness optimize --repo %q --manifest %q\n", state.Repository, state.ManifestPath)
	b.WriteString("```\n")
	return b.String()
}

func acceptedMarker(accepted bool) string {
	if accepted {
		return " ✅ accepted"
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
		return State{}, err
	}
	defer store.Close()
	return store.Load()
}
