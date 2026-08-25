package abrepro

import (
	"context"
	"fmt"
	"testing"

	"example.com/gotorque/internal/domain"
	"example.com/gotorque/internal/runner"
)

func TestABRepro(t *testing.T) {
	dir := t.TempDir()
	artifacts, _ := runner.NewArtifactStore(dir + "/artifacts")
	r, _ := runner.New(runner.Options{Artifacts: artifacts, SandboxRoot: dir + "/sandboxes", LocalIsolation: true})
	base := "/tmp/goharness-runs/gojq-accept/builds/26d574e25a80d7a0d66b7f54-gojq"
	cand := "/tmp/goharness-runs/gojq-accept/builds/99869c0acb40352f2ecbc0a2-gojq"
	mk := func(id, bin string) runner.RunRequest {
		return runner.RunRequest{
			Build:    runner.Build{ID: id, BinaryPath: bin},
			Workload: domain.Workload{ID: "w", Name: "w", Command: domain.Command{Path: bin, Args: []string{".users[] | {name, active}"}}},
			Mode:     domain.RunModeMeasurement,
			Stdin:    []byte(`{"users":[{"name":"Ada","active":true},{"name":"Grace","active":false}]}`),
		}
	}
	ab, err := r.RunInterleaved(context.Background(), runner.ABRequest{Baseline: mk("base", base), Candidate: mk("cand", cand), Repetitions: 7})
	if err != nil {
		t.Fatal(err)
	}
	for i := range ab.Baseline {
		b := ab.Baseline[i].Metrics[0].Value
		c := ab.Candidate[i].Metrics[0].Value
		fmt.Printf("rep%d base=%.2fms cand=%.2fms\n", i, float64(b)/1e6, float64(c)/1e6)
	}
}
