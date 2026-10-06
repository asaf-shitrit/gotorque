// Package discovery gathers the hot-path evidence a campaign's analysis starts
// from: which functions of the target's own code a representative run spends
// its time in. It samples the release binary under the platform sampler,
// falls back to the module's benchmarks when sampling cannot work, and keeps
// the benchmark profile the PGO lane needs.
//
// The package knows nothing of the campaign engine. Its inputs are plain data
// and its output is Evidence, which the engine applies to its persisted state.
package discovery
