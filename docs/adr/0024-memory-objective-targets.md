# 0024. A memory objective chooses memory-relevant targets

- Status: accepted
- Date: 2026-09-25

## Context

ADR 0020 let a campaign improve peak memory instead of wall time (`--tradeoff lean`), but left
its consequence unaddressed: "discovery still profiles CPU, so a `lean` campaign's targets come
from CPU-hot code and find memory wins only where that code also allocates." Discovery's hot list
comes from sampling the release binary on a workload, or, when that fails, a benchmark CPU
profile (`internal/campaign/engine.go` `sampleTargetProfile`, `benchmarkCPUProfile`); either way
it is CPU time, not bytes allocated. The Jev analyst then orders every flagged cause by
`z/sqrt(1+rank)` (ADR 0018, `internal/campaign/causes.go` `targets`), which favours strong
evidence near the top of a CPU profile regardless of what the cause is. A function that spends no
CPU time can allocate heavily and never appear in either list, and a lean campaign never gets a
chance to name it.

## Decision

Two changes, both gated on `state.Manifest.Performance.PrimaryMetric == "peak_memory_bytes"`
(the resolved objective after ADR 0020's trade-off is applied), and both no-ops for any other
objective:

1. **Target order.** `causeAnalyst.AnalyzeCauses` now reads the objective, carried on
   `orchestrator.CampaignRequest.Objective` (set once, in `Engine.campaignRequest`, from the
   manifest already resolved by the trade-off). `targets()` splits each `z/sqrt(1+rank)` tier in
   two under the memory objective: every cause `jev.Cause.IsAllocation` names (`alloc`,
   `prealloc`, `string_build`) sorts ahead of every other cause, the discounted order kept inside
   each half. A site flagged for both an allocation cause and, say, `fast_path` now offers the
   allocation cause first even when the fast path's z score is larger.

2. **Allocation evidence.** When the objective is memory and the module has benchmarks,
   `Engine.profileAllocations` runs them a second time under `-memprofile` (a new
   `toolchain.TestRequest.Memprofile` field, plumbed through `testArgs` exactly like
   `Cpuprofile`, with the same `-o` fix that keeps the compiled test binary out of the canonical
   checkout) and summarizes the heap profile's `alloc_space` sample index — cumulative bytes ever
   allocated, not bytes still live — via a new `Toolchain.PprofTopAllocSpace` /
   `Collector.SummarizePprofAllocSpace`. The resulting hot functions are merged to the front of
   `DiscoveryHotFunctions` (`mergeAllocFirst`), ahead of every CPU-only function, deduplicated and
   capped at the existing budget. A module with no benchmarks is skipped silently beyond a
   `discovery_alloc_profile_skipped` event: discovery still has its CPU evidence, unchanged from
   before this ADR.

Neither change touches `internal/policy` or injects anything into the target's binary; both are
deterministic code reading Jev's or pprof's output, never a model judgment.

## Evidence

`TestTargetsRankAllocationCausesFirstUnderMemoryObjective` replays a tier with one strong
non-allocation cause and two weaker allocation causes: under `wall_time_ns` the strong cause
leads, unchanged from ADR 0018; under `peak_memory_bytes` both allocation causes lead it, the
weaker-evidence one first, and the discounted order still holds within each half.
`TestCampaignRequestCarriesTheObjective` checks the objective reaches
`CauseRequest.Campaign.Objective` unchanged from the manifest's resolved primary metric.
`TestAllocationProfilingLeavesTheCheckoutClean` is `TestBenchmarkProfilingLeavesTheCheckoutClean`'s
memprofile analogue: `git status --porcelain` is empty after `profileAllocations` runs.
`TestMergeAllocFirstPrefersAllocatorsWithoutDroppingCPUEvidence` checks the merge is deduplicated,
order-preserving, and budget-capped. `TestTestPassesAbsoluteMemprofile` checks `-memprofile` is
emitted only for an absolute path, mirroring the existing cpuprofile test.

## Consequences

A `lean` campaign's report now carries a "Discovery profile" line naming which profile(s) chose
its targets (`State.DiscoveryProfileSource`), so "a target sample" versus "a target sample + a
benchmark alloc_space profile" is visible without reading the event log. Discovery now runs the
module's benchmarks up to twice under a memory objective (once for CPU, once for allocation) when
neither sampling nor an existing CPU benchmark profile already covers it; this only runs when the
objective is memory, so every other campaign's discovery cost is unchanged. The allocation
evidence still depends on the module having benchmarks that exercise the allocating code, exactly
as the existing CPU benchmark fallback does — a module with no benchmarks at all gets no memory
evidence beyond whatever a CPU-hot function happens to also allocate, which is where this effort
started.
