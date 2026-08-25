package abrepro

import (
	"context"
	"fmt"
	"testing"
	"time"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/runner"
)

func TestABReproCampaignContext(t *testing.T) {
	// Reuse the real campaign dir as sandbox root to match its state.
	root := "/tmp/goharness-runs/gojq-accept"
	artifacts, _ := runner.NewArtifactStore(root + "/artifacts")
	r, _ := runner.New(runner.Options{Artifacts: artifacts, SandboxRoot: root + "/sandboxes", LocalIsolation: true})
	base := "/tmp/goharness-runs/gojq-accept/builds/26d574e25a80d7a0d66b7f54-gojq"
	cand := "/tmp/goharness-runs/gojq-accept/builds/99869c0acb40352f2ecbc0a2-gojq"
	mk := func(id, bin string) runner.RunRequest {
		return runner.RunRequest{
			Build:         runner.Build{ID: id, BinaryPath: bin},
			Workload:      domain.Workload{ID: "w-sf", Name: "select-fields", Command: domain.Command{Path: bin, Args: []string{".users[] | {name, active}"}, }, Timeout: 30 * time.Second},
			Mode:          domain.RunModeMeasurement,
			Stdin:         []byte(`{"users":[{"name":"Ada","active":true},{"name":"Grace","active":false}]}`),
			AdditionalEnv: map[string]string{"GOTOOLCHAIN": "local"},
		}
	}
	// self-consistency probes like the campaign
	for i := 0; i < 2; i++ {
		if _, err := r.Run(context.Background(), mk("probe"+fmt.Sprint(i), base)); err != nil {
			t.Fatal(err)
		}
	}
	ab, err := r.RunInterleaved(context.Background(), runner.ABRequest{Baseline: mk("base", base), Candidate: mk("cand", cand), Repetitions: 7})
	if err != nil {
		t.Fatal(err)
	}
	for i := range ab.Baseline {
		b := ab.Baseline[i].Metrics[0].Value
		c := ab.Candidate[i].Metrics[0].Value
		fmt.Printf("rep%d base=%.2fms cand=%.2fms exit=%d/%d\n", i, float64(b)/1e6, float64(c)/1e6,
			ab.Baseline[i].ExitCode, ab.Candidate[i].ExitCode)
	}
}
