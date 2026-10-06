// Package discovery gathers the hot-path evidence a campaign's analysis starts
// from: which functions of the target's own code a representative run spends
// its time in. It samples the release binary under the platform sampler,
// falls back to the module's benchmarks when sampling cannot work, and keeps
// the benchmark profile the PGO lane needs.
//
// The package knows nothing of the campaign engine. Its inputs are plain data
// and its output is Evidence, which the engine applies to its persisted state.
package discovery

import (
	"github.com/asaf-shitrit/gotorque/internal/manifest"
	"github.com/asaf-shitrit/gotorque/internal/runner"
)

// Inputs is everything discovery needs, as plain data. It holds no engine
// state, so a test builds one directly.
type Inputs struct {
	// BinaryPath is the release baseline binary that is sampled.
	BinaryPath string
	// Command is the target's command prefix, placed before each seed's args.
	Command []string
	// Seeds are the manifest's seed workloads, in order; the first is the
	// representative one discovery samples.
	Seeds []manifest.SeedWorkload
	// Sandbox is the campaign's sandbox policy for sampled runs.
	Sandbox runner.SandboxPolicy
	// Dir is the campaign directory; raw sampler reports are kept under
	// profile-sample/ in it.
	Dir string
}
