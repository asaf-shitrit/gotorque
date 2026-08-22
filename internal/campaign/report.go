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
	b.WriteString("\n## Candidate experiments\n\nNo source candidate was attempted. Completing with no accepted candidate is a successful harness result.\n\n")
	b.WriteString("## Reproduction\n\n```sh\n")
	fmt.Fprintf(&b, "goharness optimize --repo %q --manifest %q\n", state.Repository, state.ManifestPath)
	b.WriteString("```\n")
	return b.String()
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
