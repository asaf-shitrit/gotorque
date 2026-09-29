package campaign

import (
	"testing"

	"example.com/gotorque/internal/manifest"
	"github.com/stretchr/testify/require"
)

// TestMeasurementCarriesTheSeedsExpectedExitCode: every seed measurement --
// baseline, candidate, confirmation and the PGO lane -- is built by
// seedMeasurementRequest, so this is where a diff tool's exit 1 must reach
// the runner.
func TestMeasurementCarriesTheSeedsExpectedExitCode(t *testing.T) {
	e := &Engine{}
	req := e.seedMeasurementRequest(manifest.SeedWorkload{ID: "diff", ExitCode: 1}, "build", "/bin/true")
	require.Equal(t, 1, req.Workload.ExpectedExitCode)
	req = e.seedMeasurementRequest(manifest.SeedWorkload{ID: "plain"}, "build", "/bin/true")
	require.Equal(t, 0, req.Workload.ExpectedExitCode)
}
